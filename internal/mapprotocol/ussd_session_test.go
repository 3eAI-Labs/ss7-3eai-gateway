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

type ussdEvent struct {
	kind    string
	id      int32
	arg     *mapprotocol.USSDArg
	open    *mapprotocol.MAPOpenInfo
	code    mapprotocol.ErrorCode
	aborted bool
	ended   bool
}

type recordingUSSD struct {
	events []ussdEvent
	refuse bool // USSDBegin answers false
	active bool // USSDActive
}

func (r *recordingUSSD) USSDBegin(_ *tcap.Dialogue, id int32, arg *mapprotocol.USSDArg, open *mapprotocol.MAPOpenInfo) bool {
	r.events = append(r.events, ussdEvent{kind: "begin", id: id, arg: arg, open: open})
	return !r.refuse
}
func (r *recordingUSSD) USSDAnswer(_ *tcap.Dialogue, id int32, res *mapprotocol.USSDArg) {
	r.events = append(r.events, ussdEvent{kind: "answer", id: id, arg: res})
}
func (r *recordingUSSD) USSDError(_ *tcap.Dialogue, id int32, code mapprotocol.ErrorCode, _ string, ended bool) {
	r.events = append(r.events, ussdEvent{kind: "error", id: id, code: code, ended: ended})
}
func (r *recordingUSSD) USSDActive(*tcap.Dialogue) bool { return r.active }
func (r *recordingUSSD) USSDEnded(_ *tcap.Dialogue, aborted bool, _ string) {
	r.events = append(r.events, ussdEvent{kind: "ended", aborted: aborted})
}

func ussdDialogue() *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: 0x01020304, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &tcap.ACNetworkUnstructuredSsV2}
}

func i32p(v int32) *int32 { return &v }

// A subscriber-initiated session as the HLR runs it: BEGIN with
// processUnstructuredSS-Request and MAP-OpenInfo; the answer to the
// gateway's prompt in a CONTINUE; a Reject; the END.
func TestUSSDSessionComponents(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	rec := &recordingUSSD{}
	h.SetUSSDSessionHandler(rec)
	d := ussdDialogue()

	msisdn := &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: "905321112233"}
	argBytes, err := mapprotocol.EncodeUSSDArg(&mapprotocol.USSDArg{DCS: 0x0F, String: []byte{0xAA, 0x98, 0x6C, 0x36, 0x02}, MSISDN: msisdn})
	require.NoError(t, err)
	open, err := mapprotocol.EncodeMAPOpenInfo(&mapprotocol.MAPOpenInfo{DestinationReference: &mapprotocol.AddressString{
		Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanLandMobile, Digits: "286010000000123"}})
	require.NoError(t, err)
	op59 := int32(mapprotocol.OpProcessUnstructuredSSReq)
	d.OpeningUserInformation = open // TCAP keeps the AARQ's user-information
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeBegin,
		DialoguePortion: &tcap.DialoguePortion{ApplicationContext: &tcap.ACNetworkUnstructuredSsV2, UserInformation: open},
		Components:      []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op59, Parameter: argBytes}}}))

	res, err := mapprotocol.EncodeUSSDRes(0x0F, []byte{0x31})
	require.NoError(t, err)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeContinue,
		Components: []*tcap.Component{
			{Type: tcap.CompTypeReturnResult, InvokeID: i32p(2), OperationCode: i32p(60), Parameter: res},
			{Type: tcap.CompTypeReturnResult, InvokeID: i32p(3)}, // a result without parameter
			{Type: tcap.CompTypeReturnError, InvokeID: i32p(4), ErrorCode: i32p(72)},
		}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeAbort}))

	require.Len(t, rec.events, 5)
	b := rec.events[0]
	assert.Equal(t, "begin", b.kind)
	assert.Equal(t, int32(1), b.id)
	assert.Equal(t, "905321112233", b.arg.MSISDN.Digits)
	require.NotNil(t, b.open)
	assert.Equal(t, "286010000000123", b.open.DestinationReference.Digits)
	assert.Equal(t, mapprotocol.NumberingPlanLandMobile, b.open.DestinationReference.NumberPlan)
	assert.Equal(t, ussdEvent{kind: "answer", id: 2, arg: &mapprotocol.USSDArg{DCS: 0x0F, String: []byte{0x31}}}, rec.events[1])
	assert.Equal(t, ussdEvent{kind: "answer", id: 3}, rec.events[2])
	assert.Equal(t, ussdEvent{kind: "error", id: 4, code: mapprotocol.ErrUSSDBusy}, rec.events[3])
	assert.Equal(t, ussdEvent{kind: "ended", aborted: true}, rec.events[4])
	assert.Empty(t, out, "nothing is answered for the session handler")
}

