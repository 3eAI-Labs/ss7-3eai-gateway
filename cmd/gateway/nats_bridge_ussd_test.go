// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ussdNet records what the sessions send toward the network and publish.
type ussdNet struct {
	mu        sync.Mutex
	sent      []string // "invoke 60 id=2", "release id=1", "reject id=1 34", "close", "abort"
	args      []*mapprotocol.USSDArg
	originate *ussdOriginate
	timeouts  []time.Duration

	responses []ussdResponse
	events    []*natsbridge.NatsMessage
	failures  []ussdResponse

	failResponses bool   // PublishUSSDResponse fails
	onOriginate   func() // runs inside OriginateWith
}

type ussdOriginate struct {
	ac              *tcap.ApplicationContext
	called, calling *sccp.SccpAddress
	op              int32
	param, open     []byte
	corrID          string
	timeout         time.Duration
	d               *tcap.Dialogue
}

type ussdResponse struct {
	corrID, status string
	code           int32
	detail         string
	ussd           *ss7client.USSD
}

func (n *ussdNet) record(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
}

func (n *ussdNet) OriginateWith(ac *tcap.ApplicationContext, called, calling *sccp.SccpAddress, op int32, param []byte,
	corrID string, opts tcap.OriginateOptions) (*tcap.Dialogue, error) {
	// As TCAP: no peer TID until the HLR answers (hlrAnswers).
	d := &tcap.Dialogue{LocalTID: 0x0A0B0C0D, NATSCorrelationID: corrID, ApplicationContext: ac}
	n.mu.Lock()
	n.originate = &ussdOriginate{ac, called, calling, op, param, opts.UserInformation, corrID, opts.Timeout, d}
	hook := n.onOriginate
	n.mu.Unlock()
	if hook != nil {
		hook()
	}
	return d, nil
}

// hlrAnswers gives an originated dialogue the HLR's TID, as its first
// CONTINUE does.
func hlrAnswers(d *tcap.Dialogue) { d.RemoteTID = []byte{1, 2, 3, 4} }

func (n *ussdNet) EndLocally(*tcap.Dialogue) { n.record("end-locally") }
func (n *ussdNet) SetDialogueTimeout(_ *tcap.Dialogue, t time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.timeouts = append(n.timeouts, t)
}
func (n *ussdNet) USSDInvoke(_ *tcap.Dialogue, id int32, op mapprotocol.OperationCode, arg *mapprotocol.USSDArg) error {
	n.mu.Lock()
	n.args = append(n.args, arg)
	n.mu.Unlock()
	n.record(fmt.Sprintf("invoke %d id=%d", op, id))
	return nil
}
func (n *ussdNet) USSDRelease(_ *tcap.Dialogue, id int32, dcs byte, str []byte) error {
	n.mu.Lock()
	n.args = append(n.args, &mapprotocol.USSDArg{DCS: dcs, String: str})
	n.mu.Unlock()
	n.record(fmt.Sprintf("release id=%d", id))
	return nil
}
func (n *ussdNet) USSDRejectRequest(_ *tcap.Dialogue, id int32, code mapprotocol.ErrorCode) error {
	n.record(fmt.Sprintf("reject id=%d %d", id, code))
	return nil
}
func (n *ussdNet) USSDClose(d *tcap.Dialogue) error {
	if len(d.PeerTID()) == 0 {
		return mapprotocol.ErrUSSDNoPeerTID
	}
	n.record("close")
	return nil
}
func (n *ussdNet) USSDAbort(d *tcap.Dialogue) error {
	if len(d.PeerTID()) == 0 {
		return mapprotocol.ErrUSSDNoPeerTID
	}
	n.record("abort")
	return nil
}

func (n *ussdNet) PublishUSSDResponse(corrID string, u *ss7client.USSD) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failResponses {
		return fmt.Errorf("outgoing channel full")
	}
	n.responses = append(n.responses, ussdResponse{corrID: corrID, ussd: u})
	return nil
}
func (n *ussdNet) PublishUSSDEvent(route string, msg *natsbridge.NatsMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if msg.USSD.Route != route {
		panic("event route " + msg.USSD.Route + " published on " + route)
	}
	n.events = append(n.events, msg)
	return nil
}
func (n *ussdNet) PublishFailure(mt natsbridge.MessageType, corrID, status string, code int32, detail string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if mt != natsbridge.MsgTypeMAPUSSD {
		panic("failure of type " + string(mt))
	}
	n.failures = append(n.failures, ussdResponse{corrID: corrID, status: status, code: code, detail: detail})
	return nil
}

