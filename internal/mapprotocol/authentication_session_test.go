// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol_test

import (
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hrEvent struct {
	kind    string
	id      int32
	sai     *mapprotocol.SendAuthenticationInfoArg
	purge   *mapprotocol.PurgeMSArg
	aborted bool
}

// recordingHR records the calls; its dialogue is active once it took a
// request, and it takes a later request when more is set.
type recordingHR struct {
	events []hrEvent
	active bool
	more   bool
}

func (r *recordingHR) SendAuthenticationInfo(_ *tcap.Dialogue, id int32, arg *mapprotocol.SendAuthenticationInfoArg) bool {
	r.events = append(r.events, hrEvent{kind: "sai", id: id, sai: arg})
	ok := !r.active
	r.active = true
	return ok
}
func (r *recordingHR) MoreAuthenticationInfo(_ *tcap.Dialogue, id int32) bool {
	r.events = append(r.events, hrEvent{kind: "more", id: id})
	return r.more
}
func (r *recordingHR) PurgeMS(_ *tcap.Dialogue, id int32, arg *mapprotocol.PurgeMSArg) bool {
	r.events = append(r.events, hrEvent{kind: "purge", id: id, purge: arg})
	ok := !r.active
	r.active = true
	return ok
}
func (r *recordingHR) HLRRequestEnded(_ *tcap.Dialogue, aborted bool, _ string) {
	r.events = append(r.events, hrEvent{kind: "ended", aborted: aborted})
}
func (r *recordingHR) HLRRequestRejected(*tcap.Dialogue, string) {
	r.events = append(r.events, hrEvent{kind: "rejected"})
}
func (r *recordingHR) HLRRequestActive(*tcap.Dialogue) bool { return r.active }

// endsOnMore is a handler whose dialogue ends with the first request for
// more: its answer was the last part.
type endsOnMore struct{ recordingHR }

func (e *endsOnMore) MoreAuthenticationInfo(d *tcap.Dialogue, id int32) bool {
	e.recordingHR.MoreAuthenticationInfo(d, id)
	e.active = false
	return true
}

// The VLR rejects a part: the handler is told. An Invoke in the VLR's END
// is not answered. Components after the gateway's END are dropped.
func TestHLRRequestPeerRejectAndEnd(t *testing.T) {
	sai := int32(mapprotocol.OpSendAuthenticationInfo)
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 2)
	h.SetTCAPOutbound(out)
	rec := &recordingHR{active: true}
	h.SetHLRRequestHandler(rec)
	d := hrDialogue(tcap.ACInfoRetrievalV3)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{
		{Type: tcap.CompTypeReject, InvokeID: i32p(1), ProblemCode: &tcap.ProblemCode{Type: tcap.ProblemTypeReturnResult, Code: 2}}}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeEnd, Components: []*tcap.Component{invoke(2, sai, nil)}}))
	assert.Equal(t, []hrEvent{{kind: "rejected"}, {kind: "ended"}}, rec.events)
	assert.Empty(t, out, "nothing answers an Invoke in an END")

	more := &endsOnMore{recordingHR{active: true}}
	h.SetHLRRequestHandler(more)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeContinue,
		Components: []*tcap.Component{invoke(3, sai, nil), invoke(4, sai, nil)}}))
	assert.Len(t, more.events, 1, "the second request comes after the gateway's last part")
	assert.Empty(t, out, "and is not answered with a second END")
}

func hrDialogue(ac tcap.ApplicationContext) *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: 0x01020304, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &ac}
}

func saiArg(t *testing.T) []byte {
	t.Helper()
	b, err := mapprotocol.EncodeSendAuthenticationInfoArg(&mapprotocol.SendAuthenticationInfoArg{IMSI: "286010000000123", NumberOfRequestedVectors: 5})
	require.NoError(t, err)
	return b
}

func invoke(id, op int32, param []byte) *tcap.Component {
	return &tcap.Component{Type: tcap.CompTypeInvoke, InvokeID: &id, OperationCode: &op, Parameter: param}
}

