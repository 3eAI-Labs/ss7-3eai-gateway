// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// USSD (TS 29.002 §11.9–11.11), with the gateway as gsmSCF under
// networkUnstructuredSsContext-v2. A session is one TCAP dialogue that stays
// open while a human reads and answers. It lives on the node holding the
// dialogue: that node keeps it here, answers its commands (every node hears
// them; the session ID names the node) and publishes its one end event.
//
// A session waits for one thing at a time — the subscriber's answer to the
// request or notify outstanding, or else the application's next command —
// and for at most map.ussd_session_timeout (timer ml of the operations).
// Its TCAP dialogue is given that timeout plus ussdTCAPGrace, so the
// session's own timer, which ends it with an event, decides.

const ussdTCAPGrace = 30 * time.Second

// The TCAP and MAP calls the sessions make; consumer-side so tests can
// record them.
type ussdTCAP interface {
	OriginateWith(ac *tcap.ApplicationContext, called, calling *sccp.SccpAddress, opCode int32,
		parameter []byte, natsCorrelationID string, opts tcap.OriginateOptions) (*tcap.Dialogue, error)
	SetDialogueTimeout(d *tcap.Dialogue, timeout time.Duration)
	EndLocally(d *tcap.Dialogue)
}

type ussdMAP interface {
	USSDInvoke(d *tcap.Dialogue, invokeID int32, op mapprotocol.OperationCode, arg *mapprotocol.USSDArg) error
	USSDRelease(d *tcap.Dialogue, invokeID int32, dcs byte, str []byte) error
	USSDRejectRequest(d *tcap.Dialogue, invokeID int32, code mapprotocol.ErrorCode) error
	USSDClose(d *tcap.Dialogue) error
	USSDAbort(d *tcap.Dialogue) error
}

type ussdPublisher interface {
	PublishUSSDResponse(correlationID string, u *ss7client.USSD) error
	PublishUSSDEvent(route string, msg *natsbridge.NatsMessage) error
	PublishFailure(msgType natsbridge.MessageType, correlationID, status string, errorCode int32, errMsg string) error
}

// ussdRejectCodes are the errors processUnstructuredSS-Request allows
// (TS 29.002 §11.9.3).
var ussdRejectCodes = map[mapprotocol.ErrorCode]bool{
	mapprotocol.ErrSystemFailure: true, mapprotocol.ErrDataMissing: true, mapprotocol.ErrUnexpectedDataValue: true,
	mapprotocol.ErrUnknownAlphabet: true, mapprotocol.ErrCallBarred: true,
}

var ussdRouteName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type ussdSession struct {
	id    string
	route string
	// mo: the subscriber started the session (processUnstructuredSS-Request
	// from the HLR, invoke beginInvoke, answered when the session ends).
	mo          bool
	beginInvoke int32
	// d is the dialogue; for a network-initiated session nil until
	// OriginateWith returns or the HLR answers, whichever comes first.
	d          *tcap.Dialogue
	lastInvoke int32
	pending    *ussdPending
	timer      *time.Timer
	timerGen   uint64
}

// ussdPending is the request or notify a session waits on the network for.
type ussdPending struct {
	invokeID int32
	action   string // ss7client.USSDRequest or USSDNotify
	corrID   string // of the start or command to answer
}

// nextInvokeID: one invoke is outstanding at a time, so IDs only need to
// differ from the one open, which in a subscriber-initiated session is
// processUnstructuredSS-Request's until the end (Invoke ID is -128..127,
// Q.773).
func (s *ussdSession) nextInvokeID() int32 {
	id := s.lastInvoke
	for {
		if id++; id > 127 {
			id = -128
		}
		if !s.mo || id != s.beginInvoke {
			break
		}
	}
	s.lastInvoke = id
	return id
}

type ussdSessions struct {
	tcap    ussdTCAP
	mapH    ussdMAP
	pub     ussdPublisher
	routes  []mapprotocol.USSDRoute // longest prefix first
	timeout time.Duration
	// prefix starts the IDs of this node's sessions.
	prefix string
	logger *zap.Logger

	mu       sync.Mutex
	sessions map[string]*ussdSession
	byTID    map[uint32]*ussdSession // subscriber-initiated, by local TID
	closed   bool
}