func (n *ussdNet) snapshot() (sent []string, responses []ussdResponse, events []*natsbridge.NatsMessage, failures []ussdResponse) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...), append([]ussdResponse(nil), n.responses...),
		append([]*natsbridge.NatsMessage(nil), n.events...), append([]ussdResponse(nil), n.failures...)
}

func newUSSDUnderTest(t *testing.T) (*natsBridge, *ussdSessions, *ussdNet) {
	t.Helper()
	b, _ := locationBridge(t)
	b.sccpCfg.ClusterGlobalTitle.Address = "905900000000"
	b.mapCfg = &mapprotocol.Config{USSDRoutes: []mapprotocol.USSDRoute{
		{Prefix: "*1", Route: "short"}, {Prefix: "*123", Route: "balance"},
	}}
	n := &ussdNet{}
	m := newUSSDSessions("node-a", b.mapCfg, n, n, n, zap.NewNop())
	b.ussd = m
	t.Cleanup(m.close)
	return b, m, n
}

func moDialogue(tid uint32) *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: tid, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &tcap.ACNetworkUnstructuredSsV2}
}

func command(id, action string, corrID string, set func(*ss7client.USSD)) *natsbridge.NatsMessage {
	u := &ss7client.USSD{SessionID: id, Action: action}
	if set != nil {
		set(u)
	}
	return &natsbridge.NatsMessage{CorrelationID: corrID, MessageType: natsbridge.MsgTypeMAPUSSD, USSD: u}
}

func withText(s string) func(*ss7client.USSD) { return func(u *ss7client.USSD) { u.Text = s } }

// The subscriber dials *123#: a begin on the longest matching route, a
// prompt and its answer, the release with the final text.
func TestUSSD_SubscriberInitiatedSession(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	d := moDialogue(7)
	dcs, str, err := mapprotocol.EncodeUSSDText("*123#")
	require.NoError(t, err)
	ok := m.USSDBegin(d, 5, &mapprotocol.USSDArg{DCS: dcs, String: str,
		MSISDN: &mapprotocol.AddressString{Digits: "905321112233"}},
		&mapprotocol.MAPOpenInfo{DestinationReference: &mapprotocol.AddressString{
			NumberPlan: mapprotocol.NumberingPlanLandMobile, Digits: "286010000000123"}})
	require.True(t, ok)
	_, _, events, _ := n.snapshot()
	require.Len(t, events, 1)
	begin := events[0]
	id := begin.USSD.SessionID
	assert.Equal(t, ss7client.USSDBegin, begin.USSD.Action)
	assert.Equal(t, "balance", begin.USSD.Route, "the longest prefix wins")
	assert.Equal(t, "*123#", begin.USSD.Text)
	assert.Equal(t, str, begin.USSD.Data)
	assert.Equal(t, "905321112233", begin.MSISDN)
	assert.Equal(t, "286010000000123", begin.IMSI)
	assert.True(t, strings.HasPrefix(id, m.prefix))
	assert.LessOrEqual(t, len(id), 64)
	assert.Equal(t, []time.Duration{mapprotocol.DefaultUSSDSessionTimeout + ussdTCAPGrace}, n.timeouts)

	assert.False(t, m.USSDBegin(d, 6, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil), "one session per dialogue")

	require.NoError(t, m.command(command(id, ss7client.USSDRequest, "c1", withText("1=yes"))))
	sent, _, _, _ := n.snapshot()
	assert.Equal(t, []string{"invoke 60 id=6"}, sent, "the invoke after the HLR's")

	reply := &mapprotocol.USSDArg{DCS: mapprotocol.DCSGSM7}
	_, reply.String, _ = mapprotocol.EncodeUSSDText("1")
	m.USSDAnswer(d, 9, reply) // no such invoke: dropped
	m.USSDAnswer(d, 6, reply)
	_, responses, _, _ := n.snapshot()
	require.Len(t, responses, 1)
	assert.Equal(t, "c1", responses[0].corrID)
	assert.Equal(t, "1", responses[0].ussd.Text)
	assert.Equal(t, id, responses[0].ussd.SessionID)

	assert.ErrorContains(t, m.command(command(id, ss7client.USSDRelease, "c2", nil)), "final text")
	assert.ErrorContains(t, m.command(command(id, ss7client.USSDReject, "c2", func(u *ss7client.USSD) { u.Error = 27 })), "MAP error 27")
	require.NoError(t, m.command(command(id, ss7client.USSDRelease, "c2", withText("Bye"))))
	sent, responses, events, failures := n.snapshot()
	assert.Equal(t, "release id=5", sent[1], "processUnstructuredSS-Request's result")
	assert.Equal(t, ss7client.USSDRelease, responses[1].ussd.Action)
	require.Len(t, events, 2)
	assert.Equal(t, ss7client.USSDEnd, events[1].USSD.Action)
	assert.Equal(t, "", events[1].Status)
	assert.Equal(t, "application", events[1].USSD.Detail)
	assert.Empty(t, failures)

	assert.ErrorContains(t, m.command(command(id, ss7client.USSDNotify, "c3", withText("x"))), "unknown or ended")
	m.USSDEnded(d, false, "late END") // the session is gone: nothing more
	_, _, events, _ = n.snapshot()
	assert.Len(t, events, 2)
}

