// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Location management with the gateway as HLR (TS 29.002 §8.1, §19.1.1.5):
// the gateway is the MAP process in the HLR, the application on map.ul is
// the location management application process (TS 23.012) that decides.
//
// A location update lives on the node holding its dialogue, which asks the
// application at each decision point of figure 19.1.1/13, one turn at a
// time: after the VLR's updateLocation, and after each insertSubscriberData
// it sent. The answer — insert data, accept, reject — comes back to the
// asking node as the NATS reply. Each wait is bounded: the application's
// answer by map.hlr_answer_timeout, the VLR's answer to subscriber data by
// timer m (TS 29.002 §17.1.2: at most 30 s).

const (
	// luNetworkTimeout bounds the wait for the VLR's answer to
	// insertSubscriberData: timer m's upper limit.
	luNetworkTimeout = 30 * time.Second
	// luTCAPGrace keeps TCAP's own dialogue timer behind the location
	// update's, so the location update's timer, which tells the
	// application, decides.
	luTCAPGrace = 5 * time.Second
)

// The TCAP, MAP and NATS calls location updates make; consumer-side so
// tests can record them.
type luTCAP interface {
	Fits(msg *tcap.Message) (bool, error)
	SetDialogueTimeout(d *tcap.Dialogue, timeout time.Duration)
}

type luMAP interface {
	InsertSubscriberDataMessage(d *tcap.Dialogue, invokeID int32, param []byte) *tcap.Message
	Send(d *tcap.Dialogue, msg *tcap.Message) error
	UpdateLocationAccept(d *tcap.Dialogue, invokeID int32, hlrNumber mapprotocol.AddressString) error
	UpdateLocationRefuse(d *tcap.Dialogue, invokeID int32, code mapprotocol.ErrorCode) error
	LocationUpdateAbort(d *tcap.Dialogue) error
}

type luNATS interface {
	RequestLocationUpdate(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error)
	PublishLocationUpdateEnd(msg *natsbridge.NatsMessage) error
}

type locationUpdate struct {
	id       string
	d        *tcap.Dialogue
	imsi     string
	ulInvoke int32
	update   *ss7client.LocationUpdate // what the VLR sent, on every turn
	// gen counts the location update's steps; an answer or timer of an
	// earlier step finds it moved on and is dropped.
	gen uint64
	// Subscriber data being sent: the parts, the next to send, the invoke
	// outstanding and the results so far.
	parts       [][]byte
	next        int
	outstanding int32
	lastInvoke  int32
	result      mapprotocol.InsertSubscriberDataRes
	timer       *time.Timer
	// vlrError is the VLR's error to the data in the message that ended
	// the dialogue: the end turn reports it.
	vlrError *ss7client.Message
	// asking: a turn is with the application. An end meanwhile waits for
	// it (endAfterAsk), so that the application gets turns one at a time
	// and the end after its answer.
	asking      bool
	endAfterAsk *natsbridge.NatsMessage
}

type locationUpdates struct {
	tcap           luTCAP
	mapH           luMAP
	nats           luNATS
	hlrNumber      *mapprotocol.AddressString
	answerTimeout  time.Duration
	networkTimeout time.Duration // luNetworkTimeout; shorter in tests
	prefix         string
	logger         *zap.Logger

	ctx    context.Context // ends at close: the turns asked are given up
	cancel context.CancelFunc

	mu     sync.Mutex
	byTID  map[uint32]*locationUpdate
	closed bool
}

func newLocationUpdates(nodeID string, hlrNumber *mapprotocol.AddressString, cfg *mapprotocol.Config,
	t luTCAP, m luMAP, n luNATS, logger *zap.Logger) *locationUpdates {
	sum := sha256.Sum256([]byte(nodeID))
	ctx, cancel := context.WithCancel(context.Background())
	return &locationUpdates{
		tcap: t, mapH: m, nats: n, hlrNumber: hlrNumber,
		answerTimeout:  cmp.Or(cfg.HLRAnswerTimeout, mapprotocol.DefaultHLRAnswerTimeout),
		networkTimeout: luNetworkTimeout,
		prefix:         hex.EncodeToString(sum[:6]) + "-",
		logger:         logger,
		ctx:            ctx, cancel: cancel,
		byTID: make(map[uint32]*locationUpdate),
	}
}

