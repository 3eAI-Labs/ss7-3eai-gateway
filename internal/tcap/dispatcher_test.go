// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// fakeHandler is a test double for AppContextHandler that records calls.
type fakeHandler struct {
	name      string
	callCount atomic.Int32
	returnErr error

	mu       sync.Mutex
	received []*Message
}

func (f *fakeHandler) HandleTCAPMessage(dialogue *Dialogue, msg *Message) error {
	f.callCount.Add(1)
	f.mu.Lock()
	f.received = append(f.received, msg)
	f.mu.Unlock()
	return f.returnErr
}

func (f *fakeHandler) Name() string { return f.name }

func newFakeHandler(name string) *fakeHandler {
	return &fakeHandler{name: name}
}

// dialogueMsgWithAC builds a minimal Message with an ApplicationContext set.
func dialogueMsgWithAC(oid []int) *Message {
	return &Message{
		MessageType: MsgTypeBegin,
		DialoguePortion: &DialoguePortion{
			ApplicationContext: &ApplicationContext{OID: oid},
		},
	}
}

// --- Register --------------------------------------------------------------

func TestDispatcher_Register_Success(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	h := newFakeHandler("map")

	if err := d.Register(ACMapV3.OID, h); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := d.LookupHandler(ACMapV3.OID)
	if got != h {
		t.Fatalf("expected handler %v, got %v", h, got)
	}
}

func TestDispatcher_Register_Duplicate(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	h1 := newFakeHandler("map")
	h2 := newFakeHandler("imposter")

	if err := d.Register(ACMapV3.OID, h1); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	err := d.Register(ACMapV3.OID, h2)
	if !errors.Is(err, ErrDuplicateOID) {
		t.Fatalf("expected ErrDuplicateOID, got %v", err)
	}
	// First handler must remain registered (no overwrite).
	if got := d.LookupHandler(ACMapV3.OID); got != h1 {
		t.Fatalf("registry overwritten on duplicate; got %v want %v", got, h1)
	}
}

func TestDispatcher_Register_NilHandler(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	if err := d.Register(ACMapV3.OID, nil); !errors.Is(err, ErrNilHandler) {
		t.Fatalf("expected ErrNilHandler, got %v", err)
	}
}

func TestDispatcher_Register_InvalidOID(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	h := newFakeHandler("map")

	cases := []struct {
		name string
		oid  []int
	}{
		{"empty", nil},
		{"single arc", []int{0}},
		{"negative arc", []int{0, 4, 0, -1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.Register(tc.oid, h); !errors.Is(err, ErrInvalidOID) {
				t.Fatalf("expected ErrInvalidOID, got %v", err)
			}
		})
	}
}

// --- Dispatch --------------------------------------------------------------

