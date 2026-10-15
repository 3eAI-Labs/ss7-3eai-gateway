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
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Authentication and MS purging with the gateway as HLR (TS 29.002 §8.5.2,
// §8.1.6): a VLR or SGSN opens a dialogue with one request, the node
// holding it asks the application on map.sai or map.purge as a NATS
// request, and answers with what the application said. The application's
// answer is bounded by map.hlr_answer_timeout.
//
// Vectors that do not fit one message go in several results (TS 29.002
// Annex C.3.3): each a ReturnResultLast, all but the last in a CONTINUE;
// the VLR asks for the next with another sendAuthenticationInfo, within
// timer m. A request with segmentationProhibited gets what fits one END.
//
// The vectors are key material: they are never logged.

// The MAP calls these dialogues make; consumer-side so tests can record
// them.
type hrMAP interface {
	AuthenticationInfoMessage(d *tcap.Dialogue, invokeID int32, param []byte, more bool) *tcap.Message
	Send(d *tcap.Dialogue, msg *tcap.Message) error
	HLRRequestRefuse(d *tcap.Dialogue, invokeID int32, e mapprotocol.UserError) error
	PurgeMSAccept(d *tcap.Dialogue, invokeID int32, res *mapprotocol.PurgeMSRes) error
	Close(d *tcap.Dialogue) error
	Abort(d *tcap.Dialogue) error
}

type hrNATS interface {
	RequestAuthentication(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error)
	RequestPurge(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error)
}

type hlrRequest struct {
	id       string
	d        *tcap.Dialogue
	imsi     string
	invokeID int32 // of the request being answered
	// auth: a sendAuthenticationInfo; nil for a purgeMS.
	auth *mapprotocol.SendAuthenticationInfoArg
	// pending: the vectors not sent yet, while the VLR is to ask for them
	// (awaitingNext).
	pending      *mapprotocol.SendAuthenticationInfoRes
	awaitingNext bool
	// gen counts the request's steps; an answer or timer of an earlier
	// step finds it moved on and is dropped.
	gen   uint64
	timer *time.Timer
}

func (r *hlrRequest) what() string {
	if r.auth != nil {
		return "sendAuthenticationInfo"
	}
	return "purgeMS"
}

type hlrRequests struct {
	tcap           luTCAP
	mapH           hrMAP
	nats           hrNATS
	answerTimeout  time.Duration
	networkTimeout time.Duration // luNetworkTimeout; shorter in tests
	prefix         string
	logger         *zap.Logger

	ctx    context.Context // ends at close: the requests asked are given up
	cancel context.CancelFunc

	mu     sync.Mutex
	byTID  map[uint32]*hlrRequest
	closed bool
}

func newHLRRequests(nodeID string, cfg *mapprotocol.Config, t luTCAP, m hrMAP, n hrNATS, logger *zap.Logger) *hlrRequests {
	sum := sha256.Sum256([]byte(nodeID))
	ctx, cancel := context.WithCancel(context.Background())
	return &hlrRequests{
		tcap: t, mapH: m, nats: n,
		answerTimeout:  cmp.Or(cfg.HLRAnswerTimeout, mapprotocol.DefaultHLRAnswerTimeout),
		networkTimeout: luNetworkTimeout,
		prefix:         hex.EncodeToString(sum[:6]) + "-",
		logger:         logger,
		ctx:            ctx, cancel: cancel,
		byTID: make(map[uint32]*hlrRequest),
	}
}

func (m *hlrRequests) currentLocked(r *hlrRequest, gen uint64) bool {
	return m.byTID[r.d.LocalTID] == r && r.gen == gen
}

// removeLocked forgets r: its dialogue has ended or is ending.
func (m *hlrRequests) removeLocked(r *hlrRequest) {
	delete(m.byTID, r.d.LocalTID)
	r.gen++
	if r.timer != nil {
		r.timer.Stop()
	}
	r.d.MarkAnswered()
}

// refuseLocked ends r with its operation's error.
func (m *hlrRequests) refuseLocked(r *hlrRequest, e mapprotocol.UserError, why string) {
	m.removeLocked(r)
	m.logger.Info(r.what()+" refused", zap.String("dialogue_id", r.id), zap.String("imsi", r.imsi),
		zap.Stringer("error", e.Code), zap.Stringer("cause", e.Cause), zap.String("why", why))
	if err := m.mapH.HLRRequestRefuse(r.d, r.invokeID, e); err != nil {
		m.logger.Warn(r.what()+" refusal not sent", zap.String("dialogue_id", r.id), zap.Error(err))
	}
}

// failLocked ends r on the gateway's own account: sendAuthenticationInfo
// with systemFailure; purgeMS, which has no such error, with an abort.
func (m *hlrRequests) failLocked(r *hlrRequest, why string) {
	if r.auth != nil {
		m.refuseLocked(r, mapprotocol.UserError{Code: mapprotocol.ErrSystemFailure}, why)
		return
	}
	m.abortLocked(r, why)
}

func (m *hlrRequests) abortLocked(r *hlrRequest, why string) {
	m.removeLocked(r)
	m.logger.Info(r.what()+" aborted", zap.String("dialogue_id", r.id), zap.String("imsi", r.imsi), zap.String("why", why))
	if err := m.mapH.Abort(r.d); err != nil {
		m.logger.Warn(r.what()+" abort not sent", zap.String("dialogue_id", r.id), zap.Error(err))
	}
}

// askLocked asks the application about r, off the TCAP worker.
func (m *hlrRequests) askLocked(r *hlrRequest, msg *natsbridge.NatsMessage) {
	r.gen++
	gen := r.gen
	by := time.Now().Add(m.answerTimeout)
	request := m.nats.RequestPurge
	if r.auth != nil {
		msg.Authentication.AnswerBy = &by
		request = m.nats.RequestAuthentication
	} else {
		msg.Purge.AnswerBy = &by
	}
	m.tcap.SetDialogueTimeout(r.d, m.answerTimeout+luTCAPGrace)
	go func() {
		ctx, cancel := context.WithDeadline(m.ctx, by)
		defer cancel()
		answer, err := request(ctx, msg)
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.currentLocked(r, gen) {
			m.logger.Debug(r.what()+" answer for a request no longer open, dropped", zap.String("dialogue_id", r.id))
			return
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			m.failLocked(r, "no answer from the HLR application within map.hlr_answer_timeout")
		case err != nil:
			m.failLocked(r, "no answer from the HLR application: "+err.Error())
		case r.auth != nil:
			m.answerAuthenticationLocked(r, answer.Authentication)
		default:
			m.answerPurgeLocked(r, answer.Purge)
		}
	}()
}

// answerAuthenticationLocked carries out the application's answer to a
// sendAuthenticationInfo.
func (m *hlrRequests) answerAuthenticationLocked(r *hlrRequest, a *ss7client.Authentication) {
	if a == nil {
		m.failLocked(r, "the HLR application's answer has no authentication")
		return
	}
	if a.Error != 0 || a.ErrorCause != "" {
		e, err := userError(a.Error, a.ErrorCause)
		if err == nil && !mapprotocol.SendAuthenticationInfoErrors[e.Code] {
			err = fmt.Errorf("error %d is not one sendAuthenticationInfo allows", a.Error)
		}
		if err != nil {
			m.failLocked(r, "the HLR application's "+err.Error())
			return
		}
		why := "the HLR application refused it"
		if a.Detail != "" {
			why += ": " + a.Detail
		}
		m.refuseLocked(r, e, why)
		return
	}
	res, err := authenticationResMAP(a)
	if err == nil {
		err = res.Validate(r.auth)
	}
	if err != nil {
		m.failLocked(r, "the HLR application's vectors cannot be sent: "+err.Error())
		return
	}
	r.pending = res
	m.sendNextLocked(r)
}

// sendNextLocked sends as many of the pending vectors as fit one message:
// the last part in an END, a part with more to come in a CONTINUE, after
// which the VLR asks for the next within timer m.
func (m *hlrRequests) sendNextLocked(r *hlrRequest) {
	param, rest, err := m.nextPart(r)
	if err != nil {
		m.failLocked(r, err.Error())
		return
	}
	more := rest != nil && !r.auth.SegmentationProhibited
	if rest != nil && !more {
		m.logger.Info("sendAuthenticationInfo: segmentation prohibited, vectors that do not fit one message dropped",
			zap.String("dialogue_id", r.id), zap.Int("dropped", vectorCount(rest)))
	}
	msg := m.mapH.AuthenticationInfoMessage(r.d, r.invokeID, param, more)
	if !more {
		m.removeLocked(r)
		if err := m.mapH.Send(r.d, msg); err != nil {
			m.logger.Warn("sendAuthenticationInfo result not sent", zap.String("dialogue_id", r.id), zap.Error(err))
		}
		return
	}
	if err := m.mapH.Send(r.d, msg); err != nil {
		m.abortLocked(r, "sendAuthenticationInfo result not sent: "+err.Error())
		return
	}
	r.pending, r.awaitingNext = rest, true
	r.gen++
	gen := r.gen
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(m.networkTimeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.currentLocked(r, gen) {
			m.abortLocked(r, "the VLR did not ask for the rest of the vectors within timer m")
		}
	})
	m.tcap.SetDialogueTimeout(r.d, m.networkTimeout+luTCAPGrace)
}