// current reports whether lu is still live and at step gen.
func (m *locationUpdates) currentLocked(lu *locationUpdate, gen uint64) bool {
	return m.byTID[lu.d.LocalTID] == lu && lu.gen == gen
}

// removeLocked forgets lu: its dialogue has ended or is ending.
func (m *locationUpdates) removeLocked(lu *locationUpdate) {
	delete(m.byTID, lu.d.LocalTID)
	lu.gen++
	if lu.timer != nil {
		lu.timer.Stop()
	}
	lu.d.MarkAnswered()
}

// refuseLocked ends lu with updateLocation's error, as the application
// asked. It reports whether the error went out; when it did not, the
// application is told with an end turn.
func (m *locationUpdates) refuseLocked(lu *locationUpdate, code mapprotocol.ErrorCode, why string) bool {
	m.removeLocked(lu)
	m.logger.Info("location update refused", zap.String("dialogue_id", lu.id), zap.String("imsi", lu.imsi),
		zap.Stringer("error", code), zap.String("why", why))
	if err := m.mapH.UpdateLocationRefuse(lu.d, lu.ulInvoke, code); err != nil {
		m.logger.Warn("location update refusal not sent", zap.String("dialogue_id", lu.id), zap.Error(err))
		m.endLocked(lu, ss7client.StatusAborted, ss7client.ErrCodeAborted, "the refusal could not be sent: "+err.Error())
		return false
	}
	return true
}

// failLocked refuses lu with systemFailure on the gateway's own account —
// no answer, or one it cannot carry out — and tells the application with
// an end turn.
func (m *locationUpdates) failLocked(lu *locationUpdate, status string, code int32, why string) {
	if m.refuseLocked(lu, mapprotocol.ErrSystemFailure, why) {
		m.endLocked(lu, status, code, "refused with systemFailure: "+why)
	}
}

// endLocked ends lu without the application's answer carried out, and
// tells it so with its one end turn — after the turn it is answering, if
// any.
func (m *locationUpdates) endLocked(lu *locationUpdate, status string, code int32, detail string) {
	m.removeLocked(lu)
	msg := m.turn(lu, ss7client.ULEnd)
	if v := lu.vlrError; v != nil {
		status, code, detail = v.Status, v.ErrorCode, v.ErrorMessage+"; then "+detail
	}
	msg.Status, msg.ErrorCode, msg.ErrorMessage = status, code, detail
	msg.LocationUpdate.Detail = detail
	msg.LocationUpdate.AnswerBy = nil
	m.logger.Debug("location update ended", zap.String("dialogue_id", lu.id), zap.String("status", status), zap.String("detail", detail))
	if lu.asking {
		lu.endAfterAsk = msg
		return
	}
	m.publishEnd(lu, msg)
}

func (m *locationUpdates) publishEnd(lu *locationUpdate, msg *natsbridge.NatsMessage) {
	if err := m.nats.PublishLocationUpdateEnd(msg); err != nil {
		m.logger.Error("location update end not published", zap.String("dialogue_id", lu.id), zap.Error(err))
	}
}

// abortLocked gives lu's dialogue up and tells the application.
func (m *locationUpdates) abortLocked(lu *locationUpdate, status string, code int32, detail string) {
	if err := m.mapH.LocationUpdateAbort(lu.d); err != nil {
		m.logger.Warn("location update abort not sent", zap.String("dialogue_id", lu.id), zap.Error(err))
	}
	m.endLocked(lu, status, code, detail)
}

