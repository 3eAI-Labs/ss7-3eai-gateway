// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/hex"
	"errors"
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

// hrNet plays TCAP, MAP and NATS for authentication and purging: it
// records what is sent towards the VLR and hands each request to the
// test, which answers it.
type hrNet struct {
	mu     sync.Mutex
	sent   []string // "result id=1 more", "result id=2 end", "refuse id=1 34", "purge id=3 tmsi", "abort"
	params [][]byte // of the results sent
	// fitsUnder: a message fits when its parameter is at most this long
	// (0: everything fits).
	fitsUnder int
	requests  chan *hrAsk
}

type hrAsk struct {
	msg    *natsbridge.NatsMessage
	answer chan luReply
}

func (n *hrNet) record(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
}

func (n *hrNet) sentSoFar() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

func (n *hrNet) Fits(msg *tcap.Message) (bool, error) {
	return n.fitsUnder == 0 || len(msg.Components[0].Parameter) <= n.fitsUnder, nil
}
func (n *hrNet) SetDialogueTimeout(*tcap.Dialogue, time.Duration) {}
func (n *hrNet) AuthenticationInfoMessage(d *tcap.Dialogue, id int32, param []byte, more bool) *tcap.Message {
	t := tcap.MsgTypeEnd
	if more {
		t = tcap.MsgTypeContinue
	}
	return &tcap.Message{MessageType: t, Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: &id, Parameter: param}}}
}
func (n *hrNet) Send(_ *tcap.Dialogue, msg *tcap.Message) error {
	n.mu.Lock()
	n.params = append(n.params, msg.Components[0].Parameter)
	n.mu.Unlock()
	how := "end"
	if msg.MessageType == tcap.MsgTypeContinue {
		how = "more"
	}
	n.record(fmt.Sprintf("result id=%d %s", *msg.Components[0].InvokeID, how))
	return nil
}
func (n *hrNet) HLRRequestRefuse(_ *tcap.Dialogue, id int32, e mapprotocol.UserError) error {
	if e.Cause != mapprotocol.CauseNone {
		n.record(fmt.Sprintf("refuse id=%d %d %s", id, e.Code, e.Cause))
		return nil
	}
	n.record(fmt.Sprintf("refuse id=%d %d", id, e.Code))
	return nil
}
func (n *hrNet) PurgeMSAccept(_ *tcap.Dialogue, id int32, res *mapprotocol.PurgeMSRes) error {
	var freezes []string
	for name, set := range map[string]bool{"tmsi": res.FreezeTMSI, "ptmsi": res.FreezePTMSI, "mtmsi": res.FreezeMTMSI} {
		if set {
			freezes = append(freezes, name)
		}
	}
	n.record(fmt.Sprintf("purge id=%d %s", id, strings.Join(freezes, ",")))
	return nil
}
func (n *hrNet) Abort(*tcap.Dialogue) error { n.record("abort"); return nil }
func (n *hrNet) Close(*tcap.Dialogue) error { n.record("close"); return nil }

func (n *hrNet) ask(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error) {
	a := &hrAsk{msg: msg, answer: make(chan luReply, 1)}
	n.requests <- a
	select {
	case r := <-a.answer:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (n *hrNet) RequestAuthentication(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error) {
	return n.ask(ctx, msg)
}
func (n *hrNet) RequestPurge(ctx context.Context, msg *natsbridge.NatsMessage) (*natsbridge.NatsMessage, error) {
	return n.ask(ctx, msg)
}

func newHRRig(t *testing.T) (*hlrRequests, *hrNet) {
	t.Helper()
	n := &hrNet{requests: make(chan *hrAsk, 4)}
	m := newHLRRequests("node-1", &mapprotocol.Config{}, n, n, n, zap.NewNop())
	t.Cleanup(m.cancel)
	return m, n
}

func (n *hrNet) next(t *testing.T) *hrAsk {
	t.Helper()
	select {
	case a := <-n.requests:
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("nothing asked")
		return nil
	}
}

func (n *hrNet) waitSent(t *testing.T, want ...string) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Join(n.sentSoFar(), "|") == strings.Join(want, "|") },
		2*time.Second, 5*time.Millisecond, "sent %v, want %v", n.sentSoFar(), want)
}

func hrDialogueAt(tid uint32, ac tcap.ApplicationContext) *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: tid, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &ac,
		RemoteAddress: &sccp.SccpAddress{GlobalTitle: &sccp.GlobalTitle{Address: "905321000002"}}}
}