// nextPart takes from r.pending the longest prefix of its vectors — the
// authentication set list's, then the EPS vectors — that fits one message,
// the UE usage type with the first part. rest is nil when nothing is left.
func (m *hlrRequests) nextPart(r *hlrRequest) (param []byte, rest *mapprotocol.SendAuthenticationInfoRes, err error) {
	p := r.pending
	n := vectorCount(p)
	fits := func(part *mapprotocol.SendAuthenticationInfoRes) ([]byte, bool, error) {
		b, err := mapprotocol.EncodeSendAuthenticationInfoRes(part)
		if err != nil {
			return nil, false, err
		}
		// Asked as a CONTINUE, which is longer than an END.
		ok, err := m.tcap.Fits(m.mapH.AuthenticationInfoMessage(r.d, r.invokeID, b, true))
		return b, ok, err
	}
	if n == 0 {
		b, ok, err := fits(p)
		if err == nil && !ok {
			err = errors.New("the empty result does not fit one message")
		}
		return b, nil, err
	}
	for k := 1; k <= n; k++ {
		b, ok, err := fits(vectorRange(p, 0, k, true))
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			if k == 1 {
				return nil, nil, errors.New("one vector does not fit one message")
			}
			break
		}
		param = b
		if k == n {
			return param, nil, nil
		}
		rest = vectorRange(p, k, n, false)
	}
	return param, rest, nil
}