// The HLR sends the gsmSCF no unstructuredSS-Request: refused with a Reject.
func TestUSSDSessionRefusesOtherInvokes(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	rec := &recordingUSSD{}
	h.SetUSSDSessionHandler(rec)
	op60 := int32(mapprotocol.OpUnstructuredSSRequest)
	require.NoError(t, h.HandleTCAPMessage(ussdDialogue(), &tcap.Message{MessageType: tcap.MsgTypeBegin,
		Components: []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op60, Parameter: []byte{0x30, 0x00}}}}))
	assert.Empty(t, rec.events)
	m := <-out
	require.Len(t, m.Components, 1)
	assert.Equal(t, tcap.CompTypeReject, m.Components[0].Type)
}

// The senders: a prompt in a CONTINUE from our TID to the HLR's; the final
// result and a basic END.
func TestUSSDSenders(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	d := ussdDialogue()
	require.NoError(t, h.USSDInvoke(d, 2, mapprotocol.OpUnstructuredSSRequest, &mapprotocol.USSDArg{DCS: 0x0F, String: []byte{0x31}}))
	m := <-out
	assert.Equal(t, tcap.MsgTypeContinue, m.MessageType)
	assert.Equal(t, []byte{1, 2, 3, 4}, m.OriginatingTID)
	assert.Equal(t, []byte{9, 9, 9, 9}, m.DestinationTID)
	assert.Equal(t, int32(60), *m.Components[0].OperationCode)

	require.NoError(t, h.USSDRelease(d, 1, 0x0F, []byte{0x31}))
	m = <-out
	assert.Equal(t, tcap.MsgTypeEnd, m.MessageType)
	assert.Equal(t, tcap.CompTypeReturnResult, m.Components[0].Type)
	assert.Equal(t, int32(59), *m.Components[0].OperationCode)

	require.NoError(t, h.USSDClose(d))
	assert.Empty(t, (<-out).Components)
	assert.Error(t, h.USSDInvoke(d, 3, mapprotocol.OpProcessUnstructuredSSReq, &mapprotocol.USSDArg{DCS: 0x0F, String: []byte{1}}))
	assert.Error(t, h.USSDClose(&tcap.Dialogue{}), "no peer TID yet")
}

// After an empty Begin (TS 29.002 §C.2.1) processUnstructuredSS-Request
// comes in the CONTINUE; its MAP-OpenInfo was in the BEGIN's AARQ. A
// second one in the same dialogue, refused by the session handler, is
// rejected.
func TestUSSDBeginAfterEmptyBegin(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	rec := &recordingUSSD{}
	h.SetUSSDSessionHandler(rec)
	d := ussdDialogue()
	open, err := mapprotocol.EncodeMAPOpenInfo(&mapprotocol.MAPOpenInfo{DestinationReference: &mapprotocol.AddressString{
		Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: "905321112233"}})
	require.NoError(t, err)
	d.OpeningUserInformation = open
	arg, err := mapprotocol.EncodeUSSDArg(&mapprotocol.USSDArg{DCS: 0x0F, String: []byte{0xAA, 0x98, 0x6C, 0x36, 0x02}})
	require.NoError(t, err)
	op59 := int32(mapprotocol.OpProcessUnstructuredSSReq)
	cont := &tcap.Message{MessageType: tcap.MsgTypeContinue,
		Components: []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op59, Parameter: arg}}}
	require.NoError(t, h.HandleTCAPMessage(d, cont))
	require.Len(t, rec.events, 1)
	assert.Equal(t, "905321112233", rec.events[0].open.DestinationReference.Digits)
	assert.Empty(t, out)

	rec.refuse, rec.active = true, true
	require.NoError(t, h.HandleTCAPMessage(d, cont))
	m := <-out
	require.Len(t, m.Components, 1)
	assert.Equal(t, tcap.CompTypeReject, m.Components[0].Type)
	assert.Equal(t, tcap.MsgTypeContinue, m.MessageType, "the session lives on: the Reject does not end the dialogue")
	assert.Equal(t, []byte{1, 2, 3, 4}, m.OriginatingTID)

	// An error in an END says the dialogue ends with it.
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeEnd,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnError, InvokeID: i32p(2), ErrorCode: i32p(34)}}}))
	assert.Equal(t, ussdEvent{kind: "error", id: 2, code: mapprotocol.ErrSystemFailure, ended: true}, rec.events[2])
	assert.Equal(t, ussdEvent{kind: "ended"}, rec.events[3])
}