func newUSSDSessions(nodeID string, cfg *mapprotocol.Config, t ussdTCAP, m ussdMAP, p ussdPublisher, logger *zap.Logger) *ussdSessions {
	routes := slices.Clone(cfg.USSDRoutes)
	slices.SortStableFunc(routes, func(a, b mapprotocol.USSDRoute) int { return len(b.Prefix) - len(a.Prefix) })
	sum := sha256.Sum256([]byte(nodeID))
	return &ussdSessions{
		tcap: t, mapH: m, pub: p, routes: routes,
		timeout:  cmp.Or(cfg.USSDSessionTimeout, mapprotocol.DefaultUSSDSessionTimeout),
		prefix:   hex.EncodeToString(sum[:6]) + "-",
		logger:   logger,
		sessions: make(map[string]*ussdSession),
		byTID:    make(map[uint32]*ussdSession),
	}
}

// newID: the node's prefix and 128 random bits (39 characters).
func (m *ussdSessions) newID() string { return m.prefix + rand.Text() }

// route is the route of what the subscriber dialled.
func (m *ussdSessions) route(dialled string) string {
	for _, r := range m.routes {
		if strings.HasPrefix(dialled, r.Prefix) {
			return r.Route
		}
	}
	return ss7client.USSDDefaultRoute
}

// sessionLocked finds the session of a dialogue: a network-initiated one by
// the correlation ID it was originated with (its session ID, so an answer
// that beats OriginateWith's return is not lost), a subscriber-initiated
// one by the dialogue's local TID.
func (m *ussdSessions) sessionLocked(d *tcap.Dialogue) *ussdSession {
	if d == nil {
		return nil
	}
	if d.NATSCorrelationID != "" {
		s := m.sessions[d.NATSCorrelationID]
		if s == nil || s.mo {
			return nil
		}
		s.d = d
		return s
	}
	if s := m.byTID[d.LocalTID]; s != nil && s.d == d {
		return s
	}
	return nil
}

// releaseDialogueLocked gives the session's dialogue up: a U-ABORT, or —
// when the peer has not answered it, so no message can address it — a
// local end.
func (m *ussdSessions) releaseDialogueLocked(s *ussdSession) {
	if s.d == nil {
		return
	}
	err := m.mapH.USSDAbort(s.d)
	if errors.Is(err, mapprotocol.ErrUSSDNoPeerTID) {
		m.tcap.EndLocally(s.d)
		return
	}
	if err != nil {
		m.logger.Warn("USSD abort not sent", zap.String("session_id", s.id), zap.Error(err))
	}
}

// armLocked (re)starts the session's wait.
func (m *ussdSessions) armLocked(s *ussdSession) {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timerGen++
	gen := s.timerGen
	s.timer = time.AfterFunc(m.timeout, func() { m.expire(s, gen) })
	if s.d != nil {
		m.tcap.SetDialogueTimeout(s.d, m.timeout+ussdTCAPGrace)
	}
}

func (m *ussdSessions) expire(s *ussdSession, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.id] != s || s.timerGen != gen {
		return
	}
	detail := "no command from the application within map.ussd_session_timeout"
	if s.pending != nil {
		detail = "no answer from the network within map.ussd_session_timeout"
	}
	m.releaseDialogueLocked(s)
	m.endLocked(s, ss7client.StatusTimeout, ss7client.ErrCodeTimeout, detail)
}