func (m *locationUpdates) turn(lu *locationUpdate, action string) *natsbridge.NatsMessage {
	u := *lu.update
	u.DialogueID, u.Action = lu.id, action
	return &natsbridge.NatsMessage{CorrelationID: lu.id, IMSI: lu.imsi, LocationUpdate: &u}
}

// askLocked asks the application about lu's next step, off the TCAP worker.
func (m *locationUpdates) askLocked(lu *locationUpdate, msg *natsbridge.NatsMessage) {
	lu.gen++
	gen := lu.gen
	by := time.Now().Add(m.answerTimeout)
	msg.LocationUpdate.AnswerBy = &by
	m.tcap.SetDialogueTimeout(lu.d, m.answerTimeout+luTCAPGrace)
	lu.asking = true
	go func() {
		ctx, cancel := context.WithDeadline(m.ctx, by)
		defer cancel()
		answer, err := m.nats.RequestLocationUpdate(ctx, msg)
		m.mu.Lock()
		defer m.mu.Unlock()
		lu.asking = false
		if end := lu.endAfterAsk; end != nil {
			// The location update ended while the application decided:
			// its answer, if any, is not carried out, and the end follows it.
			lu.endAfterAsk = nil
			m.publishEnd(lu, end)
			return
		}
		if !m.currentLocked(lu, gen) {
			m.logger.Debug("location update answer for a step past, dropped", zap.String("dialogue_id", lu.id))
			return
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			m.failLocked(lu, ss7client.StatusTimeout, ss7client.ErrCodeTimeout,
				"no answer from the HLR application within map.hlr_answer_timeout")
		case err != nil:
			m.failLocked(lu, ss7client.StatusRejected, ss7client.ErrCodeRejected, "no answer from the HLR application: "+err.Error())
		default:
			m.answerLocked(lu, answer)
		}
	}()
}

// answerLocked carries out the application's answer.
func (m *locationUpdates) answerLocked(lu *locationUpdate, answer *natsbridge.NatsMessage) {
	u := answer.LocationUpdate
	reject := func(why string) { m.failLocked(lu, ss7client.StatusRejected, ss7client.ErrCodeRejected, why) }
	if u == nil {
		reject("the HLR application's answer has no location_update")
		return
	}
	switch u.Action {
	case ss7client.ULAccept:
		m.removeLocked(lu)
		if err := m.mapH.UpdateLocationAccept(lu.d, lu.ulInvoke, *m.hlrNumber); err != nil {
			m.logger.Error("location update result not sent", zap.String("dialogue_id", lu.id), zap.Error(err))
			m.endLocked(lu, ss7client.StatusAborted, ss7client.ErrCodeAborted, "the result could not be sent: "+err.Error())
			return
		}
		m.logger.Debug("location update accepted", zap.String("dialogue_id", lu.id), zap.String("imsi", lu.imsi))
	case ss7client.ULReject:
		code := mapprotocol.ErrorCode(u.Error)
		if !mapprotocol.UpdateLocationErrors[code] {
			reject(fmt.Sprintf("the HLR application's error %d is not one updateLocation allows", u.Error))
			return
		}
		why := "the HLR application refused it"
		if u.Detail != "" {
			why += ": " + u.Detail
		}
		m.refuseLocked(lu, code, why)
	case ss7client.ULInsert:
		m.insertLocked(lu, answer.SubscriberData)
	default:
		reject(fmt.Sprintf("the HLR application answered %q", u.Action))
	}
}

