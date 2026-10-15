// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
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

// luNet plays TCAP, MAP and NATS for location updates: it records what is
// sent towards the VLR and published, and hands each turn to the test,
// which answers it.
type luNet struct {
	mu       sync.Mutex
	sent     []string // "isd id=1", "accept id=5 905309999999", "refuse id=5 34", "abort"
	params   [][]byte // of the insertSubscriberData sent
	ends     []*natsbridge.NatsMessage
	timeouts []time.Duration
	// fitsUnder: a message fits when its parameter is at most this long
	// (0: everything fits).
	fitsUnder int
	turns     chan *luTurn
}

type luTurn struct {
	msg    *natsbridge.NatsMessage
	answer chan luReply
}

type luReply struct {
	msg *natsbridge.NatsMessage
	err error
}

func (n *luNet) record(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
}

func (n *luNet) sentSoFar() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

func (n *luNet) Fits(msg *tcap.Message) (bool, error) {
	return n.fitsUnder == 0 || len(msg.Components[0].Parameter) <= n.fitsUnder, nil
}
func (n *luNet) SetDialogueTimeout(_ *tcap.Dialogue, t time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.timeouts = append(n.timeouts, t)
}
func (n *luNet) InsertSubscriberDataMessage(d *tcap.Dialogue, id int32, param []byte) *tcap.Message {
	code := int32(mapprotocol.OpInsertSubscriberData)
	return &tcap.Message{MessageType: tcap.MsgTypeContinue, Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: &id, OperationCode: &code, Parameter: param}}}
}
func (n *luNet) Send(_ *tcap.Dialogue, msg *tcap.Message) error {
	n.mu.Lock()
	n.params = append(n.params, msg.Components[0].Parameter)
	n.mu.Unlock()
	n.record(fmt.Sprintf("isd id=%d", *msg.Components[0].InvokeID))
	return nil
}
func (n *luNet) UpdateLocationAccept(_ *tcap.Dialogue, id int32, hlr mapprotocol.AddressString) error {
	n.record(fmt.Sprintf("accept id=%d %s", id, hlr.Digits))
	return nil
}
func (n *luNet) UpdateLocationRefuse(_ *tcap.Dialogue, id int32, code mapprotocol.ErrorCode) error {
	n.record(fmt.Sprintf("refuse id=%d %d", id, code))
	return nil
}
func (n *luNet) LocationUpdateAbort(*tcap.Dialogue) error { n.record("abort"); return nil }

func (n *luNet) RequestLocationUpdate(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error) {
	t := &luTurn{msg: msg, answer: make(chan luReply, 1)}
	n.turns <- t
	select {
	case r := <-t.answer:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (n *luNet) PublishLocationUpdateEnd(msg *natsbridge.NatsMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ends = append(n.ends, msg)
	return nil
}

func (n *luNet) endsSoFar() []*natsbridge.NatsMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*natsbridge.NatsMessage(nil), n.ends...)
}

func newLURig(t *testing.T) (*locationUpdates, *luNet) {
	t.Helper()
	n := &luNet{turns: make(chan *luTurn, 4)}
	hlr := &mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905309999999"}
	m := newLocationUpdates("node-1", hlr, &mapprotocol.Config{}, n, n, n, zap.NewNop())
	t.Cleanup(m.cancel)
	return m, n
}

func (n *luNet) nextTurn(t *testing.T) *luTurn {
	t.Helper()
	select {
	case turn := <-n.turns:
		return turn
	case <-time.After(2 * time.Second):
		t.Fatal("no turn asked")
		return nil
	}
}

func (n *luNet) waitSent(t *testing.T, want ...string) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Join(n.sentSoFar(), "|") == strings.Join(want, "|") },
		2*time.Second, 5*time.Millisecond, "sent %v, want %v", n.sentSoFar(), want)
}

func (turn *luTurn) reply(action string, data *ss7client.SubscriberData, code int32) {
	turn.answer <- luReply{msg: &natsbridge.NatsMessage{SubscriberData: data,
		LocationUpdate: &ss7client.LocationUpdate{Action: action, Error: code}}}
}

func luDialogueAt(tid uint32) *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: tid, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &tcap.ACNetworkLocUpV3,
		RemoteAddress: &sccp.SccpAddress{GlobalTitle: &sccp.GlobalTitle{Address: "905321000002"}}}
}