// An authentication dialogue as a VLR runs it with MAP segmentation: the
// BEGIN with the request, a CONTINUE asking for more (its argument, which
// it should not carry, discarded), the END.
func TestAuthenticationDialogueComponents(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	rec := &recordingHR{more: true}
	h.SetHLRRequestHandler(rec)
	d := hrDialogue(tcap.ACInfoRetrievalV3)
	op := int32(mapprotocol.OpSendAuthenticationInfo)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeBegin,
		Components: []*tcap.Component{invoke(1, op, saiArg(t))}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeContinue,
		Components: []*tcap.Component{invoke(2, op, saiArg(t))}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeEnd}))

	require.Len(t, rec.events, 3)
	assert.Equal(t, "sai", rec.events[0].kind)
	assert.Equal(t, 5, rec.events[0].sai.NumberOfRequestedVectors)
	assert.Equal(t, hrEvent{kind: "more", id: 2}, rec.events[1])
	assert.Equal(t, hrEvent{kind: "ended"}, rec.events[2])
	assert.Empty(t, out, "nothing is sent by the MAP layer itself")
	assert.Equal(t, uint64(2), h.GetMetrics().SendAuthenticationInfoCount.Load())
}

func TestPurgeDialogueComponents(t *testing.T) {
	h := newTestHandler(t)
	h.SetTCAPOutbound(make(chan *tcap.Message, 1))
	rec := &recordingHR{}
	h.SetHLRRequestHandler(rec)
	vlr := mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000002"}
	param, err := mapprotocol.EncodePurgeMSArg(&mapprotocol.PurgeMSArg{IMSI: "286010000000123", VLRNumber: &vlr})
	require.NoError(t, err)
	d := hrDialogue(tcap.ACMSPurgingV3)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeBegin,
		Components: []*tcap.Component{invoke(3, int32(mapprotocol.OpPurgeMS), param)}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeAbort}))
	require.Len(t, rec.events, 2)
	assert.Equal(t, hrEvent{kind: "purge", id: 3, purge: &mapprotocol.PurgeMSArg{IMSI: "286010000000123", VLRNumber: &vlr}}, rec.events[0])
	assert.Equal(t, hrEvent{kind: "ended", aborted: true}, rec.events[1])
	assert.Equal(t, uint64(1), h.GetMetrics().PurgeMSCount.Load())
}

// What the gateway does not serve is rejected: in an END when the dialogue
// holds nothing, in a CONTINUE when a request lives in it. A first
// sendAuthenticationInfo without argument is dataMissing.
func TestHLRRequestRejects(t *testing.T) {
	sai, purge := int32(mapprotocol.OpSendAuthenticationInfo), int32(mapprotocol.OpPurgeMS)
	for name, tc := range map[string]struct {
		ac       tcap.ApplicationContext
		msg      *tcap.Message
		rec      recordingHR
		wantType tcap.MessageType
		problem  uint8
		error    int32
	}{
		"purgeMS under infoRetrieval": {ac: tcap.ACInfoRetrievalV3, msg: &tcap.Message{MessageType: tcap.MsgTypeBegin,
			Components: []*tcap.Component{invoke(1, purge, []byte{0xA3, 0x00})}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemUnrecognizedOperation},
		"sendAuthenticationInfo under msPurging": {ac: tcap.ACMSPurgingV3, msg: &tcap.Message{MessageType: tcap.MsgTypeBegin,
			Components: []*tcap.Component{invoke(1, sai, saiArg(t))}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemUnrecognizedOperation},
		"mistyped sendAuthenticationInfo": {ac: tcap.ACInfoRetrievalV3, msg: &tcap.Message{MessageType: tcap.MsgTypeBegin,
			Components: []*tcap.Component{invoke(1, sai, []byte{0x30, 0x00})}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemMistypedParameter},
		"sendAuthenticationInfo without argument": {ac: tcap.ACInfoRetrievalV3, msg: &tcap.Message{MessageType: tcap.MsgTypeBegin,
			Components: []*tcap.Component{invoke(1, sai, nil)}},
			wantType: tcap.MsgTypeEnd, error: int32(mapprotocol.ErrDataMissing)},
		"purgeMS without argument": {ac: tcap.ACMSPurgingV3, msg: &tcap.Message{MessageType: tcap.MsgTypeBegin,
			Components: []*tcap.Component{invoke(1, purge, nil)}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemMistypedParameter},
		"a request for more with nothing waiting": {ac: tcap.ACInfoRetrievalV3, rec: recordingHR{active: true},
			msg:      &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{invoke(2, sai, nil)}},
			wantType: tcap.MsgTypeContinue, problem: tcap.InvokeProblemUnrecognizedOperation},
		"a second purgeMS": {ac: tcap.ACMSPurgingV3, rec: recordingHR{active: true},
			msg:      &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{invoke(2, purge, []byte{0xA3, 0x00})}},
			wantType: tcap.MsgTypeContinue, problem: tcap.InvokeProblemUnrecognizedOperation},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t)
			out := make(chan *tcap.Message, 2)
			h.SetTCAPOutbound(out)
			rec := tc.rec
			h.SetHLRRequestHandler(&rec)
			require.NoError(t, h.HandleTCAPMessage(hrDialogue(tc.ac), tc.msg))
			require.Len(t, out, 1)
			m := <-out
			assert.Equal(t, tc.wantType, m.MessageType)
			require.Len(t, m.Components, 1)
			if tc.error != 0 {
				assert.Equal(t, tcap.CompTypeReturnError, m.Components[0].Type)
				assert.Equal(t, tc.error, *m.Components[0].ErrorCode)
				return
			}
			assert.Equal(t, tcap.CompTypeReject, m.Components[0].Type)
			assert.Equal(t, tc.problem, m.Components[0].ProblemCode.Code)
		})
	}
}

