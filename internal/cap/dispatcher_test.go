// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap_test

import (
	"errors"
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/cap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Compile-time assertion: *cap.Handler satisfies tcap.AppContextHandler.
var _ tcap.AppContextHandler = (*cap.Handler)(nil)

func newTestHandler(t *testing.T) *cap.Handler {
	t.Helper()
	cfg := &cap.Config{
		GSMSCFAddress:   "127.0.0.1",
		GSMSCFPointCode: 5678,
	}
	h, err := cap.NewHandler(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func invokeMsg(opCode cap.OperationCode, ac []int) *tcap.Message {
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

func TestCap_Name(t *testing.T) {
	h := newTestHandler(t)
	if got := h.Name(); got != "cap" {
		t.Fatalf("Name() = %q, want %q", got, "cap")
	}
}

func TestCap_Dispatch_InitialDP(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACCapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	dlg := &tcap.Dialogue{LocalTID: 0x02000001}
	if err := d.Dispatch(dlg, invokeMsg(cap.OpInitialDP, tcap.ACCapV3.OID)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := h.GetMetrics().InitialDPCount.Load(); got != 1 {
		t.Fatalf("InitialDPCount = %d, want 1", got)
	}
	if got := h.GetMetrics().OperationsReceived.Load(); got != 1 {
		t.Fatalf("OperationsReceived = %d, want 1", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 1 {
		t.Fatalf("Dispatcher.Dispatches = %d, want 1", got)
	}
}

func TestCap_Dispatch_PerOpCounters(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACCapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := []struct {
		op     cap.OperationCode
		assert func(*cap.Metrics) uint64
	}{
		{cap.OpInitialDP, func(m *cap.Metrics) uint64 { return m.InitialDPCount.Load() }},
		{cap.OpEventReportBCSM, func(m *cap.Metrics) uint64 { return m.EventReportBCSMCount.Load() }},
		{cap.OpConnect, func(m *cap.Metrics) uint64 { return m.ConnectCount.Load() }},
		{cap.OpContinue, func(m *cap.Metrics) uint64 { return m.ContinueCount.Load() }},
		{cap.OpReleaseCall, func(m *cap.Metrics) uint64 { return m.ReleaseCallCount.Load() }},
		{cap.OpApplyCharging, func(m *cap.Metrics) uint64 { return m.ApplyChargingCount.Load() }},
	}

	for _, tc := range cases {
		before := tc.assert(h.GetMetrics())
		if err := d.Dispatch(nil, invokeMsg(tc.op, tcap.ACCapV3.OID)); err != nil {
			t.Fatalf("Dispatch op=%d: %v", tc.op, err)
		}
		if after := tc.assert(h.GetMetrics()); after != before+1 {
			t.Fatalf("op=%d: counter %d → %d, want +1", tc.op, before, after)
		}
	}
}

func TestCap_HandleTCAPMessage_NilMessage(t *testing.T) {
	h := newTestHandler(t)
	if err := h.HandleTCAPMessage(nil, nil); err == nil {
		t.Fatal("expected error on nil message")
	}
}

func TestCap_HandleTCAPMessage_UnsupportedOpCode(t *testing.T) {
	h := newTestHandler(t)
	// Op code 200 is not assigned in 3GPP TS 29.078.
	msg := invokeMsg(200, tcap.ACCapV3.OID)

	if err := h.HandleTCAPMessage(nil, msg); err != nil {
		t.Fatalf("expected nil (dialogue continues), got %v", err)
	}
	if got := h.GetMetrics().Errors.Load(); got != 1 {
		t.Fatalf("Errors counter = %d, want 1", got)
	}
}

// TestCap_DispatcherStrictRouting confirms CAP only receives messages whose
// AC OID matches its registration; MAP-OID messages are not routed here.
func TestCap_DispatcherStrictRouting(t *testing.T) {
	h := newTestHandler(t)
	d := tcap.NewDispatcher(zap.NewNop())
	if err := d.Register(tcap.ACCapV3.OID, h); err != nil {
		t.Fatalf("Register: %v", err)
	}

	err := d.Dispatch(nil, invokeMsg(cap.OpInitialDP, tcap.ACMapV3.OID))
	if !errors.Is(err, tcap.ErrUnknownAppContext) {
		t.Fatalf("expected ErrUnknownAppContext for MAP-OID, got %v", err)
	}
	if got := h.GetMetrics().InitialDPCount.Load(); got != 0 {
		t.Fatalf("InitialDPCount = %d, want 0 (cross-context dispatch must not reach handler)", got)
	}
}