// insertLocked sends subscriber data: in parts if it does not fit one
// message. What cannot be sent goes back to the application as a failed
// insert, for it to decide.
func (m *locationUpdates) insertLocked(lu *locationUpdate, data *ss7client.SubscriberData) {
	mapData, err := subscriberDataMAP(data)
	var parts [][]byte
	if err == nil {
		parts, err = mapprotocol.SplitSubscriberData(mapData, func(param []byte) (bool, error) {
			return m.tcap.Fits(m.mapH.InsertSubscriberDataMessage(lu.d, 1, param))
		})
	}
	if err != nil {
		msg := m.turn(lu, ss7client.ULInsertResult)
		msg.Status, msg.ErrorCode = ss7client.StatusRejected, ss7client.ErrCodeRejected
		msg.ErrorMessage = "subscriber data not sent: " + err.Error()
		msg.LocationUpdate.Detail = msg.ErrorMessage
		m.askLocked(lu, msg)
		return
	}
	lu.parts, lu.next, lu.result = parts, 0, mapprotocol.InsertSubscriberDataRes{}
	m.sendPartLocked(lu)
}

// sendPartLocked sends the next part of the subscriber data and waits for
// the VLR's answer.
func (m *locationUpdates) sendPartLocked(lu *locationUpdate) {
	id := lu.lastInvoke
	if id++; id > 127 {
		id = 1
	}
	lu.lastInvoke = id
	if err := m.mapH.Send(lu.d, m.mapH.InsertSubscriberDataMessage(lu.d, id, lu.parts[lu.next])); err != nil {
		m.abortLocked(lu, ss7client.StatusAborted, ss7client.ErrCodeAborted, "subscriber data not sent: "+err.Error())
		return
	}
	lu.next++
	lu.outstanding = id
	lu.gen++
	gen := lu.gen
	if lu.timer != nil {
		lu.timer.Stop()
	}
	lu.timer = time.AfterFunc(m.networkTimeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.currentLocked(lu, gen) {
			m.abortLocked(lu, ss7client.StatusTimeout, ss7client.ErrCodeTimeout,
				"no answer from the VLR to the subscriber data within timer m")
		}
	})
	m.tcap.SetDialogueTimeout(lu.d, m.networkTimeout+luTCAPGrace)
}

// --- mapprotocol.LocationUpdateHandler (TCAP workers) ----------------------

// UpdateLocation opens a location update and asks the application.
func (m *locationUpdates) UpdateLocation(d *tcap.Dialogue, invokeID int32, arg *mapprotocol.UpdateLocationArg) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byTID[d.LocalTID] != nil {
		return false
	}
	var calling string
	if a := d.RemoteAddress; a != nil && a.GlobalTitle != nil {
		calling = a.GlobalTitle.Address
	}
	lu := &locationUpdate{id: m.prefix + rand.Text(), d: d, imsi: string(arg.IMSI), ulInvoke: invokeID,
		update: updateLocationWire(arg, calling)}
	m.byTID[d.LocalTID] = lu
	switch {
	case m.closed:
		m.failLocked(lu, ss7client.StatusRejected, ss7client.ErrCodeRejected, "the node is shutting down")
	case m.hlrNumber == nil:
		m.failLocked(lu, ss7client.StatusRejected, ss7client.ErrCodeRejected, "no HLR number (set map.hlr_number or an sccp global title)")
	default:
		m.logger.Debug("location update", zap.String("dialogue_id", lu.id), zap.String("imsi", lu.imsi),
			zap.String("vlr", arg.VLRNumber.Digits), zap.String("calling_gt", calling))
		m.askLocked(lu, m.turn(lu, ss7client.ULUpdateLocation))
	}
	return true
}

// InsertSubscriberDataResult: the VLR took a part of the data. A result in
// the END that closes the dialogue asks nothing more: the end follows.
func (m *locationUpdates) InsertSubscriberDataResult(d *tcap.Dialogue, invokeID int32, res *mapprotocol.InsertSubscriberDataRes, ended bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lu := m.byTID[d.LocalTID]
	if lu == nil || lu.d != d || lu.outstanding != invokeID || lu.parts == nil {
		m.logger.Warn("insertSubscriberData result for nothing outstanding, dropped",
			zap.Uint32("tid", d.LocalTID), zap.Int32("invoke_id", invokeID))
		return
	}
	lu.outstanding = 0
	mergeInsertResults(&lu.result, res)
	if ended {
		lu.parts = nil
		lu.gen++ // a timer of this step finds it moved on
		if lu.timer != nil {
			lu.timer.Stop()
		}
		return
	}
	if lu.next < len(lu.parts) {
		m.sendPartLocked(lu)
		return
	}
	lu.parts = nil
	if lu.timer != nil {
		lu.timer.Stop()
	}
	msg := m.turn(lu, ss7client.ULInsertResult)
	msg.InsertResult = insertResultWire(&lu.result)
	m.askLocked(lu, msg)
}

