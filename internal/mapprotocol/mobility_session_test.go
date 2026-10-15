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

type luEvent struct {
	kind    string
	id      int32
	arg     *mapprotocol.UpdateLocationArg
	res     *mapprotocol.InsertSubscriberDataRes
	code    mapprotocol.ErrorCode
	aborted bool
	ended   bool
}

type recordingLU struct {
	events []luEvent
	refuse bool
	active bool
}

func (r *recordingLU) UpdateLocation(_ *tcap.Dialogue, id int32, arg *mapprotocol.UpdateLocationArg) bool {
	r.events = append(r.events, luEvent{kind: "update", id: id, arg: arg})
	return !r.refuse
}
func (r *recordingLU) InsertSubscriberDataResult(_ *tcap.Dialogue, id int32, res *mapprotocol.InsertSubscriberDataRes, ended bool) {
	r.events = append(r.events, luEvent{kind: "result", id: id, res: res, ended: ended})
}
func (r *recordingLU) InsertSubscriberDataError(_ *tcap.Dialogue, id int32, code mapprotocol.ErrorCode, _ string, ended bool) {
	r.events = append(r.events, luEvent{kind: "error", id: id, code: code, ended: ended})
}
func (r *recordingLU) LocationUpdateEnded(_ *tcap.Dialogue, aborted bool, _ string) {
	r.events = append(r.events, luEvent{kind: "ended", aborted: aborted})
}
func (r *recordingLU) LocationUpdateActive(*tcap.Dialogue) bool { return r.active }

func luDialogue() *tcap.Dialogue {
	return &tcap.Dialogue{LocalTID: 0x01020304, RemoteTID: []byte{9, 9, 9, 9}, ApplicationContext: &tcap.ACNetworkLocUpV3}
}

func ulArg(t *testing.T) []byte {
	t.Helper()
	b, err := mapprotocol.EncodeUpdateLocationArg(&mapprotocol.UpdateLocationArg{IMSI: "286010000000123",
		MSCNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000001"},
		VLRNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905321000002"}})
	require.NoError(t, err)
	return b
}

// A location update as a VLR runs it: BEGIN with updateLocation; the
// results and errors of the gateway's insertSubscriberData in CONTINUEs;
// the END.
func TestLocationUpdateComponents(t *testing.T) {
	h := newTestHandler(t)
	h.SetTCAPOutbound(make(chan *tcap.Message, 4))
	rec := &recordingLU{}
	h.SetLocationUpdateHandler(rec)
	d := luDialogue()
	op2 := int32(mapprotocol.OpUpdateLocation)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeBegin,
		Components: []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32p(5), OperationCode: &op2, Parameter: ulArg(t)}}}))
	res, err := mapprotocol.EncodeInsertSubscriberDataRes(&mapprotocol.InsertSubscriberDataRes{SSCodes: []byte{0x12}})
	require.NoError(t, err)
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeContinue,
		Components: []*tcap.Component{
			{Type: tcap.CompTypeReturnResult, InvokeID: i32p(1), OperationCode: i32p(7), Parameter: res},
			{Type: tcap.CompTypeReturnResult, InvokeID: i32p(2)}, // the result is optional
			{Type: tcap.CompTypeReturnError, InvokeID: i32p(3), ErrorCode: i32p(36)},
			{Type: tcap.CompTypeReturnResult, InvokeID: i32p(4), Parameter: []byte{0x04, 0x00}},
		}}))
	require.NoError(t, h.HandleTCAPMessage(d, &tcap.Message{MessageType: tcap.MsgTypeEnd}))

	require.Len(t, rec.events, 6)
	assert.Equal(t, luEvent{kind: "update", id: 5, arg: rec.events[0].arg}, rec.events[0])
	assert.Equal(t, mapprotocol.IMSI("286010000000123"), rec.events[0].arg.IMSI)
	assert.Equal(t, []byte{0x12}, rec.events[1].res.SSCodes)
	assert.Equal(t, luEvent{kind: "result", id: 2, res: &mapprotocol.InsertSubscriberDataRes{}}, rec.events[2])
	assert.Equal(t, luEvent{kind: "error", id: 3, code: mapprotocol.ErrUnexpectedDataValue}, rec.events[3])
	assert.Equal(t, luEvent{kind: "error", id: 4}, rec.events[4], "an undecodable result is an error without a code")
	assert.Equal(t, luEvent{kind: "ended"}, rec.events[5])
	assert.Equal(t, uint64(1), h.GetMetrics().UpdateLocationCount.Load())
}