var luArg = &mapprotocol.UpdateLocationArg{IMSI: "286010000000123",
	MSCNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000001"},
	VLRNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000002"},
	LMSI:      []byte{1, 2, 3, 4},
	VLRCapability: &mapprotocol.VLRCapability{SupportedCAMELPhases: []int{0, 1, 2},
		SupportedRATTypes: []int{0, 1, 4}, MSISDNLessOperationSupported: true}}

func profile() *ss7client.SubscriberData {
	msisdn := ss7client.E164("905312345678")
	return &ss7client.SubscriberData{MSISDN: &msisdn, Category: "0a", SubscriberStatus: "serviceGranted",
		Teleservices: []string{"11", "21", "22"}, BearerServices: []string{"16"},
		CallBarring: []ss7client.CallBarringInfo{{SSCode: "92", Features: []ss7client.CallBarringFeature{{Status: ss7client.SSStatus{Provisioned: true}}}}},
		SSData:      []ss7client.SSData{{SSCode: "12", Status: ss7client.SSStatus{Provisioned: true, Active: true}, CLIRestriction: "temporaryDefaultRestricted"}}}
}

// A location update the application serves: data, its result, accept.
func TestLocationUpdate_InsertThenAccept(t *testing.T) {
	m, n := newLURig(t)
	d := luDialogueAt(0x01000001)
	require.True(t, m.UpdateLocation(d, 5, luArg))
	assert.True(t, m.LocationUpdateActive(d))
	assert.False(t, m.UpdateLocation(d, 6, luArg), "one location update per dialogue")

	turn := n.nextTurn(t)
	u := turn.msg.LocationUpdate
	assert.Equal(t, ss7client.ULUpdateLocation, u.Action)
	assert.Equal(t, "286010000000123", turn.msg.IMSI)
	assert.True(t, strings.HasPrefix(u.DialogueID, m.prefix))
	assert.Equal(t, "905321000002", u.VLRNumber.Digits)
	assert.Equal(t, "905321000002", u.CallingGT)
	assert.Equal(t, "01020304", u.LMSI)
	assert.Equal(t, []int{1, 2, 3}, u.VLRCapability.SupportedCAMELPhases)
	assert.Equal(t, []string{"utran", "geran", "e-utran"}, u.VLRCapability.SupportedRATTypes)
	require.NotNil(t, u.AnswerBy)
	assert.WithinDuration(t, time.Now().Add(mapprotocol.DefaultHLRAnswerTimeout), *u.AnswerBy, time.Second)

	turn.reply(ss7client.ULInsert, profile(), 0)
	n.waitSent(t, "isd id=1")
	arg, err := mapprotocol.DecodeInsertSubscriberDataArg(n.params[0])
	require.NoError(t, err)
	assert.Empty(t, arg.IMSI, "no IMSI within a location update")
	assert.Equal(t, "905312345678", arg.Data.MSISDN.Digits)
	assert.Equal(t, [][]byte{{0x11}, {0x21}, {0x22}}, arg.Data.Teleservices)
	require.Len(t, arg.Data.ProvisionedSS, 2)
	assert.Equal(t, byte(0x04), arg.Data.ProvisionedSS[0].CallBarring.Features[0].SSStatus)
	assert.Equal(t, 1, *arg.Data.ProvisionedSS[1].SSData.CLIRestrictionOption)

	m.InsertSubscriberDataResult(d, 2, &mapprotocol.InsertSubscriberDataRes{}, false) // not the outstanding invoke
	m.InsertSubscriberDataResult(d, 1, &mapprotocol.InsertSubscriberDataRes{Teleservices: [][]byte{{0x22}}}, false)
	turn = n.nextTurn(t)
	assert.Equal(t, ss7client.ULInsertResult, turn.msg.LocationUpdate.Action)
	assert.Equal(t, u.DialogueID, turn.msg.LocationUpdate.DialogueID)
	assert.Equal(t, "905321000002", turn.msg.LocationUpdate.VLRNumber.Digits, "every turn carries the request")
	assert.Equal(t, []string{"22"}, turn.msg.InsertResult.UnsupportedTeleservices)
	assert.Empty(t, turn.msg.Status)

	turn.reply(ss7client.ULAccept, nil, 0)
	n.waitSent(t, "isd id=1", "accept id=5 905309999999")
	assert.False(t, m.LocationUpdateActive(d))
	assert.Empty(t, n.endsSoFar(), "the application ended it: no end turn")
}