// vectorCount is the number of vectors in r.
func vectorCount(r *mapprotocol.SendAuthenticationInfoRes) int {
	return len(r.Triplets) + len(r.Quintuplets) + len(r.EPSVectors)
}

// vectorRange is r with its vectors [i, j) — the authentication set list's
// first — and its UE usage type when first.
func vectorRange(r *mapprotocol.SendAuthenticationInfoRes, i, j int, first bool) *mapprotocol.SendAuthenticationInfoRes {
	out := &mapprotocol.SendAuthenticationInfoRes{}
	if first {
		out.UEUsageType = r.UEUsageType
	}
	set := len(r.Triplets) + len(r.Quintuplets)
	for k := i; k < j; k++ {
		switch {
		case k < len(r.Triplets):
			out.Triplets = append(out.Triplets, r.Triplets[k])
		case k < set:
			out.Quintuplets = append(out.Quintuplets, r.Quintuplets[k-len(r.Triplets)])
		default:
			out.EPSVectors = append(out.EPSVectors, r.EPSVectors[k-set])
		}
	}
	return out
}

// answerPurgeLocked carries out the application's answer to a purgeMS.
func (m *hlrRequests) answerPurgeLocked(r *hlrRequest, p *ss7client.Purge) {
	switch {
	case p == nil:
		m.failLocked(r, "the HLR application's answer has no purge")
	case p.Error != 0 || p.ErrorCause != "":
		e, err := userError(p.Error, p.ErrorCause)
		if err == nil && !mapprotocol.PurgeMSErrors[e.Code] {
			err = fmt.Errorf("error %d is not one purgeMS allows", p.Error)
		}
		if err != nil {
			m.failLocked(r, "the HLR application's "+err.Error())
			return
		}
		why := "the HLR application refused it"
		if p.Detail != "" {
			why += ": " + p.Detail
		}
		m.refuseLocked(r, e, why)
	default:
		m.removeLocked(r)
		res := &mapprotocol.PurgeMSRes{FreezeTMSI: p.FreezeTMSI, FreezePTMSI: p.FreezePTMSI, FreezeMTMSI: p.FreezeMTMSI}
		if err := m.mapH.PurgeMSAccept(r.d, r.invokeID, res); err != nil {
			m.logger.Warn("purgeMS result not sent", zap.String("dialogue_id", r.id), zap.Error(err))
			return
		}
		m.logger.Debug("purgeMS answered", zap.String("dialogue_id", r.id), zap.String("imsi", r.imsi))
	}
}

// open registers a request for d, or reports false when d has one.
func (m *hlrRequests) openLocked(d *tcap.Dialogue, invokeID int32, imsi string) (*hlrRequest, string, bool) {
	if m.byTID[d.LocalTID] != nil {
		return nil, "", false
	}
	var calling string
	if a := d.RemoteAddress; a != nil && a.GlobalTitle != nil {
		calling = a.GlobalTitle.Address
	}
	r := &hlrRequest{id: m.prefix + rand.Text(), d: d, imsi: imsi, invokeID: invokeID}
	m.byTID[d.LocalTID] = r
	return r, calling, true
}