func hexRep(b byte, n int) string { return strings.Repeat(fmt.Sprintf("%02x", b), n) }

// quintuplet i, its octets all i.
func quintuplet(i byte) ss7client.AuthenticationQuintuplet {
	return ss7client.AuthenticationQuintuplet{RAND: hexRep(i, 16), XRES: hexRep(i, 8), CK: hexRep(i, 16), IK: hexRep(i, 16), AUTN: hexRep(i, 16)}
}

func triplet(i byte) ss7client.AuthenticationTriplet {
	return ss7client.AuthenticationTriplet{RAND: hexRep(i, 16), SRES: hexRep(i, 4), Kc: hexRep(i, 8)}
}

func (a *hrAsk) vectors(v ss7client.Authentication) {
	a.answer <- luReply{msg: &natsbridge.NatsMessage{Authentication: &v}}
}

// The application gets what the VLR asked; vectors that fit one message go
// in the END.
func TestAuthentication_Answered(t *testing.T) {
	m, n := newHRRig(t)
	d := hrDialogueAt(0x02000001, tcap.ACInfoRetrievalV3)
	node := mapprotocol.NodeSGSN
	arg := &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 3,
		ImmediateResponsePreferred: true, RequestingNodeType: &node, RequestingPLMNID: []byte{0x62, 0xF2, 0x10},
		ResyncRAND: make([]byte, 16), ResyncAUTS: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}}
	require.True(t, m.SendAuthenticationInfo(d, 1, arg))
	assert.True(t, m.HLRRequestActive(d))
	assert.False(t, m.SendAuthenticationInfo(d, 2, arg), "one request per dialogue")

	a := n.next(t)
	r := a.msg.Authentication
	assert.Equal(t, "286010000000123", a.msg.IMSI)
	assert.True(t, strings.HasPrefix(r.DialogueID, m.prefix))
	assert.Equal(t, 3, r.RequestedVectors)
	assert.Equal(t, ss7client.NodeSGSN, r.RequestingNodeType)
	assert.Equal(t, "62f210", r.RequestingPLMNID)
	assert.True(t, r.ImmediateResponsePreferred)
	assert.Equal(t, &ss7client.Resynchronisation{RAND: hexRep(0, 16), AUTS: "0102030405060708090a0b0c0d0e"}, r.Resynchronisation)
	assert.Equal(t, "905321000002", r.CallingGT)
	require.NotNil(t, r.AnswerBy)
	assert.WithinDuration(t, time.Now().Add(mapprotocol.DefaultHLRAnswerTimeout), *r.AnswerBy, time.Second)

	a.vectors(ss7client.Authentication{Triplets: []ss7client.AuthenticationTriplet{triplet(1), triplet(2)}})
	n.waitSent(t, "result id=1 end")
	res, err := mapprotocol.DecodeSendAuthenticationInfoRes(n.params[0])
	require.NoError(t, err)
	require.Len(t, res.Triplets, 2)
	assert.Equal(t, hexRep(2, 8), hex.EncodeToString(res.Triplets[1].Kc))
	assert.False(t, m.HLRRequestActive(d))
	assert.False(t, d.MarkAnswered(), "the dialogue is marked answered")
}

// A request without a node type says so: the HLR's process tells
// "absent" from "vlr" (TS 29.002 figure 25.5/6).
func TestAuthentication_NoNodeType(t *testing.T) {
	m, n := newHRRig(t)
	require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000002, tcap.ACInfoRetrievalV3), 1,
		&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 1}))
	a := n.next(t)
	assert.Empty(t, a.msg.Authentication.RequestingNodeType)
	assert.Nil(t, a.msg.Authentication.Resynchronisation)
	a.vectors(ss7client.Authentication{}) // the empty response
	n.waitSent(t, "result id=1 end")
	assert.Equal(t, []byte{0xA3, 0x00}, n.params[0])
}