// Data that does not fit one message goes in parts, one at a time; their
// results are one.
func TestLocationUpdate_DataInParts(t *testing.T) {
	m, n := newLURig(t)
	n.fitsUnder = 30
	data, err := subscriberDataMAP(profile())
	require.NoError(t, err)
	parts, err := mapprotocol.SplitSubscriberData(data, func(p []byte) (bool, error) { return len(p) <= 30, nil })
	require.NoError(t, err)
	require.Greater(t, len(parts), 1)

	d := luDialogueAt(0x01000002)
	m.UpdateLocation(d, 5, luArg)
	n.nextTurn(t).reply(ss7client.ULInsert, profile(), 0)
	var want []string
	for i := range parts {
		want = append(want, fmt.Sprintf("isd id=%d", i+1))
		n.waitSent(t, want...)
		select {
		case <-n.turns:
			t.Fatal("asked before every part was answered")
		default:
		}
		res := &mapprotocol.InsertSubscriberDataRes{}
		if i == 0 {
			res.SSCodes = []byte{0x12}
		}
		if i == len(parts)-1 {
			res.Teleservices = [][]byte{{0x22}}
		}
		m.InsertSubscriberDataResult(d, int32(i+1), res, false)
	}
	turn := n.nextTurn(t)
	assert.Equal(t, []string{"12"}, turn.msg.InsertResult.UnsupportedSSCodes)
	assert.Equal(t, []string{"22"}, turn.msg.InsertResult.UnsupportedTeleservices)
	assert.Equal(t, parts, n.params, "the parts as split")
	turn.reply(ss7client.ULAccept, nil, 0)
	n.waitSent(t, append(want, "accept id=5 905309999999")...)
}

// The application's refusal is carried out and ends it; a refusal on the
// gateway's own account (no answer, an answer it cannot carry out) is
// told with an end turn.
func TestLocationUpdate_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		answer  func(*luTurn)
		want    string
		endWith string // the end turn's status; "" none
	}{
		"roamingNotAllowed": {func(turn *luTurn) { turn.reply(ss7client.ULReject, nil, ss7client.MAPErrRoamingNotAllowed) },
			"refuse id=5 8", ""},
		"an error updateLocation does not allow": {func(turn *luTurn) { turn.reply(ss7client.ULReject, nil, ss7client.MAPErrCallBarred) },
			"refuse id=5 34", ss7client.StatusRejected},
		"an unknown action": {func(turn *luTurn) { turn.reply("release", nil, 0) }, "refuse id=5 34", ss7client.StatusRejected},
		"no location_update": {func(turn *luTurn) { turn.answer <- luReply{msg: &natsbridge.NatsMessage{}} },
			"refuse id=5 34", ss7client.StatusRejected},
		"no application": {func(turn *luTurn) { turn.answer <- luReply{err: natsbridge.ErrNoResponder} },
			"refuse id=5 34", ss7client.StatusRejected},
		"no answer in time": {func(*luTurn) {}, "refuse id=5 34", ss7client.StatusTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			m, n := newLURig(t)
			m.answerTimeout = 100 * time.Millisecond
			d := luDialogueAt(0x01000003)
			m.UpdateLocation(d, 5, luArg)
			tc.answer(n.nextTurn(t))
			n.waitSent(t, tc.want)
			assert.False(t, m.LocationUpdateActive(d))
			if tc.endWith == "" {
				time.Sleep(20 * time.Millisecond)
				assert.Empty(t, n.endsSoFar())
				return
			}
			require.Eventually(t, func() bool { return len(n.endsSoFar()) == 1 }, 2*time.Second, 5*time.Millisecond)
			ends := n.endsSoFar()
			assert.Equal(t, tc.endWith, ends[0].Status)
			assert.Contains(t, ends[0].ErrorMessage, "systemFailure")
		})
	}
}

// What the gateway cannot send goes back to the application to decide.
func TestLocationUpdate_DataNotSent(t *testing.T) {
	for name, tc := range map[string]struct {
		data      *ss7client.SubscriberData
		fitsUnder int
		want      string
	}{
		"invalid":   {&ss7client.SubscriberData{Category: "0a0b"}, 0, "category"},
		"too large": {profile(), 5, "does not fit"},
		"missing":   {nil, 0, "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			m, n := newLURig(t)
			n.fitsUnder = tc.fitsUnder
			d := luDialogueAt(0x01000004)
			m.UpdateLocation(d, 5, luArg)
			n.nextTurn(t).reply(ss7client.ULInsert, tc.data, 0)
			turn := n.nextTurn(t)
			assert.Equal(t, ss7client.ULInsertResult, turn.msg.LocationUpdate.Action)
			assert.Equal(t, ss7client.StatusRejected, turn.msg.Status)
			assert.Contains(t, turn.msg.ErrorMessage, tc.want)
			assert.Empty(t, n.sentSoFar())
			turn.reply(ss7client.ULReject, nil, ss7client.MAPErrSystemFailure)
			n.waitSent(t, "refuse id=5 34")
		})
	}
}