// What the gateway does not serve in the context is rejected: in an END
// when the dialogue holds nothing, in a CONTINUE when a location update
// lives in it.
func TestLocationUpdateRejects(t *testing.T) {
	op2, op57 := int32(mapprotocol.OpUpdateLocation), int32(57) // restoreData: in the context, not served
	for name, tc := range map[string]struct {
		msg      *tcap.Message
		active   bool
		refuse   bool
		wantType tcap.MessageType
		problem  uint8
	}{
		"mistyped updateLocation": {msg: &tcap.Message{MessageType: tcap.MsgTypeBegin, Components: []*tcap.Component{
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op2, Parameter: []byte{0x30, 0x00}}}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemMistypedParameter},
		"restoreData": {msg: &tcap.Message{MessageType: tcap.MsgTypeBegin, Components: []*tcap.Component{
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op57}}},
			wantType: tcap.MsgTypeEnd, problem: tcap.InvokeProblemUnrecognizedOperation},
		"a second updateLocation": {msg: &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(7), OperationCode: &op2, Parameter: ulArg(t)}}},
			active: true, refuse: true, wantType: tcap.MsgTypeContinue, problem: tcap.InvokeProblemUnrecognizedOperation},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t)
			out := make(chan *tcap.Message, 2)
			h.SetTCAPOutbound(out)
			h.SetLocationUpdateHandler(&recordingLU{active: tc.active, refuse: tc.refuse})
			require.NoError(t, h.HandleTCAPMessage(luDialogue(), tc.msg))
			require.Len(t, out, 1)
			m := <-out
			assert.Equal(t, tc.wantType, m.MessageType)
			require.Len(t, m.Components, 1)
			assert.Equal(t, tcap.CompTypeReject, m.Components[0].Type)
			assert.Equal(t, tc.problem, m.Components[0].ProblemCode.Code)
		})
	}
}

func TestLocationUpdateSenders(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 4)
	h.SetTCAPOutbound(out)
	d := luDialogue()

	isd := h.InsertSubscriberDataMessage(d, 1, []byte{0x30, 0x00})
	assert.Equal(t, tcap.MsgTypeContinue, isd.MessageType)
	assert.Equal(t, []byte{1, 2, 3, 4}, isd.OriginatingTID)
	assert.Equal(t, []byte{9, 9, 9, 9}, isd.DestinationTID)
	assert.Equal(t, int32(7), *isd.Components[0].OperationCode)
	require.NoError(t, h.Send(d, isd))

	require.NoError(t, h.UpdateLocationAccept(d, 5, mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905309999999"}))
	<-out
	end := <-out
	assert.Equal(t, tcap.MsgTypeEnd, end.MessageType)
	c := end.Components[0]
	assert.Equal(t, tcap.CompTypeReturnResult, c.Type)
	assert.Equal(t, int32(5), *c.InvokeID)
	assert.Equal(t, int32(2), *c.OperationCode)
	assert.Equal(t, []byte{0x30, 0x09, 0x04, 0x07, 0x91, 0x09, 0x35, 0x90, 0x99, 0x99, 0x99}, c.Parameter)

	require.NoError(t, h.UpdateLocationRefuse(d, 5, mapprotocol.ErrRoamingNotAllowed))
	assert.Equal(t, int32(8), *(<-out).Components[0].ErrorCode)
	assert.Error(t, h.UpdateLocationRefuse(d, 5, mapprotocol.ErrCallBarred), "callBarred is not one of updateLocation's errors")

	require.NoError(t, h.LocationUpdateAbort(d))
	assert.Equal(t, tcap.MsgTypeAbort, (<-out).MessageType)
}

// A BEGIN with updateLocation and a second Invoke: the second is rejected in
// a CONTINUE, which leaves the location update alive; with the other
// Invoke first, the rejection ends the dialogue and nothing after it runs.
func TestLocationUpdateTwoInvokesInBegin(t *testing.T) {
	op2, op57 := int32(mapprotocol.OpUpdateLocation), int32(57)
	for name, tc := range map[string]struct {
		comps   []*tcap.Component
		want    tcap.MessageType
		updates int
	}{
		"updateLocation first": {[]*tcap.Component{
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op2, Parameter: ulArg(t)},
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(2), OperationCode: &op57}}, tcap.MsgTypeContinue, 1},
		"the other first": {[]*tcap.Component{
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(2), OperationCode: &op57},
			{Type: tcap.CompTypeInvoke, InvokeID: i32p(1), OperationCode: &op2, Parameter: ulArg(t)}}, tcap.MsgTypeEnd, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t)
			out := make(chan *tcap.Message, 2)
			h.SetTCAPOutbound(out)
			rec := &activeAfterUpdate{}
			h.SetLocationUpdateHandler(rec)
			require.NoError(t, h.HandleTCAPMessage(luDialogue(), &tcap.Message{MessageType: tcap.MsgTypeBegin, Components: tc.comps}))
			require.Len(t, out, 1)
			assert.Equal(t, tc.want, (<-out).MessageType)
			assert.Equal(t, tc.updates, rec.updates)
		})
	}
}

// activeAfterUpdate is a handler whose dialogue is active once it took an
// updateLocation.
type activeAfterUpdate struct {
	recordingLU
	updates int
}

func (a *activeAfterUpdate) UpdateLocation(*tcap.Dialogue, int32, *mapprotocol.UpdateLocationArg) bool {
	a.updates++
	return true
}
func (a *activeAfterUpdate) LocationUpdateActive(*tcap.Dialogue) bool { return a.updates > 0 }