// InsertSubscriberDataError: the VLR refused the data. The rest of it is
// not sent; the application decides, unless the VLR ended the dialogue.
func (m *locationUpdates) InsertSubscriberDataError(d *tcap.Dialogue, invokeID int32, code mapprotocol.ErrorCode, detail string, ended bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lu := m.byTID[d.LocalTID]
	if lu == nil || lu.d != d || lu.outstanding != invokeID || lu.parts == nil {
		m.logger.Warn("insertSubscriberData error for nothing outstanding, dropped",
			zap.Uint32("tid", d.LocalTID), zap.Int32("invoke_id", invokeID), zap.String("detail", detail))
		return
	}
	lu.outstanding, lu.parts = 0, nil
	if lu.timer != nil {
		lu.timer.Stop()
	}
	status, errCode := remoteErrorWire(code)
	msg := m.turn(lu, ss7client.ULInsertResult)
	msg.Status, msg.ErrorCode, msg.ErrorMessage = status, errCode, "the VLR refused the subscriber data: "+detail
	msg.LocationUpdate.Detail = msg.ErrorMessage
	if ended {
		lu.vlrError = msg
		lu.gen++ // a timer of this step finds it moved on
		return
	}
	m.askLocked(lu, msg)
}

// LocationUpdateEnded: the VLR ended the dialogue before the answer.
func (m *locationUpdates) LocationUpdateEnded(d *tcap.Dialogue, aborted bool, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lu := m.byTID[d.LocalTID]; lu != nil && lu.d == d {
		if !aborted {
			detail += " before the location update was answered"
		}
		m.endLocked(lu, ss7client.StatusAborted, ss7client.ErrCodeAborted, detail)
	}
}

// LocationUpdateActive reports whether d has a location update.
func (m *locationUpdates) LocationUpdateActive(d *tcap.Dialogue) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lu := m.byTID[d.LocalTID]
	return lu != nil && lu.d == d
}

// lost is TCAP's lost sink's share: TCAP ended the dialogue on its own.
func (m *locationUpdates) lost(d *tcap.Dialogue, reason tcap.FailureReason, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lu := m.byTID[d.LocalTID]; lu != nil && lu.d == d {
		m.endLocked(lu, reason.String(), failureErrorCode(reason), detail)
	}
}

// close aborts the node's location updates at shutdown, telling the
// application of each, and gives up the turns asked.
func (m *locationUpdates) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, lu := range m.byTID {
		m.abortLocked(lu, ss7client.StatusAborted, ss7client.ErrCodeAborted, "the gateway node is shutting down")
	}
	m.cancel()
}

// --- requests to a VLR -----------------------------------------------------

// hlrNumber is the HLR number of updateLocation's result: map.hlr_number,
// else the cluster GT (the same whichever node answers), else this node's.
func (b *natsBridge) hlrNumber() *mapprotocol.AddressString {
	for _, digits := range []string{b.mapCfg.HLRNumber, b.sccpCfg.ClusterGlobalTitle.Address, b.sccpCfg.LocalGlobalTitle.Address} {
		if digits != "" {
			return &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: digits}
		}
	}
	return nil
}