// The VLR's error to the data is a turn; with the dialogue ended, the end.
func TestLocationUpdate_VLRRefusesData(t *testing.T) {
	m, n := newLURig(t)
	d := luDialogueAt(0x01000005)
	m.UpdateLocation(d, 5, luArg)
	n.nextTurn(t).reply(ss7client.ULInsert, profile(), 0)
	n.waitSent(t, "isd id=1")
	m.InsertSubscriberDataError(d, 1, mapprotocol.ErrUnexpectedDataValue, "unexpectedDataValue", false)
	turn := n.nextTurn(t)
	assert.Equal(t, ss7client.StatusRemoteError, turn.msg.Status)
	assert.Equal(t, int32(36), turn.msg.ErrorCode)
	turn.reply(ss7client.ULReject, nil, ss7client.MAPErrSystemFailure)
	n.waitSent(t, "isd id=1", "refuse id=5 34")

	d = luDialogueAt(0x01000006)
	m.UpdateLocation(d, 5, luArg)
	n.nextTurn(t).reply(ss7client.ULInsert, profile(), 0)
	require.Eventually(t, func() bool { return len(n.sentSoFar()) == 3 }, 2*time.Second, 5*time.Millisecond)
	m.InsertSubscriberDataError(d, 1, mapprotocol.ErrUnidentifiedSubscriber, "unidentifiedSubscriber", true)
	m.LocationUpdateEnded(d, false, "ended by the VLR")
	ends := n.endsSoFar()
	require.Len(t, ends, 1)
	assert.Equal(t, ss7client.ULEnd, ends[0].LocationUpdate.Action)
	assert.Equal(t, ss7client.StatusRemoteError, ends[0].Status)
	assert.Equal(t, int32(5), ends[0].ErrorCode)
	assert.Contains(t, ends[0].ErrorMessage, "unidentifiedSubscriber")
	assert.Nil(t, ends[0].LocationUpdate.AnswerBy, "an end is not answered")
}