// Five quintuplets, two to a message: three parts, the VLR asking for the
// second and third; the UE usage type with the first.
func TestAuthentication_InParts(t *testing.T) {
	m, n := newHRRig(t)
	// Two quintuplets with an 8-octet XRES (84 octets each), the long-form
	// headers of the list and the result (3 each), the UE usage type (6).
	n.fitsUnder = 2*84 + 3 + 3 + 6
	d := hrDialogueAt(0x02000003, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123",
		NumberOfRequestedVectors: 5, UEUsageTypeRequested: true}))
	n.next(t).vectors(ss7client.Authentication{UEUsageType: "00000080", Quintuplets: []ss7client.AuthenticationQuintuplet{
		quintuplet(1), quintuplet(2), quintuplet(3), quintuplet(4), quintuplet(5)}})
	n.waitSent(t, "result id=1 more")
	assert.False(t, m.MoreAuthenticationInfo(hrDialogueAt(0x02000099, tcap.ACInfoRetrievalV3), 2), "another dialogue")
	require.True(t, m.MoreAuthenticationInfo(d, 2))
	n.waitSent(t, "result id=1 more", "result id=2 more")
	require.True(t, m.MoreAuthenticationInfo(d, 3))
	n.waitSent(t, "result id=1 more", "result id=2 more", "result id=3 end")
	var counts []int
	for _, p := range n.params {
		res, err := mapprotocol.DecodeSendAuthenticationInfoRes(p)
		require.NoError(t, err)
		counts = append(counts, len(res.Quintuplets))
	}
	assert.Equal(t, []int{2, 2, 1}, counts)
	assert.False(t, m.MoreAuthenticationInfo(d, 4), "nothing left")

	var got []byte
	for i, p := range n.params {
		res, err := mapprotocol.DecodeSendAuthenticationInfoRes(p)
		require.NoError(t, err)
		assert.Equal(t, i == 0, res.UEUsageType != nil, "the UE usage type in the first part only")
		for _, q := range res.Quintuplets {
			got = append(got, q.RAND[0])
		}
	}
	assert.Equal(t, []byte{1, 2, 3, 4, 5}, got, "every vector once, in order")
}

// With segmentation prohibited, what fits one message goes in the END.
func TestAuthentication_SegmentationProhibited(t *testing.T) {
	m, n := newHRRig(t)
	n.fitsUnder = 2*84 + 3 + 3
	require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000004, tcap.ACInfoRetrievalV3), 1,
		&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 5, SegmentationProhibited: true}))
	n.next(t).vectors(ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{
		quintuplet(1), quintuplet(2), quintuplet(3)}})
	n.waitSent(t, "result id=1 end")
	res, err := mapprotocol.DecodeSendAuthenticationInfoRes(n.params[0])
	require.NoError(t, err)
	assert.Len(t, res.Quintuplets, 2)
}

// The VLR does not ask for the rest within timer m: the dialogue is
// aborted.
func TestAuthentication_VLRDoesNotAskForMore(t *testing.T) {
	m, n := newHRRig(t)
	m.networkTimeout = 50 * time.Millisecond
	n.fitsUnder = 84 + 4
	d := hrDialogueAt(0x02000005, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 2}))
	n.next(t).vectors(ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1), quintuplet(2)}})
	n.waitSent(t, "result id=1 more", "abort")
	assert.False(t, m.HLRRequestActive(d))
}

// The VLR rejects a part: the dialogue is closed at once, not held until
// timer m (figure 25.5/6: a notice, then a close).
func TestAuthentication_VLRRejectsAPart(t *testing.T) {
	m, n := newHRRig(t)
	n.fitsUnder = 84 + 4
	d := hrDialogueAt(0x02000007, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 2}))
	n.next(t).vectors(ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1), quintuplet(2)}})
	n.waitSent(t, "result id=1 more")
	m.HLRRequestRejected(d, "TCAP Reject")
	n.waitSent(t, "result id=1 more", "close")
	assert.False(t, m.HLRRequestActive(d))
}