// A BEGIN with sendAuthenticationInfo and a second Invoke: the second is
// rejected in a CONTINUE, which leaves the request alive.
func TestHLRRequestSecondInvokeInBegin(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 2)
	h.SetTCAPOutbound(out)
	rec := &recordingHR{}
	h.SetHLRRequestHandler(rec)
	require.NoError(t, h.HandleTCAPMessage(hrDialogue(tcap.ACInfoRetrievalV3), &tcap.Message{MessageType: tcap.MsgTypeBegin,
		Components: []*tcap.Component{invoke(1, int32(mapprotocol.OpSendAuthenticationInfo), saiArg(t)), invoke(2, 57, nil)}}))
	require.Len(t, out, 1)
	assert.Equal(t, tcap.MsgTypeContinue, (<-out).MessageType)
	require.Len(t, rec.events, 1)
}

func TestHLRRequestSenders(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	auth, purge := hrDialogue(tcap.ACInfoRetrievalV3), hrDialogue(tcap.ACMSPurgingV3)

	more := h.AuthenticationInfoMessage(auth, 1, []byte{0xA3, 0x00}, true)
	assert.Equal(t, tcap.MsgTypeContinue, more.MessageType)
	assert.Equal(t, []byte{1, 2, 3, 4}, more.OriginatingTID)
	assert.Equal(t, []byte{9, 9, 9, 9}, more.DestinationTID)
	c := more.Components[0]
	assert.Equal(t, tcap.CompTypeReturnResult, c.Type)
	assert.False(t, c.NotLast, "each part is a ReturnResultLast (TS 29.002 Annex C.3.3)")
	assert.Equal(t, int32(56), *c.OperationCode)
	last := h.AuthenticationInfoMessage(auth, 2, []byte{0xA3, 0x00}, false)
	assert.Equal(t, tcap.MsgTypeEnd, last.MessageType)
	assert.Nil(t, last.OriginatingTID)

	require.NoError(t, h.HLRRequestRefuse(auth, 1, mapprotocol.UserError{Code: mapprotocol.ErrSystemFailure}))
	assert.Equal(t, int32(34), *(<-out).Components[0].ErrorCode)
	assert.Error(t, h.HLRRequestRefuse(purge, 1, mapprotocol.UserError{Code: mapprotocol.ErrSystemFailure}), "purgeMS has no systemFailure")
	require.NoError(t, h.HLRRequestRefuse(purge, 1, mapprotocol.UserError{Code: mapprotocol.ErrUnknownSubscriber}))
	<-out

	require.NoError(t, h.PurgeMSAccept(purge, 7, &mapprotocol.PurgeMSRes{FreezeTMSI: true}))
	end := <-out
	assert.Equal(t, tcap.MsgTypeEnd, end.MessageType)
	assert.Equal(t, int32(67), *end.Components[0].OperationCode)
	assert.Equal(t, []byte{0x30, 0x02, 0x80, 0x00}, end.Components[0].Parameter)

	require.NoError(t, h.Abort(purge))
	assert.Equal(t, tcap.MsgTypeAbort, (<-out).MessageType)
}
