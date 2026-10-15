// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// FailureSink contract: composition root installs the sink, TCAP fires it
// exactly once per originated dialogue that ends without a ReturnResult.
// Two terminal paths exercised here: local DialogueTimeout (TimerManager
// reaper) and inbound P-ABORT from peer (handleAbort).

type sinkCall struct {
	corrID string
	opCode int32
	reason FailureReason
	detail string
}

type recordingSink struct {
	mu    sync.Mutex
	calls []sinkCall
}

func (r *recordingSink) fn() FailureSink {
	return func(corrID string, opCode int32, reason FailureReason, detail string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, sinkCall{corrID, opCode, reason, detail})
	}
}

func (r *recordingSink) snapshot() []sinkCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sinkCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestFailureSink_DialogueTimeout drives a real Originate, lets the timer
// manager reap the dialogue, and asserts the sink was fired with the
// originated dialogue's correlation id and op code.
func TestFailureSink_DialogueTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := &Config{
		NodeID:                  1,
		DialogueTimeout:         150 * time.Millisecond,
		InvokeTimeout:           150 * time.Millisecond,
		IdleTimeout:             5 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          1,
		TimerResolution:         50 * time.Millisecond,
	}
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(cfg, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, h.Start(ctx))
	defer h.Stop(ctx)

	called, calling := sampleAddrs()
	const opCode int32 = 45 // SRI-for-SM
	const corrID = "nats-corr-timeout-1"

	_, err = h.Originate(&ACMapV3, called, calling, opCode, []byte{0x30, 0x00}, corrID)
	require.NoError(t, err)

	// Handler's own worker drains appOutgoingCh and forwards encoded UDT to
	// sccpOutgoingCh (buffered, no drainer needed). We just wait for the
	// timer-manager reaper to fire past DialogueTimeout + a couple of
	// TimerResolution ticks. The reaper's own P-ABORT send-attempt logs a
	// "Failed to send timeout Abort" error because RemoteTID is empty (no
	// peer END ever arrived) — that's expected for a pure-timeout case and
	// orthogonal to the failure-sink contract under test here.
	require.Eventually(t, func() bool {
		return len(rec.snapshot()) >= 1
	}, 1*time.Second, 25*time.Millisecond, "failure sink not fired")

	calls := rec.snapshot()
	require.Len(t, calls, 1, "exactly one sink fire per timed-out dialogue")
	assert.Equal(t, corrID, calls[0].corrID)
	assert.Equal(t, opCode, calls[0].opCode)
	assert.Equal(t, FailureTimeout, calls[0].reason)
	assert.NotEmpty(t, calls[0].detail)
}

// TestFailureSink_AbortReceived seeds a dialogue (with NATSCorrelationID)
// directly, then drives handleAbort to assert the sink fires with
// FailureAbortReceived and the detail carries the abort cause.
func TestFailureSink_AbortReceived(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := &Config{
		NodeID:                  1,
		DialogueTimeout:         5 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             10 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          1,
		TimerResolution:         50 * time.Millisecond,
	}
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(cfg, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())

	const opCode int32 = 44 // MtForwardSM
	const corrID = "nats-corr-abort-1"

	// Seed an originated-style dialogue with the natsbridge-side context
	// already pinned. LocalTID 0x01020304 → DTID-encoded form on the wire.
	localTID := uint32(0x01020304)
	h.dialoguesMu.Lock()
	h.dialogues[localTID] = &Dialogue{
		LocalTID:           localTID,
		State:              StateActive,
		OriginatingOpCode:  opCode,
		NATSCorrelationID:  corrID,
		ApplicationContext: &ACMapV3,
		CreatedAt:          time.Now(),
		LastActivity:       time.Now(),
		ExpiresAt:          time.Now().Add(5 * time.Second),
		PendingInvokes:     map[int32]*PendingInvoke{},
	}
	h.metrics.ActiveDialogues.Add(1)
	h.dialoguesMu.Unlock()

	cause := AbortResourceLimitation
	dtid := h.encodeTID(localTID)
	abortMsg := &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: dtid,
		AbortCause:     &cause,
		PAbort:         true,
	}

	h.handleAbort(nil, abortMsg)

	calls := rec.snapshot()
	require.Len(t, calls, 1, "exactly one sink fire per inbound ABORT")
	assert.Equal(t, corrID, calls[0].corrID)
	assert.Equal(t, opCode, calls[0].opCode)
	assert.Equal(t, FailureAbortReceived, calls[0].reason)
	assert.Contains(t, calls[0].detail, "P-ABORT")
	assert.Contains(t, calls[0].detail, "0x04", "AbortResourceLimitation cause")

	// Dialogue must be gone after ABORT — terminal state.
	h.dialoguesMu.RLock()
	_, exists := h.dialogues[localTID]
	h.dialoguesMu.RUnlock()
	assert.False(t, exists, "dialogue removed after ABORT")
}