// Each node type's limits: an MME gets EPS vectors only; a combined
// MME/SGSN the additional ones of the other kind.
func TestAuthentication_VectorLimits(t *testing.T) {
	mme, mmeSGSN := mapprotocol.NodeMME, mapprotocol.NodeMMESGSN
	eps := ss7client.EPSVector{RAND: hexRep(1, 16), XRES: hexRep(1, 8), AUTN: hexRep(1, 16), KASME: hexRep(1, 32)}
	for name, tc := range map[string]struct {
		arg    mapprotocol.SendAuthenticationInfoArg
		answer ss7client.Authentication
		want   string
	}{
		"MME, EPS vectors": {arg: mapprotocol.SendAuthenticationInfoArg{NumberOfRequestedVectors: 1, RequestingNodeType: &mme},
			answer: ss7client.Authentication{EPSVectors: []ss7client.EPSVector{eps}}, want: "result id=1 end"},
		"MME, quintuplets": {arg: mapprotocol.SendAuthenticationInfoArg{NumberOfRequestedVectors: 1, RequestingNodeType: &mme},
			answer: ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1)}}, want: "refuse id=1 34"},
		"VLR, EPS vectors": {arg: mapprotocol.SendAuthenticationInfoArg{NumberOfRequestedVectors: 1},
			answer: ss7client.Authentication{EPSVectors: []ss7client.EPSVector{eps}}, want: "refuse id=1 34"},
		"MME/SGSN, additional for EPS": {arg: mapprotocol.SendAuthenticationInfoArg{NumberOfRequestedVectors: 1, RequestingNodeType: &mmeSGSN,
			AdditionalVectors: 2, AdditionalVectorsAreForEPS: true},
			answer: ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1)}, EPSVectors: []ss7client.EPSVector{eps, eps}},
			want:   "result id=1 end"},
		"MME/SGSN, the main list over its number": {arg: mapprotocol.SendAuthenticationInfoArg{NumberOfRequestedVectors: 1, RequestingNodeType: &mmeSGSN,
			AdditionalVectors: 4, AdditionalVectorsAreForEPS: true},
			answer: ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1), quintuplet(2)}}, want: "refuse id=1 34"},
	} {
		t.Run(name, func(t *testing.T) {
			m, n := newHRRig(t)
			arg := tc.arg
			arg.IMSI = "286010000000123"
			require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000040, tcap.ACInfoRetrievalV3), 1, &arg))
			n.next(t).vectors(tc.answer)
			n.waitSent(t, tc.want)
		})
	}
}

// The VLR ending after the first part, having enough, is a normal end.
func TestAuthentication_VLREndsAfterFirstPart(t *testing.T) {
	m, n := newHRRig(t)
	n.fitsUnder = 84 + 4
	d := hrDialogueAt(0x02000006, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 2}))
	n.next(t).vectors(ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1), quintuplet(2)}})
	n.waitSent(t, "result id=1 more")
	m.HLRRequestEnded(d, false, "ended by the peer")
	assert.False(t, m.HLRRequestActive(d))
	assert.False(t, m.MoreAuthenticationInfo(d, 2))
}

// Answers the gateway cannot carry out, and no answer, are systemFailure.
func TestAuthentication_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		answer *natsbridge.NatsMessage
		err    error
		want   string
	}{
		"unknownSubscriber": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{Error: 1}}, want: "refuse id=1 1"},
		"unknownSubscriber, gprs-eps": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{Error: 1,
			ErrorCause: ss7client.CauseGPRSEPSSubscriptionUnknown}}, want: "refuse id=1 1 gprs-eps-SubscriptionUnknown"},
		"a cause of another error": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{Error: 1,
			ErrorCause: ss7client.CauseOperatorDeterminedBarring}}, want: "refuse id=1 34"},
		"an unknown cause": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{Error: 1,
			ErrorCause: "imsi"}}, want: "refuse id=1 34"},
		"a cause without an error": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{
			ErrorCause: ss7client.CauseIMSIUnknown}}, want: "refuse id=1 34"},
		"an error it does not allow": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{Error: 8}}, want: "refuse id=1 34"},
		"too many vectors": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{
			Triplets: []ss7client.AuthenticationTriplet{triplet(1), triplet(2), triplet(3)}}}, want: "refuse id=1 34"},
		"triplets and quintuplets": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{
			Triplets: []ss7client.AuthenticationTriplet{triplet(1)}, Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1)}}}, want: "refuse id=1 34"},
		"a kc not hex": {answer: &natsbridge.NatsMessage{Authentication: &ss7client.Authentication{
			Triplets: []ss7client.AuthenticationTriplet{{RAND: hexRep(1, 16), SRES: hexRep(1, 4), Kc: "zz"}}}}, want: "refuse id=1 34"},
		"no authentication": {answer: &natsbridge.NatsMessage{}, want: "refuse id=1 34"},
		"no responder":      {err: natsbridge.ErrNoResponder, want: "refuse id=1 34"},
	} {
		t.Run(name, func(t *testing.T) {
			m, n := newHRRig(t)
			require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000010, tcap.ACInfoRetrievalV3), 1,
				&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 2}))
			n.next(t).answer <- luReply{msg: tc.answer, err: tc.err}
			n.waitSent(t, tc.want)
		})
	}
}

func TestAuthentication_NoAnswerInTime(t *testing.T) {
	m, n := newHRRig(t)
	m.answerTimeout = 50 * time.Millisecond
	require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000011, tcap.ACInfoRetrievalV3), 1,
		&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 2}))
	n.next(t)
	n.waitSent(t, "refuse id=1 34")
}

