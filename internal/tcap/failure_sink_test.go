// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"context"
	"sync"
	"testing"
	"time"

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

	_, err = h.Originate(&ACMapV3, called, calling, opCode, []byte{0x01}, corrID)
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