// TestFailureSink_NoCorrelationID guards the "peer-originated dialogue"
// case: no NATSCorrelationID means no upstream waiter exists, so the
// sink must not fire (otherwise we'd publish a reply nobody is waiting
// for and confuse downstream metrics).
func TestFailureSink_NoCorrelationID(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := &Config{
		NodeID:                  1,
		DialogueTimeout:         5 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             10 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          1,
		TimerResolution:         50 * time.Millisecond,
	}
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(cfg, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())

	localTID := uint32(0x01050607)
	h.dialoguesMu.Lock()
	h.dialogues[localTID] = &Dialogue{
		LocalTID:           localTID,
		State:              StateActive,
		OriginatingOpCode:  0,  // peer originated; we have no op context
		NATSCorrelationID:  "", // critical: no upstream waiter
		ApplicationContext: &ACMapV3,
		CreatedAt:          time.Now(),
		LastActivity:       time.Now(),
		ExpiresAt:          time.Now().Add(5 * time.Second),
		PendingInvokes:     map[int32]*PendingInvoke{},
	}
	h.metrics.ActiveDialogues.Add(1)
	h.dialoguesMu.Unlock()

	cause := AbortResourceLimitation
	dtid := h.encodeTID(localTID)
	h.handleAbort(nil, &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: dtid,
		AbortCause:     &cause,
		PAbort:         true,
	})

	assert.Empty(t, rec.snapshot(), "no sink fire when corrID is empty")
}

// A TC-U-ABORT carries no P-Abort cause: an HLR or MSC refusing the proposed
// application context answers with an AARE (reject-permanent) naming the one
// it supports. It must parse, end the dialogue without a panic on the absent
// cause, and tell the caller what the peer proposed.
func TestFailureSink_UAbortRefusingContext(t *testing.T) {
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 5 * time.Second, InvokeTimeout: 5 * time.Second,
		IdleTimeout: 10 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
		TimerResolution: 50 * time.Millisecond,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())

	localTID := uint32(0x01020305)
	h.dialoguesMu.Lock()
	h.dialogues[localTID] = &Dialogue{
		LocalTID: localTID, State: StateInitWait, OriginatingOpCode: 45,
		NATSCorrelationID: "nats-corr-uabort", ApplicationContext: &ACShortMsgGatewayV3,
		CreatedAt: time.Now(), LastActivity: time.Now(), ExpiresAt: time.Now().Add(5 * time.Second),
		PendingInvokes: map[int32]*PendingInvoke{},
	}
	h.dialoguesMu.Unlock()

	// The peer's answer, as it arrives on the wire.
	wire, err := h.encodeTCAPMessage(&Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: h.encodeTID(localTID),
		DialoguePortion: &DialoguePortion{
			ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 20, 2}},
			Rejected:           true,
		},
	})
	require.NoError(t, err)
	msg, err := h.parseTCAPMessage(wire)
	require.NoError(t, err, "a TC-U-ABORT must parse")
	assert.False(t, msg.PAbort)
	assert.Nil(t, msg.AbortCause)

	h.handleAbort(nil, msg)

	calls := rec.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, FailureAbortReceived, calls[0].reason)
	assert.Contains(t, calls[0].detail, "U-ABORT application context refused")
	assert.Contains(t, calls[0].detail, "peer proposes 0.4.0.0.1.0.20.2")
}

