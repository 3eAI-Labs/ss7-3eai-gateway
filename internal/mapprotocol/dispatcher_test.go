// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol_test

import (
	"errors"
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Compile-time assertion: *mapprotocol.Handler satisfies tcap.AppContextHandler.
var _ tcap.AppContextHandler = (*mapprotocol.Handler)(nil)

func newTestHandler(t *testing.T) *mapprotocol.Handler {
	t.Helper()
	cfg := &mapprotocol.Config{
		HLRAddress:   "127.0.0.1",
		HLRPointCode: 1234,
	}
	h, err := mapprotocol.NewHandler(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func invokeMsg(opCode mapprotocol.OperationCode, ac []int) *tcap.Message {
	op := int32(opCode)
	invID := int32(1)
	return &tcap.Message{
		MessageType: tcap.MsgTypeBegin,
		DialoguePortion: &tcap.DialoguePortion{
			ApplicationContext: &tcap.ApplicationContext{OID: ac},
		},
		Components: []*tcap.Component{
			{
				Type:          tcap.CompTypeInvoke,
				InvokeID:      &invID,
				OperationCode: &op,
			},
		},
	}
}

func TestMap_Name(t *testing.T) {
	h := newTestHandler(t)
	if got := h.Name(); got != "map" {
		t.Fatalf("Name() = %q, want %q", got, "map")
	}
}

func TestMap_Dispatch_UpdateLocation(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACMapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dlg := &tcap.Dialogue{LocalTID: 0x01000001}
	if err := d.Dispatch(dlg, invokeMsg(mapprotocol.OpUpdateLocation, tcap.ACMapV3.OID)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := h.GetMetrics().UpdateLocationCount.Load(); got != 1 {
		t.Fatalf("UpdateLocationCount = %d, want 1", got)
	}
	if got := h.GetMetrics().OperationsReceived.Load(); got != 1 {
		t.Fatalf("OperationsReceived = %d, want 1", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 1 {
		t.Fatalf("Dispatcher.Dispatches = %d, want 1", got)
	}
}

func TestMap_Dispatch_PerOpCounters(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACMapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := []struct {
		op     mapprotocol.OperationCode
		assert func(*mapprotocol.Metrics) uint64
	}{
		{mapprotocol.OpUpdateLocation, func(m *mapprotocol.Metrics) uint64 { return m.UpdateLocationCount.Load() }},
		{mapprotocol.OpCancelLocation, func(m *mapprotocol.Metrics) uint64 { return m.CancelLocationCount.Load() }},
		{mapprotocol.OpSendRoutingInfo, func(m *mapprotocol.Metrics) uint64 { return m.SendRoutingInfoCount.Load() }},
		{mapprotocol.OpProvideRoamingNumber, func(m *mapprotocol.Metrics) uint64 { return m.ProvideRoamingNumberCount.Load() }},
		{mapprotocol.OpAnyTimeInterrogation, func(m *mapprotocol.Metrics) uint64 { return m.AnyTimeInterrogationCount.Load() }},
	}

	for _, tc := range cases {
		before := tc.assert(h.GetMetrics())
		if err := d.Dispatch(nil, invokeMsg(tc.op, tcap.ACMapV3.OID)); err != nil {
			t.Fatalf("Dispatch op=%d: %v", tc.op, err)
		}
		if after := tc.assert(h.GetMetrics()); after != before+1 {
			t.Fatalf("op=%d: counter %d → %d, want +1", tc.op, before, after)
		}
	}
}

func TestMap_HandleTCAPMessage_NilMessage(t *testing.T) {
	h := newTestHandler(t)
	if err := h.HandleTCAPMessage(nil, nil); err == nil {
		t.Fatal("expected error on nil message")
	}
}

// An Invoke of an operation the gateway does not serve is answered with a
// Reject (Q.773 §3.1.5.3.3, unrecognizedOperation), not left unanswered.
func TestMap_HandleTCAPMessage_UnsupportedOpCode(t *testing.T) {
	h := newTestHandler(t)
	out := make(chan *tcap.Message, 1)
	h.SetTCAPOutbound(out)
	// Op code 99 is unassigned in 3GPP TS 29.002.
	msg := invokeMsg(99, tcap.ACMapV3.OID)

	if err := h.HandleTCAPMessage(&tcap.Dialogue{LocalTID: 3, RemoteTID: []byte{1, 2, 3, 4}}, msg); err != nil {
		t.Fatalf("expected nil (dialogue continues), got %v", err)
	}
	if got := h.GetMetrics().Errors.Load(); got != 1 {
		t.Fatalf("Errors counter = %d, want 1", got)
	}
	select {
	case m := <-out:
		c := m.Components[0]
		if m.MessageType != tcap.MsgTypeEnd || c.Type != tcap.CompTypeReject ||
			c.ProblemCode.Type != tcap.ProblemTypeInvoke || c.ProblemCode.Code != tcap.InvokeProblemUnrecognizedOperation {
			t.Fatalf("answer = %+v / %+v", m, c)
		}
	default:
		t.Fatal("no answer to an unsupported operation")
	}
}

// A ReturnResult the gateway cannot decode ends an originated dialogue: the
// caller is told through the error sink instead of waiting out its deadline.
func TestMap_UndecodableResultReachesErrorSink(t *testing.T) {
	h := newTestHandler(t)
	var got []sinkCall
	h.SetErrorSink(func(corrID string, op mapprotocol.OperationCode, code mapprotocol.ErrorCode, detail string) {
		got = append(got, sinkCall{corrID, op, code, detail})
	})
	invID := int32(1)
	dlg := &tcap.Dialogue{LocalTID: 4, NATSCorrelationID: "corr-ati", OriginatingOpCode: int32(mapprotocol.OpAnyTimeInterrogation)}
	comp := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: []byte{0x30, 0x00}}
	if err := h.HandleTCAPMessage(dlg, endWith(comp)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].corrID != "corr-ati" || got[0].op != mapprotocol.OpAnyTimeInterrogation || got[0].code != 0 {
		t.Fatalf("sink calls = %+v", got)
	}
}

// ATI and PSI results reach the response sink typed.
func TestMap_LocationResultsReachResponseSink(t *testing.T) {
	for _, op := range []mapprotocol.OperationCode{mapprotocol.OpAnyTimeInterrogation, mapprotocol.OpProvideSubscriberInfo} {
		h := newTestHandler(t)
		var got any
		h.SetResponseSink(func(_ string, _ mapprotocol.OperationCode, resp any) error { got = resp; return nil })
		invID := int32(1)
		dlg := &tcap.Dialogue{LocalTID: 5, NATSCorrelationID: "c", OriginatingOpCode: int32(op)}
		// { subscriberInfo { subscriberState { assumedIdle } } }
		param := []byte{0x30, 0x06, 0x30, 0x04, 0xA1, 0x02, 0x80, 0x00}
		comp := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: param}
		if err := h.HandleTCAPMessage(dlg, endWith(comp)); err != nil {
			t.Fatal(err)
		}
		var state *mapprotocol.SubscriberState
		switch r := got.(type) {
		case *mapprotocol.AnyTimeInterrogationResponse:
			state = r.SubscriberInfo.SubscriberState
		case *mapprotocol.ProvideSubscriberInfoResponse:
			state = r.SubscriberInfo.SubscriberState
		}
		if state == nil || state.State != "assumedIdle" {
			t.Fatalf("op %d: sink got %#v", op, got)
		}
	}
}

// A SendRoutingInfo result in Return Result Not Last segments, each in a
// TC-CONTINUE, then a Return Result Last in the TC-END, reaches the sink
// once, merged (TS 29.002 §C.2.3).
func TestMap_SegmentedResultMerged(t *testing.T) {
	h := newTestHandler(t)
	var got []any
	h.SetResponseSink(func(_ string, _ mapprotocol.OperationCode, resp any) error { got = append(got, resp); return nil })
	invID := int32(1)
	dlg := &tcap.Dialogue{LocalTID: 6, NATSCorrelationID: "c", OriginatingOpCode: int32(mapprotocol.OpSendRoutingInfo)}
	// [3] { imsi [9] 286010000000123 }, then [3] { roamingNumber 905329990001 }
	imsi := []byte{0xA3, 0x0A, 0x89, 0x08, 0x82, 0x06, 0x01, 0x00, 0x00, 0x00, 0x21, 0xF3}
	msrn := []byte{0xA3, 0x09, 0x04, 0x07, 0x91, 0x09, 0x35, 0x92, 0x99, 0x00, 0x10}
	notLast := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: imsi, NotLast: true}
	if err := h.HandleTCAPMessage(dlg, &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{notLast}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a Not Last segment reached the sink: %+v", got)
	}
	last := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: msrn}
	if err := h.HandleTCAPMessage(dlg, endWith(last)); err != nil {
		t.Fatal(err)
	}
	r, ok := got[0].(*mapprotocol.SendRoutingInfoResponse)
	if len(got) != 1 || !ok || r.IMSI != "286010000000123" || r.RoamingNumber == nil || r.RoamingNumber.Digits != "905329990001" {
		t.Fatalf("sink got %#v", got)
	}
}

// Past the bounds, the segments are dropped and the caller told, once.
func TestMap_SegmentedResultOverflow(t *testing.T) {
	h := newTestHandler(t)
	var results, errs int
	h.SetResponseSink(func(string, mapprotocol.OperationCode, any) error { results++; return nil })
	h.SetErrorSink(func(string, mapprotocol.OperationCode, mapprotocol.ErrorCode, string) { errs++ })
	invID := int32(1)
	dlg := &tcap.Dialogue{LocalTID: 7, NATSCorrelationID: "c", OriginatingOpCode: int32(mapprotocol.OpSendRoutingInfo)}
	seg := []byte{0xA3, 0x03, 0x84, 0x01, 0x00}
	for i := 0; i <= tcap.MaxResultSegments; i++ {
		c := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: seg, NotLast: true}
		if err := h.HandleTCAPMessage(dlg, &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{c}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.HandleTCAPMessage(dlg, endWith(&tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: &invID, Parameter: seg})); err != nil {
		t.Fatal(err)
	}
	if results != 0 || errs != 1 {
		t.Fatalf("results %d, errors %d; want 0 and 1", results, errs)
	}
}

func TestMap_HandleTCAPMessage_ReturnError(t *testing.T) {
	h := newTestHandler(t)
	invID := int32(1)
	errCode := int32(mapprotocol.ErrUnknownSubscriber)
	msg := &tcap.Message{
		MessageType: tcap.MsgTypeEnd,
		Components: []*tcap.Component{
			{
				Type:      tcap.CompTypeReturnError,
				InvokeID:  &invID,
				ErrorCode: &errCode,
			},
		},
	}

	if err := h.HandleTCAPMessage(nil, msg); err != nil {
		t.Fatalf("HandleTCAPMessage: %v", err)
	}
	if got := h.GetMetrics().UnknownSubscriberErrors.Load(); got != 1 {
		t.Fatalf("UnknownSubscriberErrors = %d, want 1", got)
	}
}

// TestMap_DispatcherStrictRouting confirms MAP only receives messages whose
// AC OID matches its registration. CAP-OID messages dispatch to a different
// (unregistered) handler → ErrUnknownAppContext.
func TestMap_DispatcherStrictRouting(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACMapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	err := d.Dispatch(nil, invokeMsg(mapprotocol.OpUpdateLocation, tcap.ACCapV3.OID))
	if !errors.Is(err, tcap.ErrUnknownAppContext) {
		t.Fatalf("expected ErrUnknownAppContext for CAP-OID, got %v", err)
	}
	if got := h.GetMetrics().UpdateLocationCount.Load(); got != 0 {
		t.Fatalf("UpdateLocationCount = %d, want 0 (cross-context dispatch must not reach handler)", got)
	}
}

type sinkCall struct {
	corrID string
	op     mapprotocol.OperationCode
	code   mapprotocol.ErrorCode
	detail string
}

func endWith(comp *tcap.Component) *tcap.Message {
	return &tcap.Message{MessageType: tcap.MsgTypeEnd, Components: []*tcap.Component{comp}}
}

// An originated dialogue answered with ReturnError or Reject reaches the error
// sink with the dialogue's correlation ID and op code, so the NATS caller gets
// an answer instead of waiting out its deadline.
func TestMap_ErrorSink_OriginatedDialogue(t *testing.T) {
	invID := int32(1)
	absent := int32(mapprotocol.ErrAbsentSubscriberSM)
	originated := func() *tcap.Dialogue {
		return &tcap.Dialogue{LocalTID: 7, NATSCorrelationID: "corr-1", OriginatingOpCode: int32(mapprotocol.OpMtForwardSM)}
	}

	cases := []struct {
		name string
		comp *tcap.Component
		want sinkCall
	}{
		{
			name: "ReturnError carries the MAP error code and its name",
			comp: &tcap.Component{Type: tcap.CompTypeReturnError, InvokeID: &invID, ErrorCode: &absent},
			want: sinkCall{"corr-1", mapprotocol.OpMtForwardSM, mapprotocol.ErrAbsentSubscriberSM, "absentSubscriberSM"},
		},
		{
			name: "ReturnError without a local error code",
			comp: &tcap.Component{Type: tcap.CompTypeReturnError, InvokeID: &invID},
			want: sinkCall{"corr-1", mapprotocol.OpMtForwardSM, 0, "MAP ReturnError without a local error code"},
		},
		{
			name: "Reject carries code 0 and the problem",
			comp: &tcap.Component{Type: tcap.CompTypeReject, InvokeID: &invID,
				ProblemCode: &tcap.ProblemCode{Type: tcap.ProblemTypeInvoke, Code: 2}},
			want: sinkCall{"corr-1", mapprotocol.OpMtForwardSM, 0, "TCAP Reject problem type=1 code=2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHandler(t)
			var got []sinkCall
			h.SetErrorSink(func(corrID string, op mapprotocol.OperationCode, code mapprotocol.ErrorCode, detail string) {
				got = append(got, sinkCall{corrID, op, code, detail})
			})
			if err := h.HandleTCAPMessage(originated(), endWith(c.comp)); err != nil {
				t.Fatalf("HandleTCAPMessage: %v", err)
			}
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("sink calls = %+v, want [%+v]", got, c.want)
			}
		})
	}
}

// One outcome per originated dialogue: whatever arrives after the first —
// a second result, an error after a result — is dropped.
func TestMap_OneOutcomePerDialogue(t *testing.T) {
	h := newTestHandler(t)
	var results, errs int
	h.SetResponseSink(func(string, mapprotocol.OperationCode, any) error { results++; return nil })
	h.SetErrorSink(func(string, mapprotocol.OperationCode, mapprotocol.ErrorCode, string) { errs++ })
	invID := int32(1)
	absent := int32(mapprotocol.ErrAbsentSubscriberSM)
	dlg := &tcap.Dialogue{LocalTID: 8, NATSCorrelationID: "c", OriginatingOpCode: int32(mapprotocol.OpMtForwardSM)}
	for _, c := range []*tcap.Component{
		{Type: tcap.CompTypeReturnResult, InvokeID: &invID},
		{Type: tcap.CompTypeReturnResult, InvokeID: &invID},
		{Type: tcap.CompTypeReturnError, InvokeID: &invID, ErrorCode: &absent},
	} {
		if err := h.HandleTCAPMessage(dlg, &tcap.Message{MessageType: tcap.MsgTypeContinue, Components: []*tcap.Component{c}}); err != nil {
			t.Fatal(err)
		}
	}
	if results != 1 || errs != 0 {
		t.Fatalf("results %d, errors %d; want 1 and 0", results, errs)
	}
	if dlg.MarkAnswered() {
		t.Fatal("the dialogue was not marked answered")
	}
}

// A dialogue the peer opened has no NATS waiter: nothing to report.
func TestMap_ErrorSink_NotForPeerDialogues(t *testing.T) {
	h := newTestHandler(t)
	called := false
	h.SetErrorSink(func(string, mapprotocol.OperationCode, mapprotocol.ErrorCode, string) { called = true })
	invID := int32(1)
	code := int32(mapprotocol.ErrSystemFailure)
	comp := &tcap.Component{Type: tcap.CompTypeReturnError, InvokeID: &invID, ErrorCode: &code}

	if err := h.HandleTCAPMessage(&tcap.Dialogue{LocalTID: 9}, endWith(comp)); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleTCAPMessage(nil, endWith(comp)); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("error sink fired for a dialogue without a NATS correlation ID")
	}
}

func TestMap_ErrorCodeString(t *testing.T) {
	if got := mapprotocol.ErrSubscriberBusyForMTSMS.String(); got != "subscriberBusyForMT-SMS" {
		t.Errorf("31 = %q", got)
	}
	if got := mapprotocol.ErrorCode(99).String(); got != "mapError(99)" {
		t.Errorf("99 = %q", got)
	}
}