// --- mapprotocol.HLRRequestHandler (TCAP workers) --------------------------

// SendAuthenticationInfo asks the application for the vectors.
func (m *hlrRequests) SendAuthenticationInfo(d *tcap.Dialogue, invokeID int32, arg *mapprotocol.SendAuthenticationInfoArg) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, calling, ok := m.openLocked(d, invokeID, string(arg.IMSI))
	if !ok {
		return false
	}
	r.auth = arg
	if m.closed {
		m.failLocked(r, "the node is shutting down")
		return true
	}
	m.logger.Debug("sendAuthenticationInfo", zap.String("dialogue_id", r.id), zap.String("imsi", r.imsi),
		zap.Int("vectors", arg.NumberOfRequestedVectors), zap.Bool("resync", arg.ResyncRAND != nil), zap.String("calling_gt", calling))
	m.askLocked(r, &natsbridge.NatsMessage{CorrelationID: r.id, IMSI: r.imsi, Authentication: authenticationWire(r.id, arg, calling)})
	return true
}

// MoreAuthenticationInfo: the VLR asks for the next part of the vectors.
func (m *hlrRequests) MoreAuthenticationInfo(d *tcap.Dialogue, invokeID int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byTID[d.LocalTID]
	if r == nil || r.d != d || r.auth == nil || !r.awaitingNext {
		return false
	}
	r.invokeID, r.awaitingNext = invokeID, false
	m.sendNextLocked(r)
	return true
}

// PurgeMS asks the application to mark the subscriber purged.
func (m *hlrRequests) PurgeMS(d *tcap.Dialogue, invokeID int32, arg *mapprotocol.PurgeMSArg) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, calling, ok := m.openLocked(d, invokeID, string(arg.IMSI))
	if !ok {
		return false
	}
	if m.closed {
		m.failLocked(r, "the node is shutting down")
		return true
	}
	m.logger.Debug("purgeMS", zap.String("dialogue_id", r.id), zap.String("imsi", r.imsi), zap.String("calling_gt", calling))
	m.askLocked(r, &natsbridge.NatsMessage{CorrelationID: r.id, IMSI: r.imsi, Purge: &ss7client.Purge{
		DialogueID: r.id, VLRNumber: addressWire(arg.VLRNumber), SGSNNumber: addressWire(arg.SGSNNumber), CallingGT: calling,
	}})
	return true
}

// HLRRequestEnded: the peer ended the dialogue before it was answered, or
// instead of asking for more vectors.
func (m *hlrRequests) HLRRequestEnded(d *tcap.Dialogue, aborted bool, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.byTID[d.LocalTID]; r != nil && r.d == d {
		m.removeLocked(r)
		m.logger.Debug(r.what()+" ended by the peer", zap.String("dialogue_id", r.id), zap.Bool("aborted", aborted), zap.String("detail", detail))
	}
}

// HLRRequestRejected: the VLR rejected a part of the vectors. The
// dialogue is closed at once (TS 29.002 §25.5.6: a notice ends the
// process with a close) rather than held until timer m.
func (m *hlrRequests) HLRRequestRejected(d *tcap.Dialogue, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byTID[d.LocalTID]
	if r == nil || r.d != d {
		return
	}
	m.removeLocked(r)
	m.logger.Info(r.what()+": the peer rejected the answer, dialogue closed", zap.String("dialogue_id", r.id), zap.String("detail", detail))
	if err := m.mapH.Close(r.d); err != nil {
		m.logger.Warn(r.what()+" close not sent", zap.String("dialogue_id", r.id), zap.Error(err))
	}
}

// HLRRequestActive reports whether d has a request.
func (m *hlrRequests) HLRRequestActive(d *tcap.Dialogue) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byTID[d.LocalTID]
	return r != nil && r.d == d
}

// lost is TCAP's lost sink's share: TCAP ended the dialogue on its own.
func (m *hlrRequests) lost(d *tcap.Dialogue, reason tcap.FailureReason, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.byTID[d.LocalTID]; r != nil && r.d == d {
		m.removeLocked(r)
		m.logger.Info(r.what()+" lost", zap.String("dialogue_id", r.id), zap.Stringer("reason", reason), zap.String("detail", detail))
	}
}

// close aborts the node's requests at shutdown and gives up the questions
// asked.
func (m *hlrRequests) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, r := range m.byTID {
		m.abortLocked(r, "the gateway node is shutting down")
	}
	m.cancel()
}