// endLocked removes the session and publishes its end event. status ""
// is a release, detail saying by whom ("application", "network"); any
// other status is a failure, detail saying what. A request or notify
// still outstanding is answered with the same failure — or, at a release,
// as aborted.
func (m *ussdSessions) endLocked(s *ussdSession, status string, code int32, detail string) {
	delete(m.sessions, s.id)
	if s.mo {
		delete(m.byTID, s.d.LocalTID)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.d != nil {
		s.d.MarkAnswered() // TCAP reports no failure of a session already ended
	}
	if p := s.pending; p != nil {
		s.pending = nil
		ps, pc, pd := status, code, detail
		if ps == "" {
			ps, pc, pd = ss7client.StatusAborted, ss7client.ErrCodeAborted, "the session was released by the "+detail
		}
		m.fail(p.corrID, ps, pc, pd)
	}
	ev := &natsbridge.NatsMessage{
		Status: status, ErrorCode: code,
		USSD: &ss7client.USSD{SessionID: s.id, Action: ss7client.USSDEnd, Route: s.route},
	}
	if status == "" {
		ev.USSD.Detail = detail
	} else {
		ev.ErrorMessage = detail
	}
	if err := m.pub.PublishUSSDEvent(s.route, ev); err != nil {
		m.logger.Error("USSD end event not published", zap.String("session_id", s.id), zap.Error(err))
	}
	m.logger.Debug("USSD session ended", zap.String("session_id", s.id),
		zap.String("status", status), zap.String("detail", detail))
}

func (m *ussdSessions) fail(corrID, status string, code int32, detail string) {
	if err := m.pub.PublishFailure(natsbridge.MsgTypeMAPUSSD, corrID, status, code, detail); err != nil {
		m.logger.Error("USSD failure not published", zap.String("correlation_id", corrID), zap.Error(err))
	}
}

// --- mapprotocol.USSDSessionHandler (TCAP workers) ------------------------

// USSDBegin opens a subscriber-initiated session and publishes its begin.
func (m *ussdSessions) USSDBegin(d *tcap.Dialogue, invokeID int32, arg *mapprotocol.USSDArg, open *mapprotocol.MAPOpenInfo) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byTID[d.LocalTID] != nil {
		return false
	}
	reject := func(why string, err error) bool {
		m.logger.Warn("USSD session refused: "+why, zap.Uint32("tid", d.LocalTID), zap.Error(err))
		if err := m.mapH.USSDRejectRequest(d, invokeID, mapprotocol.ErrSystemFailure); err != nil {
			m.logger.Warn("USSD refusal not sent", zap.Uint32("tid", d.LocalTID), zap.Error(err))
		}
		return true
	}
	if m.closed {
		return reject("the node is shutting down", nil)
	}

	s := &ussdSession{id: m.newID(), mo: true, d: d, beginInvoke: invokeID, lastInvoke: invokeID}
	dcs := arg.DCS
	u := &ss7client.USSD{SessionID: s.id, Action: ss7client.USSDBegin, DCS: &dcs, Data: arg.String}
	text, err := mapprotocol.DecodeUSSDText(arg.DCS, arg.String)
	if err != nil {
		u.Detail = "USSD string undecodable: " + err.Error()
	}
	u.Text = text
	s.route = m.route(stripLanguage(arg.DCS, text))
	u.Route = s.route
	ev := &natsbridge.NatsMessage{USSD: u}
	if arg.MSISDN != nil {
		ev.MSISDN = arg.MSISDN.Digits
	}
	if open != nil && open.DestinationReference != nil {
		switch ref := open.DestinationReference; ref.NumberPlan {
		case mapprotocol.NumberingPlanLandMobile:
			ev.IMSI = ref.Digits
		case mapprotocol.NumberingPlanISDN:
			if ev.MSISDN == "" {
				ev.MSISDN = ref.Digits
			}
		}
	}
	if err := m.pub.PublishUSSDEvent(s.route, ev); err != nil {
		return reject("begin event not published", err)
	}
	m.sessions[s.id] = s
	m.byTID[d.LocalTID] = s
	m.armLocked(s)
	m.logger.Debug("USSD session begun by the subscriber", zap.String("session_id", s.id),
		zap.String("route", s.route), zap.Uint32("tid", d.LocalTID))
	return true
}

// USSDAnswer answers the request or notify outstanding.
func (m *ussdSessions) USSDAnswer(d *tcap.Dialogue, invokeID int32, res *mapprotocol.USSDArg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessionLocked(d)
	if s == nil {
		return
	}
	p := s.pending
	if p == nil || p.invokeID != invokeID {
		m.logger.Warn("USSD result for no outstanding request, dropped",
			zap.String("session_id", s.id), zap.Int32("invoke_id", invokeID))
		return
	}
	s.pending = nil
	u := &ss7client.USSD{SessionID: s.id, Action: p.action}
	if p.action == ss7client.USSDRequest && res != nil {
		dcs := res.DCS
		u.DCS, u.Data = &dcs, res.String
		u.Text, _ = mapprotocol.DecodeUSSDText(res.DCS, res.String)
	}
	if err := m.pub.PublishUSSDResponse(p.corrID, u); err != nil {
		// The subscriber's reply is lost; a session the application cannot
		// continue is ended rather than left waiting.
		m.logger.Error("USSD answer not published: session aborted", zap.String("session_id", s.id), zap.Error(err))
		m.releaseDialogueLocked(s)
		m.endLocked(s, ss7client.StatusAborted, ss7client.ErrCodeAborted, "the answer could not be published: "+err.Error())
		return
	}
	m.armLocked(s)
}