// One vector too long for any message: systemFailure, not a silent drop.
func TestAuthentication_VectorDoesNotFit(t *testing.T) {
	m, n := newHRRig(t)
	n.fitsUnder = 40
	require.True(t, m.SendAuthenticationInfo(hrDialogueAt(0x02000012, tcap.ACInfoRetrievalV3), 1,
		&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 1}))
	n.next(t).vectors(ss7client.Authentication{Quintuplets: []ss7client.AuthenticationQuintuplet{quintuplet(1)}})
	n.waitSent(t, "refuse id=1 34")
}

// The VLR aborts while the application decides: its answer is dropped.
func TestAuthentication_VLRAbortsWhileAsking(t *testing.T) {
	m, n := newHRRig(t)
	d := hrDialogueAt(0x02000013, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 1}))
	a := n.next(t)
	m.HLRRequestEnded(d, true, "dialogue aborted by the peer")
	a.vectors(ss7client.Authentication{Triplets: []ss7client.AuthenticationTriplet{triplet(1)}})
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, n.sentSoFar())
}

func TestPurge(t *testing.T) {
	vlr := mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000002"}
	for name, tc := range map[string]struct {
		answer *natsbridge.NatsMessage
		err    error
		want   string
	}{
		"purged, TMSI frozen": {answer: &natsbridge.NatsMessage{Purge: &ss7client.Purge{FreezeTMSI: true}}, want: "purge id=3 tmsi"},
		"unknownSubscriber":   {answer: &natsbridge.NatsMessage{Purge: &ss7client.Purge{Error: 1}}, want: "refuse id=3 1"},
		"unknownSubscriber, imsiUnknown": {answer: &natsbridge.NatsMessage{Purge: &ss7client.Purge{Error: 1,
			ErrorCause: ss7client.CauseIMSIUnknown}}, want: "refuse id=3 1 imsiUnknown"},
		"a cause of another error": {answer: &natsbridge.NatsMessage{Purge: &ss7client.Purge{Error: 1,
			ErrorCause: ss7client.CauseSupportedRATTypesNotAllowed}}, want: "abort"},
		// purgeMS has no systemFailure: what the gateway cannot answer is
		// aborted.
		"systemFailure":     {answer: &natsbridge.NatsMessage{Purge: &ss7client.Purge{Error: 34}}, want: "abort"},
		"no purge":          {answer: &natsbridge.NatsMessage{}, want: "abort"},
		"no responder":      {err: natsbridge.ErrNoResponder, want: "abort"},
		"the request fails": {err: errors.New("nats: connection closed"), want: "abort"},
	} {
		t.Run(name, func(t *testing.T) {
			m, n := newHRRig(t)
			d := hrDialogueAt(0x02000020, tcap.ACMSPurgingV3)
			require.True(t, m.PurgeMS(d, 3, &mapprotocol.PurgeMSArg{IMSI: "286010000000123", VLRNumber: &vlr}))
			assert.False(t, m.PurgeMS(d, 4, &mapprotocol.PurgeMSArg{IMSI: "286010000000123"}), "one request per dialogue")
			a := n.next(t)
			p := a.msg.Purge
			assert.Equal(t, "286010000000123", a.msg.IMSI)
			assert.Equal(t, "905321000002", p.VLRNumber.Digits)
			assert.Nil(t, p.SGSNNumber)
			require.NotNil(t, p.AnswerBy)
			a.answer <- luReply{msg: tc.answer, err: tc.err}
			n.waitSent(t, tc.want)
			assert.False(t, m.HLRRequestActive(d))
		})
	}
}

func TestHLRRequests_LostAndShutdown(t *testing.T) {
	m, n := newHRRig(t)
	lost, open := hrDialogueAt(0x02000030, tcap.ACInfoRetrievalV3), hrDialogueAt(0x02000031, tcap.ACMSPurgingV3)
	require.True(t, m.SendAuthenticationInfo(lost, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 1}))
	require.True(t, m.PurgeMS(open, 1, &mapprotocol.PurgeMSArg{IMSI: "286010000000124"}))
	n.next(t)
	n.next(t)
	m.lost(lost, tcap.FailureTimeout, "dialogue timer")
	assert.False(t, m.HLRRequestActive(lost))
	m.close()
	n.waitSent(t, "abort")
	assert.False(t, m.HLRRequestActive(open))

	d := hrDialogueAt(0x02000032, tcap.ACInfoRetrievalV3)
	require.True(t, m.SendAuthenticationInfo(d, 1, &mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 1}))
	n.waitSent(t, "abort", "refuse id=1 34")
}