// --- wire mapping --------------------------------------------------------------

// errorCauses maps the wire's error causes to MAP's.
var errorCauses = map[string]mapprotocol.ErrorCause{
	"":                         mapprotocol.CauseNone,
	ss7client.CauseIMSIUnknown: mapprotocol.CauseIMSIUnknown,
	ss7client.CauseGPRSEPSSubscriptionUnknown:  mapprotocol.CauseGPRSEPSSubscriptionUnknown,
	ss7client.CausePLMNRoamingNotAllowed:       mapprotocol.CausePLMNRoamingNotAllowed,
	ss7client.CauseOperatorDeterminedBarring:   mapprotocol.CauseOperatorDeterminedBarring,
	ss7client.CauseSupportedRATTypesNotAllowed: mapprotocol.CauseSupportedRATTypesNotAllowed,
}

// userError is an application's error and the cause qualifying it, as the
// MAP error to send; an error when the cause is unknown or does not
// qualify that error.
func userError(code int32, cause string) (mapprotocol.UserError, error) {
	c, ok := errorCauses[cause]
	if !ok {
		return mapprotocol.UserError{}, fmt.Errorf("error cause %q is unknown", cause)
	}
	e := mapprotocol.UserError{Code: mapprotocol.ErrorCode(code), Cause: c}
	if _, err := e.Parameter(); err != nil {
		return mapprotocol.UserError{}, fmt.Errorf("error cause %q does not qualify error %d", cause, code)
	}
	return e, nil
}

// authenticationWire is what a sendAuthenticationInfo carries, for the
// application.
func authenticationWire(id string, arg *mapprotocol.SendAuthenticationInfoArg, callingGT string) *ss7client.Authentication {
	a := &ss7client.Authentication{
		DialogueID: id, RequestedVectors: arg.NumberOfRequestedVectors,
		ImmediateResponsePreferred: arg.ImmediateResponsePreferred, SegmentationProhibited: arg.SegmentationProhibited,
		AdditionalVectors: arg.AdditionalVectors, AdditionalVectorsAreForEPS: arg.AdditionalVectorsAreForEPS,
		UEUsageTypeRequested: arg.UEUsageTypeRequested, CallingGT: callingGT,
	}
	if t := arg.RequestingNodeType; t != nil {
		a.RequestingNodeType = t.String()
	}
	if arg.RequestingPLMNID != nil {
		a.RequestingPLMNID = hex.EncodeToString(arg.RequestingPLMNID)
	}
	if arg.ResyncRAND != nil {
		a.Resynchronisation = &ss7client.Resynchronisation{RAND: hex.EncodeToString(arg.ResyncRAND), AUTS: hex.EncodeToString(arg.ResyncAUTS)}
	}
	return a
}

// authenticationResMAP maps the application's vectors. Errors name the
// field, never its value: the vectors are key material.
func authenticationResMAP(a *ss7client.Authentication) (*mapprotocol.SendAuthenticationInfoRes, error) {
	res := &mapprotocol.SendAuthenticationInfoRes{}
	var err error
	key := func(what, s string) []byte {
		if err != nil {
			return nil
		}
		b, e := hex.DecodeString(s)
		if e != nil {
			err = fmt.Errorf("%s is not hex", what)
		}
		return b
	}
	for i, v := range a.Triplets {
		p := fmt.Sprintf("triplet %d ", i)
		res.Triplets = append(res.Triplets, mapprotocol.AuthenticationTriplet{
			RAND: key(p+"rand", v.RAND), SRES: key(p+"sres", v.SRES), Kc: key(p+"kc", v.Kc)})
	}
	for i, v := range a.Quintuplets {
		p := fmt.Sprintf("quintuplet %d ", i)
		res.Quintuplets = append(res.Quintuplets, mapprotocol.AuthenticationQuintuplet{
			RAND: key(p+"rand", v.RAND), XRES: key(p+"xres", v.XRES), CK: key(p+"ck", v.CK),
			IK: key(p+"ik", v.IK), AUTN: key(p+"autn", v.AUTN)})
	}
	for i, v := range a.EPSVectors {
		p := fmt.Sprintf("EPS vector %d ", i)
		res.EPSVectors = append(res.EPSVectors, mapprotocol.EPCAV{
			RAND: key(p+"rand", v.RAND), XRES: key(p+"xres", v.XRES), AUTN: key(p+"autn", v.AUTN), KASME: key(p+"kasme", v.KASME)})
	}
	if a.UEUsageType != "" {
		res.UEUsageType = key("ue_usage_type", a.UEUsageType)
	}
	return res, err
}