// A BEGIN that never reached the peer (no SCCP route, M3UA could not send
// it, or a UDTS returned it) ends its dialogue at once: the caller is told
// why, instead of waiting out DialogueTimeout.
func TestFailureSink_NotDelivered(t *testing.T) {
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 30 * time.Second, InvokeTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
		TimerResolution: 50 * time.Millisecond,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.Start(ctx))
	defer h.Stop(ctx)

	called, calling := sampleAddrs()
	dlg, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, []byte{0x30, 0x00}, "nats-corr-undelivered")
	require.NoError(t, err)

	var begin *sccp.SccpMessage
	select {
	case begin = <-sccpOut:
	case <-time.After(time.Second):
		t.Fatal("BEGIN not sent")
	}

	h.HandleDeliveryFailure(begin, "returned by the network: UDTS cause 1 (no translation for this specific address)")

	calls := rec.snapshot()
	require.Len(t, calls, 1, "reported at once, not after DialogueTimeout")
	assert.Equal(t, "nats-corr-undelivered", calls[0].corrID)
	assert.Equal(t, int32(44), calls[0].opCode)
	assert.Equal(t, FailureNotDelivered, calls[0].reason)
	assert.Equal(t, "ABORTED", calls[0].reason.String())
	assert.Contains(t, calls[0].detail, "UDTS cause 1")
	assert.Nil(t, h.getDialogue(dlg.LocalTID), "dialogue removed")

	// A second report for the same message (or one for an END we sent)
	// finds no waiting dialogue and does nothing.
	h.HandleDeliveryFailure(begin, "again")
	assert.Len(t, rec.snapshot(), 1)
}

func startOutcomeHandler(t *testing.T) (*Handler, chan *sccp.SccpMessage, *recordingSink) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 150 * time.Millisecond, InvokeTimeout: 150 * time.Millisecond,
		IdleTimeout: 5 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
		TimerResolution: 50 * time.Millisecond,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
		cancel()
	})
	return h, sccpIn, rec
}

// An END that answers nothing still gives the originator an outcome.
func TestFailureSink_EndWithoutResult(t *testing.T) {
	h, in, rec := startOutcomeHandler(t)
	called, calling := sampleAddrs()
	dlg, err := h.Originate(&ACMapV3, called, calling, 45, []byte{0x30, 0x00}, "corr-empty-end")
	require.NoError(t, err)
	wire, err := EncodeMessage(&Message{MessageType: MsgTypeEnd, DestinationTID: h.encodeTID(dlg.LocalTID)})
	require.NoError(t, err)
	in <- &sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, Data: wire, CalledAddress: calling, CallingAddress: called}

	require.Eventually(t, func() bool { return len(rec.snapshot()) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, FailureRemoteError, rec.snapshot()[0].reason)
	assert.Contains(t, rec.snapshot()[0].detail, "without a result")
	time.Sleep(300 * time.Millisecond) // past DialogueTimeout: no second outcome
	assert.Len(t, rec.snapshot(), 1)
}

// A dialogue whose originator already has its outcome — a result that came
// in a CONTINUE — is not reported again when its timer runs out.
func TestFailureSink_NoTimeoutAfterOutcome(t *testing.T) {
	h, _, rec := startOutcomeHandler(t)
	called, calling := sampleAddrs()
	dlg, err := h.Originate(&ACMapV3, called, calling, 45, []byte{0x30, 0x00}, "corr-answered")
	require.NoError(t, err)
	require.True(t, dlg.MarkAnswered(), "as the MAP layer does on publishing the result")

	require.Eventually(t, func() bool { return h.getDialogueCount() == 0 }, 2*time.Second, 25*time.Millisecond,
		"the dialogue still times out")
	assert.Empty(t, rec.snapshot(), "but its originator hears nothing more")
}

// A Return Result Not Last keeps its kind through the handler both ways
// (Q.773 §3.1: returnResultNotLast [7]), and a Last stays a Last.
func TestReturnResultNotLastRoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 30 * time.Second, InvokeTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	invID := int32(1)
	for _, notLast := range []bool{true, false} {
		wire, err := h.encodeTCAPMessage(&Message{
			MessageType:    MsgTypeContinue,
			OriginatingTID: h.encodeTID(9),
			DestinationTID: h.encodeTID(8),
			Components: []*Component{{
				Type: CompTypeReturnResult, InvokeID: &invID, OperationCode: ptrInt32(22), Parameter: []byte{0xA3, 0x00}, NotLast: notLast,
			}},
		})
		require.NoError(t, err)
		msg, err := h.parseTCAPMessage(wire)
		require.NoError(t, err)
		require.Len(t, msg.Components, 1)
		assert.Equal(t, CompTypeReturnResult, msg.Components[0].Type)
		assert.Equal(t, notLast, msg.Components[0].NotLast)
	}
}