// vlrRequest checks what CancelLocation, ProvideRoamingNumber and
// InsertSubscriberData share: the IMSI, and the VLR they go to — its number
// as GT with SSN 7, or msg.DPC — answering to SSN 6.
func (b *natsBridge) vlrRequest(msg *natsbridge.NatsMessage, what string) (mapprotocol.IMSI, *sccp.SccpAddress, error) {
	if !isDigits(msg.IMSI) || len(msg.IMSI) < 5 || len(msg.IMSI) > 15 {
		return "", nil, fmt.Errorf("%s: imsi %q is not 5..15 digits", what, msg.IMSI)
	}
	vlr := paramAddressString(msg, ss7client.ParamVLRNumber)
	if vlr == nil {
		return "", nil, fmt.Errorf("%s: vlr_number required in parameters", what)
	}
	if !isDigits(vlr.Digits) || len(vlr.Digits) > maxGTDigits {
		return "", nil, fmt.Errorf("%s: vlr_number %q is not E.164 digits (at most %d)", what, vlr.Digits, maxGTDigits)
	}
	if err := b.requireLocalSSN(sccp.SSNHLR); err != nil {
		return "", nil, fmt.Errorf("%s: %w", what, err)
	}
	return mapprotocol.IMSI(msg.IMSI), b.calledForGT(vlr.Digits, sccp.SSNVLR, msg.DPC), nil
}

func optionalHex(what, s string, n int) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return hexOctets(what, s, n, n)
}

// onCLRequest originates cancelLocation (op 3) under
// locationCancellationContext-v3.
func (b *natsBridge) onCLRequest(msg *natsbridge.NatsMessage) error {
	imsi, called, err := b.vlrRequest(msg, "CancelLocation")
	if err != nil {
		return err
	}
	arg := &mapprotocol.CancelLocationArg{IMSI: imsi}
	if c := msg.CancelLocation; c != nil {
		if arg.LMSI, err = optionalHex("CancelLocation: lmsi", c.LMSI, 4); err != nil {
			return err
		}
		switch c.Type {
		case "":
		case ss7client.CancellationUpdateProcedure, ss7client.CancellationSubscriptionWithdraw:
			t := mapprotocol.CancellationUpdateProcedure
			if c.Type == ss7client.CancellationSubscriptionWithdraw {
				t = mapprotocol.CancellationSubscriptionWithdraw
			}
			arg.CancellationType = &t
		default:
			return fmt.Errorf("CancelLocation: type %q is not %q or %q", c.Type,
				ss7client.CancellationUpdateProcedure, ss7client.CancellationSubscriptionWithdraw)
		}
	}
	param, err := mapprotocol.EncodeCancelLocationArg(arg)
	if err != nil {
		return err
	}
	return b.originateToVLR(&tcap.ACLocationCancellationV3, called, mapprotocol.OpCancelLocation, param, msg, "CancelLocation")
}