// Commands for another node's sessions are not this node's to answer.
func TestUSSD_CommandForAnotherNode(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	other := newUSSDSessions("node-b", &mapprotocol.Config{}, n, n, n, zap.NewNop())
	require.NotEqual(t, m.prefix, other.prefix)
	assert.NoError(t, m.command(command(other.newID(), ss7client.USSDRelease, "c", nil)))
	assert.NoError(t, m.command(&natsbridge.NatsMessage{}))
	assert.ErrorContains(t, m.command(command(m.newID(), ss7client.USSDRelease, "c", nil)), "unknown")
	sent, responses, events, _ := n.snapshot()
	assert.Empty(t, sent)
	assert.Empty(t, responses)
	assert.Empty(t, events)
}

// The application starts a session: the request goes to the subscriber's
// HLR with a MAP-OpenInfo; its answer, a notify, the release.
func TestUSSD_NetworkInitiatedSession(t *testing.T) {
	b, m, n := newUSSDUnderTest(t)
	require.NoError(t, b.onUSSDStart(&natsbridge.NatsMessage{CorrelationID: "s1", MSISDN: "905321112233",
		USSD: &ss7client.USSD{Action: ss7client.USSDRequest, Text: "Balance? 1=yes", Route: "billing"}}))
	o := n.originate
	require.NotNil(t, o)
	assert.Equal(t, tcap.ACNetworkUnstructuredSsV2.OID, o.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpUnstructuredSSRequest), o.op)
	assert.Equal(t, "905321112233", o.called.GlobalTitle.Address)
	assert.Equal(t, sccp.SSNHLR, o.called.SubsystemNumber)
	assert.Equal(t, sccp.SSNGSMSCF, o.calling.SubsystemNumber)
	assert.Equal(t, mapprotocol.DefaultUSSDSessionTimeout+ussdTCAPGrace, o.timeout)
	open, ok, err := mapprotocol.DecodeMAPOpenInfo(o.open)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN,
		Digits: "905321112233"}, *open.DestinationReference)
	assert.Equal(t, "905900000000", open.OriginationReference.Digits, "the gsmSCF: the cluster GT")
	arg, err := mapprotocol.DecodeUSSDArg(o.param)
	require.NoError(t, err)
	text, _ := mapprotocol.DecodeUSSDText(arg.DCS, arg.String)
	assert.Equal(t, "Balance? 1=yes", text)
	id := o.corrID
	assert.True(t, strings.HasPrefix(id, m.prefix), "the dialogue is correlated by its session ID")

	hlrAnswers(o.d)
	m.USSDAnswer(o.d, 1, nil) // an empty reply
	_, responses, _, _ := n.snapshot()
	require.Len(t, responses, 1)
	assert.Equal(t, ussdResponse{corrID: "s1", ussd: &ss7client.USSD{SessionID: id, Action: ss7client.USSDRequest}}, responses[0])

	assert.ErrorContains(t, m.command(command(id, ss7client.USSDReject, "c", func(u *ss7client.USSD) { u.Error = 34 })), "network-initiated")
	assert.ErrorContains(t, m.command(command(id, ss7client.USSDRelease, "c", withText("Bye"))), "without text")
	require.NoError(t, m.command(command(id, ss7client.USSDNotify, "c1", withText("Done"))))
	m.USSDAnswer(o.d, 2, nil)
	require.NoError(t, m.command(command(id, ss7client.USSDRelease, "c2", nil)))
	sent, responses, events, _ := n.snapshot()
	assert.Equal(t, []string{"invoke 61 id=2", "close"}, sent)
	assert.Equal(t, ss7client.USSDNotify, responses[1].ussd.Action)
	require.Len(t, events, 1, "an application's session has no begin event")
	assert.Equal(t, "billing", events[0].USSD.Route)
	assert.Equal(t, "application", events[0].USSD.Detail)
}