// The VLR gives up while the application is deciding: the end turn, and the
// late answer is dropped.
func TestLocationUpdate_VLRAbortsWhileAsking(t *testing.T) {
	m, n := newLURig(t)
	d := luDialogueAt(0x01000007)
	m.UpdateLocation(d, 5, luArg)
	turn := n.nextTurn(t)
	m.LocationUpdateEnded(d, true, "dialogue aborted by the VLR")
	assert.Empty(t, n.endsSoFar(), "the end waits for the turn in flight: one turn at a time")
	turn.reply(ss7client.ULAccept, nil, 0)
	require.Eventually(t, func() bool { return len(n.endsSoFar()) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, ss7client.StatusAborted, n.endsSoFar()[0].Status)
	assert.Empty(t, n.sentSoFar(), "the answer is not carried out in a dialogue that is over")
}

func TestLocationUpdate_VLRSilent(t *testing.T) {
	m, n := newLURig(t)
	m.networkTimeout = 50 * time.Millisecond
	d := luDialogueAt(0x01000008)
	m.UpdateLocation(d, 5, luArg)
	n.nextTurn(t).reply(ss7client.ULInsert, profile(), 0)
	n.waitSent(t, "isd id=1", "abort")
	require.Eventually(t, func() bool { return len(n.endsSoFar()) == 1 }, 2*time.Second, 5*time.Millisecond)
	ends := n.endsSoFar()
	assert.Equal(t, ss7client.StatusTimeout, ends[0].Status)
	assert.Contains(t, n.timeouts, m.networkTimeout+luTCAPGrace, "TCAP's timer stays behind")
}

func TestLocationUpdate_LostAndShutdown(t *testing.T) {
	m, n := newLURig(t)
	d1, d2 := luDialogueAt(0x01000009), luDialogueAt(0x0100000A)
	m.UpdateLocation(d1, 5, luArg)
	m.UpdateLocation(d2, 5, luArg)
	n.nextTurn(t)
	n.nextTurn(t)
	m.lost(d1, tcap.FailureTimeout, "dialogue timeout")
	m.close()
	n.waitSent(t, "abort")
	require.Eventually(t, func() bool { return len(n.endsSoFar()) == 2 }, 2*time.Second, 5*time.Millisecond,
		"the ends follow the turns in flight, given up at close")
	statuses := []string{n.endsSoFar()[0].Status, n.endsSoFar()[1].Status}
	assert.ElementsMatch(t, []string{"TIMEOUT", "ABORTED"}, statuses)

	assert.True(t, m.UpdateLocation(luDialogueAt(0x0100000B), 7, luArg))
	n.waitSent(t, "abort", "refuse id=7 34")
	assert.Len(t, n.endsSoFar(), 3)
}

func TestLocationUpdate_NoHLRNumber(t *testing.T) {
	m, n := newLURig(t)
	m.hlrNumber = nil
	m.UpdateLocation(luDialogueAt(0x0100000C), 5, luArg)
	n.waitSent(t, "refuse id=5 34")
	require.Len(t, n.endsSoFar(), 1)
	assert.Contains(t, n.endsSoFar()[0].ErrorMessage, "HLR number")
}

// The VLR's END carries the last result: no turn is asked for a dialogue
// that is over; the end says how it ended.
func TestLocationUpdate_ResultInTheVLRsEnd(t *testing.T) {
	m, n := newLURig(t)
	d := luDialogueAt(0x0100000D)
	m.UpdateLocation(d, 5, luArg)
	n.nextTurn(t).reply(ss7client.ULInsert, profile(), 0)
	n.waitSent(t, "isd id=1")
	m.InsertSubscriberDataResult(d, 1, &mapprotocol.InsertSubscriberDataRes{}, true)
	m.LocationUpdateEnded(d, false, "ended by the VLR")
	select {
	case turn := <-n.turns:
		t.Fatalf("turn %s asked after the end", turn.msg.LocationUpdate.Action)
	case <-time.After(50 * time.Millisecond):
	}
	require.Len(t, n.endsSoFar(), 1)
	assert.Equal(t, ss7client.StatusAborted, n.endsSoFar()[0].Status)
}

// --- requests to a VLR -------------------------------------------------------

func TestBridge_VLRRequests(t *testing.T) {
	b, rec := locationBridge(t)
	vlr := map[string]interface{}{ss7client.ParamVLRNumber: "905321000002"}
	msc := ss7client.E164("905321000001")

	require.NoError(t, b.onCLRequest(&natsbridge.NatsMessage{CorrelationID: "c1", IMSI: "286010000000123", Parameters: vlr,
		CancelLocation: &ss7client.CancelLocation{Type: ss7client.CancellationSubscriptionWithdraw, LMSI: "01020304"}}))
	require.NoError(t, b.onPRNRequest(&natsbridge.NatsMessage{CorrelationID: "c2", IMSI: "286010000000123", Parameters: vlr,
		RoamingNumberEnquiry: &ss7client.RoamingNumberEnquiry{MSCNumber: &msc, CallReferenceNumber: "abcd",
			BearerCapability: &ss7client.SignalInfo{ProtocolID: 1, Data: "a00401"}}}))
	require.NoError(t, b.onISDRequest(&natsbridge.NatsMessage{CorrelationID: "c3", IMSI: "286010000000123", Parameters: vlr, DPC: 77,
		SubscriberData: &ss7client.SubscriberData{SubscriberStatus: "operatorDeterminedBarring",
			ODB: &ss7client.ODB{General: []string{"internationalOGCallsBarred"}}}}))
	require.Len(t, rec.calls, 3)
	for i, want := range []struct {
		ac []int
		op mapprotocol.OperationCode
	}{{tcap.ACLocationCancellationV3.OID, mapprotocol.OpCancelLocation},
		{tcap.ACRoamingNumberEnquiryV3.OID, mapprotocol.OpProvideRoamingNumber},
		{tcap.ACSubscriberDataMngtV3.OID, mapprotocol.OpInsertSubscriberData}} {
		c := rec.calls[i]
		assert.Equal(t, want.ac, c.ac.OID)
		assert.Equal(t, int32(want.op), c.opCode)
		assert.Equal(t, sccp.SSNVLR, c.called.SubsystemNumber)
		assert.Equal(t, sccp.SSNHLR, c.calling.SubsystemNumber, "the VLR answers the HLR")
		assert.Equal(t, "905321000002", c.called.GlobalTitle.Address)
	}
	assert.Equal(t, uint32(77), rec.calls[2].called.PointCode)

	cl, err := mapprotocol.DecodeCancelLocationArg(rec.calls[0].parameter)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.CancellationSubscriptionWithdraw, *cl.CancellationType)
	assert.Equal(t, []byte{1, 2, 3, 4}, cl.LMSI)
	prn, err := mapprotocol.DecodeProvideRoamingNumberArg(rec.calls[1].parameter)
	require.NoError(t, err)
	assert.Equal(t, "905321000001", prn.MSCNumber.Digits)
	assert.Equal(t, []byte{0xAB, 0xCD}, prn.CallReferenceNumber)
	assert.Equal(t, []byte{0xA0, 0x04, 0x01}, prn.GSMBearerCapability.SignalInfo)
	isd, err := mapprotocol.DecodeInsertSubscriberDataArg(rec.calls[2].parameter)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.IMSI("286010000000123"), isd.IMSI, "a stand-alone insert names the subscriber")
	assert.Equal(t, mapprotocol.NamedBits{1}, isd.Data.ODB.General)
}