// onPRNRequest originates provideRoamingNumber (op 4) under
// roamingNumberEnquiryContext-v3.
func (b *natsBridge) onPRNRequest(msg *natsbridge.NatsMessage) error {
	imsi, called, err := b.vlrRequest(msg, "ProvideRoamingNumber")
	if err != nil {
		return err
	}
	e := msg.RoamingNumberEnquiry
	if e == nil || e.MSCNumber == nil {
		return errors.New("ProvideRoamingNumber: roaming_number_enquiry.msc_number required")
	}
	arg := &mapprotocol.ProvideRoamingNumberArg{IMSI: imsi, ORInterrogation: e.ORInterrogation,
		SuppressionOfAnnouncement: e.SuppressionOfAnnouncement}
	msc, err := addressMAP("msc_number", e.MSCNumber)
	if err != nil {
		return fmt.Errorf("ProvideRoamingNumber: %w", err)
	}
	arg.MSCNumber = *msc
	if arg.MSISDN, err = addressMAP("msisdn", e.MSISDN); err != nil {
		return fmt.Errorf("ProvideRoamingNumber: %w", err)
	}
	if arg.GMSCAddress, err = addressMAP("gmsc_address", e.GMSCAddress); err != nil {
		return fmt.Errorf("ProvideRoamingNumber: %w", err)
	}
	if arg.LMSI, err = optionalHex("ProvideRoamingNumber: lmsi", e.LMSI, 4); err != nil {
		return err
	}
	if e.CallReferenceNumber != "" {
		if arg.CallReferenceNumber, err = hexOctets("ProvideRoamingNumber: call_reference_number", e.CallReferenceNumber, 1, 8); err != nil {
			return err
		}
	}
	if bc := e.BearerCapability; bc != nil {
		data, err := hexOctets("ProvideRoamingNumber: bearer_capability.data", bc.Data, 1, 200)
		if err != nil {
			return err
		}
		arg.GSMBearerCapability = &mapprotocol.ExternalSignalInfo{ProtocolID: bc.ProtocolID, SignalInfo: data}
	}
	param, err := mapprotocol.EncodeProvideRoamingNumberArg(arg)
	if err != nil {
		return err
	}
	return b.originateToVLR(&tcap.ACRoamingNumberEnquiryV3, called, mapprotocol.OpProvideRoamingNumber, param, msg, "ProvideRoamingNumber")
}

// onISDRequest originates a stand-alone insertSubscriberData (op 7) under
// subscriberDataMngtContext-v3. It must fit one message; TCAP refuses one
// that does not fit even a CONTINUE after an empty Begin.
func (b *natsBridge) onISDRequest(msg *natsbridge.NatsMessage) error {
	imsi, called, err := b.vlrRequest(msg, "InsertSubscriberData")
	if err != nil {
		return err
	}
	data, err := subscriberDataMAP(msg.SubscriberData)
	if err != nil {
		return fmt.Errorf("InsertSubscriberData: %w", err)
	}
	param, err := mapprotocol.EncodeInsertSubscriberDataArg(&mapprotocol.InsertSubscriberDataArg{IMSI: imsi, Data: *data})
	if err != nil {
		return err
	}
	return b.originateToVLR(&tcap.ACSubscriberDataMngtV3, called, mapprotocol.OpInsertSubscriberData, param, msg, "InsertSubscriberData")
}

func (b *natsBridge) originateToVLR(ac *tcap.ApplicationContext, called *sccp.SccpAddress, op mapprotocol.OperationCode,
	param []byte, msg *natsbridge.NatsMessage, what string) error {
	if _, err := b.tcapHandler.Originate(ac, called, b.callingSCCP(sccp.SSNHLR), int32(op), param, correlationID(msg)); err != nil {
		return fmt.Errorf("%s originate: %w", what, err)
	}
	b.logger.Debug(what+" originated", zap.String("imsi", msg.IMSI), zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// publishMobilityResponse publishes the answer of a request to a VLR.
func (b *natsBridge) publishMobilityResponse(corrID string, resp any) error {
	msg := &natsbridge.NatsMessage{CorrelationID: corrID}
	switch r := resp.(type) {
	case *mapprotocol.CancelLocationRes:
		msg.MessageType = natsbridge.MsgTypeMAPCL
	case *mapprotocol.ProvideRoamingNumberRes:
		msg.MessageType = natsbridge.MsgTypeMAPPRN
		msg.RoamingNumberEnquiry = &ss7client.RoamingNumberEnquiry{
			RoamingNumber: addressWire(&r.RoamingNumber), VMSCAddress: addressWire(r.VMSCAddress),
			ReleaseResourcesSupported: r.ReleaseResourcesSupported,
		}
	case *mapprotocol.InsertSubscriberDataRes:
		msg.MessageType = natsbridge.MsgTypeMAPISD
		msg.InsertResult = insertResultWire(r)
	default:
		return fmt.Errorf("no mobility answer for %T", resp)
	}
	return b.natsHandler.PublishAnswer(msg)
}