func TestUSSD_StartRejections(t *testing.T) {
	for name, c := range map[string]struct {
		msg   natsbridge.NatsMessage
		setup func(*natsBridge)
		want  string
	}{
		"no ussd":       {msg: natsbridge.NatsMessage{MSISDN: "1"}, want: "ussd missing"},
		"begin action":  {msg: natsbridge.NatsMessage{MSISDN: "1", USSD: &ss7client.USSD{Action: "begin", Text: "x"}}, want: "action"},
		"no subscriber": {msg: natsbridge.NatsMessage{USSD: &ss7client.USSD{Action: "request", Text: "x"}}, want: "exactly one"},
		"both":          {msg: natsbridge.NatsMessage{MSISDN: "1", IMSI: "2", USSD: &ss7client.USSD{Action: "request", Text: "x"}}, want: "exactly one"},
		"IMSI of 16":    {msg: natsbridge.NatsMessage{IMSI: "1234567890123456", USSD: &ss7client.USSD{Action: "request", Text: "x"}}, want: "imsi"},
		"route":         {msg: natsbridge.NatsMessage{MSISDN: "1", USSD: &ss7client.USSD{Action: "request", Text: "x", Route: "a.b"}}, want: "route"},
		"no text":       {msg: natsbridge.NatsMessage{MSISDN: "1", USSD: &ss7client.USSD{Action: "request"}}, want: "text"},
		"too long":      {msg: natsbridge.NatsMessage{MSISDN: "1", USSD: &ss7client.USSD{Action: "request", Text: strings.Repeat("ж", 81)}}, want: "at most 160"},
		"IMSI, no HLR":  {msg: natsbridge.NatsMessage{IMSI: "286010000000123", USSD: &ss7client.USSD{Action: "notify", Text: "x"}}, want: "by IMSI"},
		"gsmSCF SSN": {msg: natsbridge.NatsMessage{MSISDN: "1", USSD: &ss7client.USSD{Action: "request", Text: "x"}}, want: "lacks 147",
			setup: func(b *natsBridge) { b.sccpCfg.LocalSubsystems = []sccp.SubsystemNumber{sccp.SSNHLR} }},
	} {
		t.Run(name, func(t *testing.T) {
			b, _, n := newUSSDUnderTest(t)
			if c.setup != nil {
				c.setup(b)
			}
			assert.ErrorContains(t, b.onUSSDStart(&c.msg), c.want)
			assert.Nil(t, n.originate)
		})
	}
}

// A start by IMSI with map.sri_routing "hlr": the destinationReference is
// the IMSI, numbering plan land mobile.
func TestUSSD_StartByIMSI(t *testing.T) {
	b, _, n := newUSSDUnderTest(t)
	b.mapCfg.SRIRouting, b.mapCfg.HLRGlobalTitle = mapprotocol.SRIRoutingHLR, "905320000009"
	require.NoError(t, b.onUSSDStart(&natsbridge.NatsMessage{IMSI: "286010000000123",
		USSD: &ss7client.USSD{Action: ss7client.USSDNotify, Text: "Hi"}}))
	assert.Equal(t, "905320000009", n.originate.called.GlobalTitle.Address)
	assert.Equal(t, int32(mapprotocol.OpUnstructuredSSNotify), n.originate.op)
	open, _, err := mapprotocol.DecodeMAPOpenInfo(n.originate.open)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.NumberingPlanLandMobile, open.DestinationReference.NumberPlan)
	assert.Equal(t, "286010000000123", open.DestinationReference.Digits)
}