func TestDispatcher_Dispatch_Success(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	h := newFakeHandler("map")
	if err := d.Register(ACMapV3.OID, h); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	msg := dialogueMsgWithAC(ACMapV3.OID)
	dlg := &Dialogue{LocalTID: 0x01000001}

	if err := d.Dispatch(dlg, msg); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if got := h.callCount.Load(); got != 1 {
		t.Fatalf("expected 1 handler call, got %d", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 1 {
		t.Fatalf("expected Dispatches=1, got %d", got)
	}
	if got := d.Metrics().PerOIDCount("0.4.0.0.1.0.1.3"); got != 1 {
		t.Fatalf("expected PerOIDCount=1, got %d", got)
	}
}

func TestDispatcher_Dispatch_UnknownContext(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	// Register MAP v3 but dispatch arrives for CAP v3.
	if err := d.Register(ACMapV3.OID, newFakeHandler("map")); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	msg := dialogueMsgWithAC(ACCapV3.OID)
	err := d.Dispatch(nil, msg)
	if !errors.Is(err, ErrUnknownAppContext) {
		t.Fatalf("expected ErrUnknownAppContext, got %v", err)
	}
	if got := d.Metrics().UnknownContext.Load(); got != 1 {
		t.Fatalf("expected UnknownContext=1, got %d", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 0 {
		t.Fatalf("expected Dispatches=0 on unknown ctx, got %d", got)
	}
}

func TestDispatcher_Dispatch_MissingContext(t *testing.T) {
	d := NewDispatcher(zap.NewNop())

	cases := []struct {
		name string
		msg  *Message
	}{
		{"nil message", nil},
		{"no dialogue portion", &Message{MessageType: MsgTypeBegin}},
		{"no application context", &Message{
			MessageType:     MsgTypeBegin,
			DialoguePortion: &DialoguePortion{},
		}},
		{"empty OID", &Message{
			MessageType: MsgTypeBegin,
			DialoguePortion: &DialoguePortion{
				ApplicationContext: &ApplicationContext{OID: []int{}},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := d.Dispatch(nil, tc.msg)
			if !errors.Is(err, ErrMissingAppContext) {
				t.Fatalf("expected ErrMissingAppContext, got %v", err)
			}
		})
	}
	if got := d.Metrics().MissingContext.Load(); got != uint64(len(cases)) {
		t.Fatalf("expected MissingContext=%d, got %d", len(cases), got)
	}
}

func TestDispatcher_Dispatch_HandlerError(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	handlerErr := errors.New("simulated handler failure")
	h := &fakeHandler{name: "map", returnErr: handlerErr}

	if err := d.Register(ACMapV3.OID, h); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	msg := dialogueMsgWithAC(ACMapV3.OID)
	err := d.Dispatch(nil, msg)
	if !errors.Is(err, handlerErr) {
		t.Fatalf("expected handler error wrapped, got %v", err)
	}
	if got := d.Metrics().HandlerErrors.Load(); got != 1 {
		t.Fatalf("expected HandlerErrors=1, got %d", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 0 {
		t.Fatalf("expected Dispatches=0 on handler error, got %d", got)
	}
}

// --- Multi-OID handler -----------------------------------------------------

func TestDispatcher_MultiOIDSameHandler(t *testing.T) {
	// Real-world: MAP handler registers itself for v1, v2, v3 of networkLocUp.
	d := NewDispatcher(zap.NewNop())
	mapH := newFakeHandler("map")

	for _, ac := range []ApplicationContext{ACMapV1, ACMapV2, ACMapV3} {
		if err := d.Register(ac.OID, mapH); err != nil {
			t.Fatalf("register %s failed: %v", ac.Name, err)
		}
	}

	// Dispatch each version, expect one handler call each.
	for _, ac := range []ApplicationContext{ACMapV1, ACMapV2, ACMapV3} {
		if err := d.Dispatch(nil, dialogueMsgWithAC(ac.OID)); err != nil {
			t.Fatalf("dispatch %s failed: %v", ac.Name, err)
		}
	}
	if got := mapH.callCount.Load(); got != 3 {
		t.Fatalf("expected 3 handler calls, got %d", got)
	}
	if got := d.Metrics().Dispatches.Load(); got != 3 {
		t.Fatalf("expected Dispatches=3, got %d", got)
	}
}

// --- LookupHandler / RegisteredOIDs ----------------------------------------

func TestDispatcher_LookupHandler_Unknown(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	if got := d.LookupHandler(ACMapV3.OID); got != nil {
		t.Fatalf("expected nil for unknown OID, got %v", got)
	}
	if got := d.LookupHandler([]int{}); got != nil {
		t.Fatalf("expected nil for invalid OID, got %v", got)
	}
}

func TestDispatcher_RegisteredOIDs(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	mapH := newFakeHandler("map")
	capH := newFakeHandler("cap")

	if err := d.Register(ACMapV3.OID, mapH); err != nil {
		t.Fatalf("register map failed: %v", err)
	}
	if err := d.Register(ACCapV3.OID, capH); err != nil {
		t.Fatalf("register cap failed: %v", err)
	}

	got := d.RegisteredOIDs()
	if len(got) != 2 {
		t.Fatalf("expected 2 OIDs, got %d (%v)", len(got), got)
	}
	want := map[string]bool{
		"0.4.0.0.1.0.1.3":  true, // ACMapV3
		"0.4.0.0.1.0.50.2": true, // ACCapV3
	}
	for _, oid := range got {
		if !want[oid] {
			t.Fatalf("unexpected OID in registry: %s", oid)
		}
	}
}

// --- Concurrency -----------------------------------------------------------

// TestDispatcher_ConcurrentRegisterDispatch verifies registration and
// dispatch can run concurrently without data races. Run with -race.
func TestDispatcher_ConcurrentRegisterDispatch(t *testing.T) {
	d := NewDispatcher(zap.NewNop())
	preH := newFakeHandler("preregistered")
	if err := d.Register(ACMapV3.OID, preH); err != nil {
		t.Fatalf("preregister failed: %v", err)
	}

	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half: concurrent dispatches against the preregistered OID.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = d.Dispatch(nil, dialogueMsgWithAC(ACMapV3.OID))
			}
		}()
	}

	// Half: concurrent registrations of distinct OIDs (no duplicates).
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			h := newFakeHandler("map-variant")
			// Synthetic distinct OIDs in a non-conflicting subtree.
			oid := []int{1, 3, 6, 1, 4, 1, 99999, id}
			_ = d.Register(oid, h)
		}(i)
	}

	wg.Wait()

	if got := preH.callCount.Load(); got != int32(goroutines*iterations) {
		t.Fatalf("expected %d preH calls, got %d", goroutines*iterations, got)
	}
	if got := d.Metrics().Dispatches.Load(); got != uint64(goroutines*iterations) {
		t.Fatalf("expected Dispatches=%d, got %d", goroutines*iterations, got)
	}
	if got := len(d.RegisteredOIDs()); got != 1+goroutines {
		t.Fatalf("expected %d registered OIDs, got %d", 1+goroutines, got)
	}
}

// --- canonicalOID ----------------------------------------------------------

func TestCanonicalOID(t *testing.T) {
	cases := []struct {
		name    string
		oid     []int
		want    string
		wantErr bool
	}{
		{"map-v3", []int{0, 4, 0, 0, 1, 0, 1, 3}, "0.4.0.0.1.0.1.3", false},
		{"cap-v3", []int{0, 4, 0, 0, 1, 0, 50, 2}, "0.4.0.0.1.0.50.2", false},
		{"two-arc minimum", []int{0, 0}, "0.0", false},
		{"private enterprise", []int{1, 3, 6, 1, 4, 1, 99999, 1}, "1.3.6.1.4.1.99999.1", false},
		{"empty", nil, "", true},
		{"single arc", []int{1}, "", true},
		{"negative arc", []int{0, -1, 2}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalOID(tc.oid)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err mismatch: got %v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