// USSDActive reports whether d has a session.
func (m *ussdSessions) USSDActive(d *tcap.Dialogue) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionLocked(d) != nil
}

// lost is TCAP's lost sink: it ended a dialogue on its own (a timeout,
// a message not delivered). The dialogue is gone; its session ends.
func (m *ussdSessions) lost(d *tcap.Dialogue, reason tcap.FailureReason, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessionLocked(d); s != nil {
		m.endLocked(s, reason.String(), failureErrorCode(reason), detail)
	}
}

// USSDError: the network refused the request or notify outstanding; the
// session ends with its error.
func (m *ussdSessions) USSDError(d *tcap.Dialogue, invokeID int32, code mapprotocol.ErrorCode, detail string, ended bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessionLocked(d)
	if s == nil {
		return
	}
	if p := s.pending; p == nil || p.invokeID != invokeID {
		m.logger.Warn("USSD error for no outstanding request, dropped",
			zap.String("session_id", s.id), zap.Int32("invoke_id", invokeID), zap.String("detail", detail))
		return
	}
	if !ended {
		var err error
		if s.mo {
			err = m.mapH.USSDRejectRequest(d, s.beginInvoke, mapprotocol.ErrSystemFailure)
		} else {
			err = m.mapH.USSDClose(d)
		}
		if err != nil {
			m.logger.Warn("USSD session end not sent", zap.String("session_id", s.id), zap.Error(err))
		}
	}
	status, errCode := remoteErrorWire(code)
	m.endLocked(s, status, errCode, detail)
}

// USSDEnded: the network ended the dialogue.
func (m *ussdSessions) USSDEnded(d *tcap.Dialogue, aborted bool, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessionLocked(d)
	if s == nil {
		return
	}
	switch {
	case aborted:
		m.endLocked(s, ss7client.StatusAborted, ss7client.ErrCodeAborted, detail)
	case s.pending != nil:
		m.endLocked(s, ss7client.StatusRemoteError, ss7client.ErrCodeRemoteError, "released by the network before answering")
	default:
		m.endLocked(s, "", 0, "network")
	}
}

// failed is the TCAP failure sink's share: a network-initiated session's
// dialogue timed out, was not delivered, or ended without our hearing of
// it in USSDEnded.
func (m *ussdSessions) failed(sessionID string, reason tcap.FailureReason, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[sessionID]
	if s == nil || s.mo {
		return
	}
	m.endLocked(s, reason.String(), failureErrorCode(reason), detail)
}

// --- NATS ----------------------------------------------------------------

// originate starts a network-initiated session: the session exists, with
// its request or notify outstanding, before the BEGIN leaves.
func (m *ussdSessions) originate(called, calling *sccp.SccpAddress, op mapprotocol.OperationCode, param, openInfo []byte,
	route, action, corrID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("USSD start: the node is shutting down")
	}
	// OriginateWith sends the Invoke with ID 1.
	s := &ussdSession{id: m.newID(), route: route, lastInvoke: 1,
		pending: &ussdPending{invokeID: 1, action: action, corrID: corrID}}
	m.sessions[s.id] = s
	m.armLocked(s)
	m.mu.Unlock()

	d, err := m.tcap.OriginateWith(&tcap.ACNetworkUnstructuredSsV2, called, calling, int32(op), param, s.id,
		tcap.OriginateOptions{UserInformation: openInfo, Timeout: m.timeout + ussdTCAPGrace})

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// Nothing went out: the start is refused, and there is no session
		// to end.
		if m.sessions[s.id] == s {
			delete(m.sessions, s.id)
			s.timer.Stop()
		}
		return fmt.Errorf("USSD start: %w", err)
	}
	if m.sessions[s.id] != s {
		// Ended (timeout, shutdown) while the BEGIN was being sent.
		m.tcap.EndLocally(d)
		return nil
	}
	if s.d == nil {
		s.d = d
	}
	m.logger.Debug("USSD session started by the application", zap.String("session_id", s.id),
		zap.String("route", route), zap.Uint32("tid", d.LocalTID))
	return nil
}