// How sessions end other than by the application: the network's error,
// END and ABORT, the TCAP failure sink.
func TestUSSD_NetworkEndings(t *testing.T) {
	start := func(t *testing.T) (*ussdSessions, *ussdNet, *ussdOriginate) {
		b, m, n := newUSSDUnderTest(t)
		require.NoError(t, b.onUSSDStart(&natsbridge.NatsMessage{CorrelationID: "s1", MSISDN: "905321112233",
			USSD: &ss7client.USSD{Action: ss7client.USSDRequest, Text: "?"}}))
		hlrAnswers(n.originate.d)
		return m, n, n.originate
	}

	t.Run("ussd-Busy in a CONTINUE", func(t *testing.T) {
		m, n, o := start(t)
		m.USSDError(o.d, 7, mapprotocol.ErrUSSDBusy, "ussd-Busy", false) // not the outstanding invoke
		m.USSDError(o.d, 1, mapprotocol.ErrUSSDBusy, "ussd-Busy", false)
		sent, _, events, failures := n.snapshot()
		assert.Equal(t, []string{"close"}, sent, "the dialogue is still open: the gateway ends it")
		require.Len(t, failures, 1)
		assert.Equal(t, ussdResponse{corrID: "s1", status: ss7client.StatusRemoteError, code: 72, detail: "ussd-Busy"}, failures[0])
		require.Len(t, events, 1)
		assert.Equal(t, ss7client.StatusRemoteError, events[0].Status)
		assert.Equal(t, int32(72), events[0].ErrorCode)
		assert.Equal(t, "ussd-Busy", events[0].ErrorMessage)
		assert.True(t, o.d.MarkAnswered() == false, "TCAP reports nothing more for it")
	})
	t.Run("absentSubscriber in the END", func(t *testing.T) {
		m, n, o := start(t)
		m.USSDError(o.d, 1, mapprotocol.ErrAbsentSubscriber, "absentSubscriber", true)
		m.USSDEnded(o.d, false, "released by the network")
		sent, _, events, _ := n.snapshot()
		assert.Empty(t, sent)
		require.Len(t, events, 1)
		assert.Equal(t, int32(27), events[0].ErrorCode)
	})
	t.Run("END while a request is outstanding", func(t *testing.T) {
		m, n, o := start(t)
		m.USSDEnded(o.d, false, "released by the network")
		_, _, events, failures := n.snapshot()
		assert.Equal(t, ss7client.StatusRemoteError, failures[0].status)
		assert.Equal(t, ss7client.ErrCodeRemoteError, events[0].ErrorCode)
	})
	t.Run("END with nothing outstanding", func(t *testing.T) {
		m, n, o := start(t)
		m.USSDAnswer(o.d, 1, nil)
		m.USSDEnded(o.d, false, "released by the network")
		_, _, events, failures := n.snapshot()
		assert.Empty(t, failures)
		assert.Equal(t, "", events[0].Status)
		assert.Equal(t, "network", events[0].USSD.Detail)
	})
	t.Run("ABORT", func(t *testing.T) {
		m, n, o := start(t)
		m.USSDEnded(o.d, true, "U-ABORT")
		_, _, events, failures := n.snapshot()
		assert.Equal(t, ss7client.StatusAborted, failures[0].status)
		assert.Equal(t, ss7client.StatusAborted, events[0].Status)
		assert.Equal(t, "U-ABORT", events[0].ErrorMessage)
	})
	t.Run("TCAP: not delivered", func(t *testing.T) {
		m, n, o := start(t)
		m.failed(o.corrID, tcap.FailureNotDelivered, "UDTS: no translation")
		m.failed(o.corrID, tcap.FailureNotDelivered, "UDTS: no translation") // once only
		_, _, events, failures := n.snapshot()
		require.Len(t, events, 1)
		assert.Equal(t, ss7client.ErrCodeAborted, failures[0].code)
		assert.Equal(t, "UDTS: no translation", events[0].ErrorMessage)
	})
}