// The messages of one transaction go to one worker: a CONTINUE and the END
// after it carry this node's transaction ID as destination, whatever the
// peer's ID; a BEGIN is keyed by its origination ID.
func TestTransactionShard(t *testing.T) {
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 30 * time.Second, InvokeTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	encode := func(m *Message) []byte {
		b, err := h.encodeTCAPMessage(m)
		require.NoError(t, err)
		return b
	}
	const workers = 64
	spread := map[int]bool{}
	for local := uint32(1); local <= 32; local++ {
		cont := transactionShard(encode(&Message{MessageType: MsgTypeContinue,
			OriginatingTID: h.encodeTID(0xAAAA0000 + local), DestinationTID: h.encodeTID(local)}), workers)
		end := transactionShard(encode(&Message{MessageType: MsgTypeEnd, DestinationTID: h.encodeTID(local)}), workers)
		begin := transactionShard(encode(&Message{MessageType: MsgTypeBegin, OriginatingTID: h.encodeTID(local)}), workers)
		assert.Equal(t, cont, end, "CONTINUE and END of transaction %d", local)
		assert.Equal(t, end, begin, "keyed by the same ID")
		spread[end] = true
	}
	assert.Greater(t, len(spread), 8, "transactions spread over the workers")
	assert.Equal(t, 0, transactionShard([]byte{0xFF, 0x00}, workers))
	assert.Equal(t, 0, transactionShard(nil, workers))
}

// notSentHandler is a running handler whose SCCP channel holds one
// message, with its failure and lost sinks recorded.
func notSentHandler(t *testing.T) (*Handler, chan *sccp.SccpMessage, *recordingSink, *recordingSink) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := make(chan *sccp.SccpMessage, 16), make(chan *sccp.SccpMessage, 1)
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 30 * time.Second, InvokeTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, MaxDialogues: 100, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 20, ChannelBufferSize: 100, WorkerPoolSize: 1,
		TimerResolution: 50 * time.Millisecond,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	failed, lost := &recordingSink{}, &recordingSink{}
	h.SetFailureSink(failed.fn())
	record := lost.fn()
	h.SetLostSink(func(d *Dialogue, reason FailureReason, detail string) {
		record(d.NATSCorrelationID, d.OriginatingOpCode, reason, detail)
	})
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
		cancel()
	})
	return h, sccpOut, failed, lost
}

// A BEGIN the SCCP channel cannot take ends its dialogue at once, reported
// as not delivered to the lost sink and the originator, instead of being
// dropped with the dialogue left to its timer.
func TestFailureSink_SCCPChannelFull(t *testing.T) {
	h, sccpOut, failed, lost := notSentHandler(t)
	sccpOut <- &sccp.SccpMessage{} // the channel is full

	called, calling := sampleAddrs()
	dlg, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, []byte{0x30, 0x00}, "nats-corr-full")
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(failed.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)
	c := failed.snapshot()[0]
	assert.Equal(t, sinkCall{"nats-corr-full", 44, FailureNotDelivered, "not sent: SCCP channel full"}, c)
	assert.Equal(t, []sinkCall{c}, lost.snapshot())
	assert.Nil(t, h.getDialogue(dlg.LocalTID), "dialogue removed")
	assert.Zero(t, h.metrics.ActiveDialogues.Load())
}

// DialogueNotSent — the application layer could not queue a message — ends
// the dialogue once, however often it is told.
func TestFailureSink_DialogueNotSent(t *testing.T) {
	h, sccpOut, failed, lost := notSentHandler(t)
	called, calling := sampleAddrs()
	dlg, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, []byte{0x30, 0x00}, "nats-corr-not-sent")
	require.NoError(t, err)
	<-sccpOut // the BEGIN went out

	h.DialogueNotSent(dlg, "not sent: MAP outbound channel full")
	h.DialogueNotSent(dlg, "again")
	h.DialogueNotSent(nil, "no dialogue")
	require.Eventually(t, func() bool { return len(failed.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // a second report would have arrived by now
	calls := failed.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, FailureNotDelivered, calls[0].reason)
	assert.Equal(t, "nats-corr-not-sent", calls[0].corrID)
	assert.Contains(t, []string{"not sent: MAP outbound channel full", "again"}, calls[0].detail, "whichever report came first")
	assert.Len(t, lost.snapshot(), 1)
	assert.Nil(t, h.getDialogue(dlg.LocalTID))
}