// command carries out a command on a session of this node; a command for
// another node's session is not this node's to answer.
func (m *ussdSessions) command(msg *natsbridge.NatsMessage) error {
	u := msg.USSD
	if u == nil || !strings.HasPrefix(u.SessionID, m.prefix) {
		return nil
	}
	corrID := correlationID(msg)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[u.SessionID]
	if s == nil {
		return errors.New("USSD: unknown or ended session")
	}
	what := "USSD " + u.Action
	switch u.Action {
	case ss7client.USSDRequest, ss7client.USSDNotify:
		if s.pending != nil {
			return fmt.Errorf("%s: a %s is outstanding", what, s.pending.action)
		}
		if s.d == nil {
			return fmt.Errorf("%s: the session's dialogue is not open yet", what)
		}
		dcs, str, err := ussdOutgoingText(u)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		op := mapprotocol.OpUnstructuredSSRequest
		if u.Action == ss7client.USSDNotify {
			op = mapprotocol.OpUnstructuredSSNotify
		}
		id := s.nextInvokeID()
		if err := m.mapH.USSDInvoke(s.d, id, op, &mapprotocol.USSDArg{DCS: dcs, String: str}); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		s.pending = &ussdPending{invokeID: id, action: u.Action, corrID: corrID}
		m.armLocked(s)
		return nil

	case ss7client.USSDRelease:
		var err error
		if s.mo {
			dcs, str, terr := ussdOutgoingText(u)
			if terr != nil {
				return fmt.Errorf("%s: a session the subscriber started is released with its final text: %w", what, terr)
			}
			err = m.mapH.USSDRelease(s.d, s.beginInvoke, dcs, str)
		} else {
			if u.Text != "" || u.DCS != nil || len(u.Data) > 0 {
				return fmt.Errorf("%s: a network-initiated session is released without text (send a notify first)", what)
			}
			if s.d == nil {
				return fmt.Errorf("%s: the session's dialogue is not open yet", what)
			}
			// Before the HLR's first answer no END can address the
			// dialogue: it ends here, and the HLR's answer is aborted.
			if err = m.mapH.USSDClose(s.d); errors.Is(err, mapprotocol.ErrUSSDNoPeerTID) {
				m.tcap.EndLocally(s.d)
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}

	case ss7client.USSDReject:
		if !s.mo {
			return fmt.Errorf("%s: a network-initiated session is ended with release", what)
		}
		code := mapprotocol.ErrorCode(u.Error)
		if !ussdRejectCodes[code] {
			return fmt.Errorf("%s: MAP error %d is not one processUnstructuredSS-Request allows", what, u.Error)
		}
		if err := m.mapH.USSDRejectRequest(s.d, s.beginInvoke, code); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}

	default:
		return fmt.Errorf("USSD: action %q is not a command", u.Action)
	}
	// Released or rejected: answered, then the session's end.
	if err := m.pub.PublishUSSDResponse(corrID, &ss7client.USSD{SessionID: s.id, Action: u.Action}); err != nil {
		m.logger.Error("USSD command answer not published", zap.String("session_id", s.id), zap.Error(err))
	}
	m.endLocked(s, "", 0, "application")
	return nil
}

// close aborts the node's sessions at shutdown, each with its end event.
func (m *ussdSessions) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, s := range m.sessions {
		m.releaseDialogueLocked(s)
		m.endLocked(s, ss7client.StatusAborted, ss7client.ErrCodeAborted, "the gateway node is shutting down")
	}
}

// stripLanguage removes the language indication the DCS puts before the
// text (TS 23.038 §5), leaving what the subscriber dialled.
func stripLanguage(dcs byte, text string) string {
	r := []rune(text)
	if n := mapprotocol.USSDLanguageIndication(dcs); n > 0 && len(r) >= n {
		return string(r[n:])
	}
	return text
}