// A request outstanding blocks another; release cancels it.
func TestUSSD_OneCommandAtATime(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	d := moDialogue(8)
	dcs, str, _ := mapprotocol.EncodeUSSDText("*100#")
	require.True(t, m.USSDBegin(d, 1, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	_, _, events, _ := n.snapshot()
	id := events[0].USSD.SessionID
	assert.Equal(t, "short", events[0].USSD.Route)
	require.NoError(t, m.command(command(id, ss7client.USSDRequest, "c1", withText("?"))))
	assert.ErrorContains(t, m.command(command(id, ss7client.USSDNotify, "c2", withText("!"))), "a request is outstanding")
	require.NoError(t, m.command(command(id, ss7client.USSDReject, "c3", func(u *ss7client.USSD) { u.Error = 71 })))
	sent, responses, events, failures := n.snapshot()
	assert.Equal(t, []string{"invoke 60 id=2", "reject id=1 71"}, sent)
	assert.Equal(t, ussdResponse{corrID: "c1", status: ss7client.StatusAborted, code: ss7client.ErrCodeAborted,
		detail: "the session was released by the application"}, failures[0])
	assert.Equal(t, "c3", responses[0].corrID)
	assert.Equal(t, "application", events[1].USSD.Detail)
}

// The session's own timer ends it, aborting the dialogue; any answer after
// is dropped.
func TestUSSD_SessionTimeout(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	m.timeout = 50 * time.Millisecond
	d := moDialogue(9)
	dcs, str, _ := mapprotocol.EncodeUSSDText("*9#")
	require.True(t, m.USSDBegin(d, 1, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	_, _, events, _ := n.snapshot()
	id := events[0].USSD.SessionID
	require.NoError(t, m.command(command(id, ss7client.USSDRequest, "c1", withText("?"))))
	require.Eventually(t, func() bool { _, _, ev, _ := n.snapshot(); return len(ev) == 2 }, time.Second, 5*time.Millisecond)
	sent, _, events, failures := n.snapshot()
	assert.Equal(t, []string{"invoke 60 id=2", "abort"}, sent)
	assert.Equal(t, ss7client.StatusTimeout, events[1].Status)
	assert.Equal(t, ss7client.ErrCodeTimeout, events[1].ErrorCode)
	assert.Contains(t, events[1].ErrorMessage, "no answer")
	assert.Equal(t, ss7client.StatusTimeout, failures[0].status)
	m.USSDAnswer(d, 2, nil)
	_, responses, _, _ := n.snapshot()
	assert.Empty(t, responses)
}

// At shutdown each session is aborted with its end event; a begin after is
// refused with systemFailure.
func TestUSSD_Close(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	dcs, str, _ := mapprotocol.EncodeUSSDText("*1#")
	require.True(t, m.USSDBegin(moDialogue(1), 3, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	m.close()
	require.True(t, m.USSDBegin(moDialogue(2), 4, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	sent, _, events, _ := n.snapshot()
	assert.Equal(t, []string{"abort", "reject id=4 34"}, sent)
	require.Len(t, events, 2)
	assert.Equal(t, ss7client.StatusAborted, events[1].Status)
}

func TestUSSD_InvokeIDs(t *testing.T) {
	s := &ussdSession{mo: true, beginInvoke: -128, lastInvoke: 126}
	assert.Equal(t, int32(127), s.nextInvokeID())
	assert.Equal(t, int32(-127), s.nextInvokeID(), "wraps, skipping the HLR's")
	mt := &ussdSession{lastInvoke: 127}
	assert.Equal(t, int32(-128), mt.nextInvokeID())
}

// Before the HLR's first answer no END or ABORT can address the dialogue:
// a timeout ends it locally.
func TestUSSD_TimeoutBeforeFirstAnswer(t *testing.T) {
	b, m, n := newUSSDUnderTest(t)
	m.timeout = 30 * time.Millisecond
	require.NoError(t, b.onUSSDStart(&natsbridge.NatsMessage{CorrelationID: "s1", MSISDN: "905321112233",
		USSD: &ss7client.USSD{Action: ss7client.USSDRequest, Text: "?"}}))
	require.Eventually(t, func() bool { _, _, ev, _ := n.snapshot(); return len(ev) == 1 }, time.Second, 5*time.Millisecond)
	sent, _, events, failures := n.snapshot()
	assert.Equal(t, []string{"end-locally"}, sent)
	assert.Equal(t, ss7client.StatusTimeout, events[0].Status)
	assert.Equal(t, "s1", failures[0].corrID)
}

// A session that ends while its BEGIN is being sent leaves no dialogue.
func TestUSSD_EndedWhileOriginating(t *testing.T) {
	b, m, n := newUSSDUnderTest(t)
	n.onOriginate = m.close
	require.NoError(t, b.onUSSDStart(&natsbridge.NatsMessage{CorrelationID: "s1", MSISDN: "905321112233",
		USSD: &ss7client.USSD{Action: ss7client.USSDRequest, Text: "?"}}))
	sent, _, events, failures := n.snapshot()
	assert.Equal(t, []string{"end-locally"}, sent)
	require.Len(t, events, 1)
	assert.Equal(t, ss7client.StatusAborted, events[0].Status)
	assert.Equal(t, ss7client.StatusAborted, failures[0].status)
}

// A reply that cannot be published ends the session: the application
// could not continue it.
func TestUSSD_AnswerNotPublished(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	d := moDialogue(11)
	dcs, str, _ := mapprotocol.EncodeUSSDText("*1#")
	require.True(t, m.USSDBegin(d, 1, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	_, _, events, _ := n.snapshot()
	require.NoError(t, m.command(command(events[0].USSD.SessionID, ss7client.USSDRequest, "c1", withText("?"))))
	n.mu.Lock()
	n.failResponses = true
	n.mu.Unlock()
	m.USSDAnswer(d, 2, nil)
	sent, _, events, _ := n.snapshot()
	assert.Equal(t, []string{"invoke 60 id=2", "abort"}, sent)
	require.Len(t, events, 2)
	assert.Equal(t, ss7client.StatusAborted, events[1].Status)
	assert.False(t, m.USSDActive(d))
}

// TCAP ends a subscriber's dialogue on its own (a message not delivered):
// the session ends at once, not at its timer.
func TestUSSD_DialogueLost(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	d := moDialogue(12)
	dcs, str, _ := mapprotocol.EncodeUSSDText("*1#")
	require.True(t, m.USSDBegin(d, 1, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
	assert.True(t, m.USSDActive(d))
	m.lost(moDialogue(12), tcap.FailureNotDelivered, "another dialogue with the TID") // not this one
	assert.True(t, m.USSDActive(d))
	m.lost(d, tcap.FailureNotDelivered, "UDTS: subsystem failure")
	_, _, events, _ := n.snapshot()
	require.Len(t, events, 2)
	assert.Equal(t, ss7client.StatusAborted, events[1].Status)
	assert.Equal(t, "UDTS: subsystem failure", events[1].ErrorMessage)
	assert.False(t, m.USSDActive(d))
}

// The language indication a DCS puts first is not what was dialled.
func TestUSSD_RouteWithoutLanguage(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	_, str, err := mapprotocol.EncodeUSSDText("en\r*123#")
	require.NoError(t, err)
	require.True(t, m.USSDBegin(moDialogue(13), 1, &mapprotocol.USSDArg{DCS: 0x10, String: str}, nil))
	_, _, events, _ := n.snapshot()
	assert.Equal(t, "balance", events[0].USSD.Route)
	assert.Equal(t, "en\r*123#", events[0].USSD.Text, "the text keeps it, as the contract says")
}

// Commands, answers and the timer from different goroutines (go test -race).
func TestUSSD_Concurrent(t *testing.T) {
	_, m, n := newUSSDUnderTest(t)
	m.timeout = 20 * time.Millisecond
	dcs, str, _ := mapprotocol.EncodeUSSDText("*1#")
	var wg sync.WaitGroup
	for i := range 20 {
		d := moDialogue(uint32(100 + i))
		require.True(t, m.USSDBegin(d, 1, &mapprotocol.USSDArg{DCS: dcs, String: str}, nil))
		_, _, events, _ := n.snapshot()
		id := events[len(events)-1].USSD.SessionID
		wg.Add(3)
		go func() { defer wg.Done(); _ = m.command(command(id, ss7client.USSDRequest, "c", withText("?"))) }()
		go func() { defer wg.Done(); m.USSDAnswer(d, 2, nil); _ = m.USSDActive(d) }()
		go func() { defer wg.Done(); _ = m.command(command(id, ss7client.USSDRelease, "r", withText("bye"))) }()
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.sessions) == 0
	}, time.Second, 5*time.Millisecond)
	_, _, events, _ := n.snapshot()
	ends := 0
	for _, e := range events {
		if e.USSD.Action == ss7client.USSDEnd {
			ends++
		}
	}
	assert.Equal(t, 20, ends, "one end event per session")
}