func TestBridge_VLRRequestRejections(t *testing.T) {
	vlr := map[string]interface{}{ss7client.ParamVLRNumber: "905321000002"}
	msc := ss7client.E164("905321000001")
	for name, tc := range map[string]struct {
		call  func(*natsBridge) error
		setup func(*natsBridge)
		want  string
	}{
		"no VLR": {call: func(b *natsBridge) error {
			return b.onCLRequest(&natsbridge.NatsMessage{IMSI: "286010000000123"})
		}, want: "vlr_number"},
		"bad IMSI": {call: func(b *natsBridge) error {
			return b.onCLRequest(&natsbridge.NatsMessage{IMSI: "28601", Parameters: vlr, CancelLocation: &ss7client.CancelLocation{LMSI: "01"}})
		}, want: "lmsi"},
		"HLR SSN not local": {call: func(b *natsBridge) error {
			return b.onCLRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr})
		}, setup: func(b *natsBridge) { b.sccpCfg.LocalSubsystems = []sccp.SubsystemNumber{sccp.SSNMSC} }, want: "lacks 6"},
		"cancellation type": {call: func(b *natsBridge) error {
			return b.onCLRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr,
				CancelLocation: &ss7client.CancelLocation{Type: "initialAttachProcedure"}})
		}, want: "type"},
		"PRN without MSC": {call: func(b *natsBridge) error {
			return b.onPRNRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr})
		}, want: "msc_number"},
		"PRN protocol": {call: func(b *natsBridge) error {
			return b.onPRNRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr,
				RoamingNumberEnquiry: &ss7client.RoamingNumberEnquiry{MSCNumber: &msc, BearerCapability: &ss7client.SignalInfo{ProtocolID: 9, Data: "a0"}}})
		}, want: "protocolId"},
		"ISD without data": {call: func(b *natsBridge) error {
			return b.onISDRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr})
		}, want: "missing"},
		"ISD ODB without status": {call: func(b *natsBridge) error {
			return b.onISDRequest(&natsbridge.NatsMessage{IMSI: "286010000000123", Parameters: vlr,
				SubscriberData: &ss7client.SubscriberData{ODB: &ss7client.ODB{}}})
		}, want: "odb"},
	} {
		t.Run(name, func(t *testing.T) {
			b, rec := locationBridge(t)
			if tc.setup != nil {
				tc.setup(b)
			}
			err := tc.call(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, rec.calls)
		})
	}
}

func TestBridge_HLRNumber(t *testing.T) {
	b, _ := locationBridge(t)
	assert.Equal(t, "905900000001", b.hlrNumber().Digits, "the node GT")
	b.sccpCfg.ClusterGlobalTitle.Address = "905900000000"
	assert.Equal(t, "905900000000", b.hlrNumber().Digits, "the cluster GT")
	b.mapCfg.HLRNumber = "905309999999"
	assert.Equal(t, "905309999999", b.hlrNumber().Digits, "map.hlr_number")
	b.mapCfg.HLRNumber, b.sccpCfg.ClusterGlobalTitle.Address, b.sccpCfg.LocalGlobalTitle.Address = "", "", ""
	assert.Nil(t, b.hlrNumber())
}