// ussdOutgoingText is the USSD string a start or command sends: Text,
// encoded here (GSM 7-bit, else UCS2), or DCS and Data as given.
func ussdOutgoingText(u *ss7client.USSD) (byte, []byte, error) {
	switch {
	case u.Text != "" && u.DCS == nil && len(u.Data) == 0:
		return mapprotocol.EncodeUSSDText(u.Text)
	case u.Text == "" && u.DCS != nil && len(u.Data) > 0:
		if len(u.Data) > mapprotocol.MaxUSSDStringLength {
			return 0, nil, fmt.Errorf("data of %d octets, at most %d", len(u.Data), mapprotocol.MaxUSSDStringLength)
		}
		return *u.DCS, u.Data, nil
	}
	return 0, nil, errors.New("text, or dcs and data, required")
}

// onUSSDStart starts a network-initiated session: unstructuredSS-Request or
// -Notify to the subscriber's HLR, as gsmSCF (calling SSN 147), with a
// MAP-OpenInfo naming the subscriber and this gsmSCF (TS 29.002 §17.4).
func (b *natsBridge) onUSSDStart(msg *natsbridge.NatsMessage) error {
	u := msg.USSD
	if u == nil {
		return errors.New("USSD start: ussd missing")
	}
	var op mapprotocol.OperationCode
	switch u.Action {
	case ss7client.USSDRequest:
		op = mapprotocol.OpUnstructuredSSRequest
	case ss7client.USSDNotify:
		op = mapprotocol.OpUnstructuredSSNotify
	default:
		return fmt.Errorf("USSD start: action %q, want %q or %q", u.Action, ss7client.USSDRequest, ss7client.USSDNotify)
	}
	// The destinationReference: the MSISDN (E.164), or the IMSI with the land
	// mobile numbering plan (E.212).
	var dest *mapprotocol.AddressString
	switch {
	case (msg.MSISDN == "") == (msg.IMSI == ""):
		return errors.New("USSD start: exactly one of msisdn and imsi required")
	case msg.MSISDN != "":
		if !isDigits(msg.MSISDN) || len(msg.MSISDN) > maxISDNDigits {
			return fmt.Errorf("USSD start: msisdn %q is not E.164 digits (at most %d)", msg.MSISDN, maxISDNDigits)
		}
		dest = &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: msg.MSISDN}
	default:
		if !isDigits(msg.IMSI) || len(msg.IMSI) > 15 {
			return fmt.Errorf("USSD start: imsi %q is not digits (at most 15)", msg.IMSI)
		}
		dest = &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanLandMobile, Digits: msg.IMSI}
	}
	route := u.Route
	if route == "" {
		route = ss7client.USSDDefaultRoute
	}
	if !ussdRouteName.MatchString(route) {
		return fmt.Errorf("USSD start: route %q is not 1..32 of [A-Za-z0-9_-]", route)
	}
	dcs, str, err := ussdOutgoingText(u)
	if err != nil {
		return fmt.Errorf("USSD start: %w", err)
	}
	scf := b.gsmSCFAddress()
	if scf == nil {
		return errors.New("USSD start: no gsmSCF address (set map.gsmscf_address or an sccp global title)")
	}
	if err := b.requireLocalSSN(sccp.SSNGSMSCF); err != nil {
		return fmt.Errorf("USSD start: %w", err)
	}
	called, err := b.calledForHLR(msg, "USSD start")
	if err != nil {
		return err
	}
	openInfo, err := mapprotocol.EncodeMAPOpenInfo(&mapprotocol.MAPOpenInfo{DestinationReference: dest, OriginationReference: scf})
	if err != nil {
		return fmt.Errorf("USSD start: MAP-OpenInfo: %w", err)
	}
	param, err := mapprotocol.EncodeUSSDArg(&mapprotocol.USSDArg{DCS: dcs, String: str})
	if err != nil {
		return fmt.Errorf("USSD start: %w", err)
	}
	return b.ussd.originate(called, b.callingSCCP(sccp.SSNGSMSCF), op, param, openInfo, route, u.Action, correlationID(msg))
}

func isUSSDOp(op mapprotocol.OperationCode) bool {
	return op == mapprotocol.OpProcessUnstructuredSSReq || op == mapprotocol.OpUnstructuredSSRequest ||
		op == mapprotocol.OpUnstructuredSSNotify
}
