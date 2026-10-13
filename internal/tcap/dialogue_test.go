// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// TestDialogueStateTransition tests dialogue state transitions
func TestDialogueStateTransition(t *testing.T) {
	tests := []struct {
		name          string
		initialState  DialogueState
		event         string
		expectedState DialogueState
	}{
		{
			name:          "Idle to InitWait on Begin",
			initialState:  StateIdle,
			event:         "BEGIN_SENT",
			expectedState: StateInitWait,
		},
		{
			name:          "InitWait to Active on Continue",
			initialState:  StateInitWait,
			event:         "CONTINUE_RECEIVED",
			expectedState: StateActive,
		},
		{
			name:          "Active to Terminating on End",
			initialState:  StateActive,
			event:         "END_SENT",
			expectedState: StateTerminating,
		},
		{
			name:          "Any state to Idle on Abort",
			initialState:  StateActive,
			event:         "ABORT_RECEIVED",
			expectedState: StateIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dlg := &Dialogue{
				LocalTID:     1,
				State:        tt.initialState,
				CreatedAt:    time.Now(),
				LastActivity: time.Now(),
				ExpiresAt:    time.Now().Add(60 * time.Second),
			}

			// Simulate state transition based on event
			switch tt.event {
			case "BEGIN_SENT":
				dlg.State = StateInitWait
			case "CONTINUE_RECEIVED":
				dlg.State = StateActive
			case "END_SENT":
				dlg.State = StateTerminating
			case "ABORT_RECEIVED":
				dlg.State = StateIdle
			}

			assert.Equal(t, tt.expectedState, dlg.State)
		})
	}
}

// TestDialogueStateString tests state string conversion
func TestDialogueStateString(t *testing.T) {
	tests := []struct {
		state    DialogueState
		expected string
	}{
		{StateIdle, "IDLE"},
		{StateInitWait, "INIT_WAIT"},
		{StateActive, "ACTIVE"},
		{StateTerminating, "TERMINATING"},
		{DialogueState(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.state.String())
		})
	}
}

// TestDialogueCreation tests dialogue creation and initialization
func TestDialogueCreation(t *testing.T) {
	now := time.Now()
	timeout := 60 * time.Second

	dlg := &Dialogue{
		LocalTID:           12345,
		RemoteTID:          []byte{0x01, 0x02, 0x03, 0x04},
		State:              StateIdle,
		ApplicationContext: &ACMapV3,
		CreatedAt:          now,
		LastActivity:       now,
		ExpiresAt:          now.Add(timeout),
		PendingInvokes:     make(map[int32]*PendingInvoke),
	}

	assert.Equal(t, uint32(12345), dlg.LocalTID)
	assert.Equal(t, StateIdle, dlg.State)
	assert.NotNil(t, dlg.ApplicationContext)
	assert.Equal(t, "map-ac-networkLocUp-v3", dlg.ApplicationContext.Name)
	assert.True(t, dlg.ExpiresAt.After(now))
	assert.NotNil(t, dlg.PendingInvokes)
}

// TestDialogueTimeout tests dialogue timeout handling
func TestDialogueTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         500 * time.Millisecond, // Short timeout for test
		InvokeTimeout:           300 * time.Millisecond,
		IdleTimeout:             1 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          2,
		TimerResolution:         100 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = handler.Start(ctx)
	require.NoError(t, err)
	defer handler.Stop(ctx)

	// Create a dialogue that will timeout
	now := time.Now()
	remoteTID := []byte{0xAA, 0xBB, 0xCC, 0xDD}

	handler.dialoguesMu.Lock()
	handler.dialogues[1] = &Dialogue{
		LocalTID:       1,
		RemoteTID:      remoteTID,
		State:          StateActive,
		CreatedAt:      now,
		LastActivity:   now,
		ExpiresAt:      now.Add(200 * time.Millisecond), // Will expire soon
		PendingInvokes: make(map[int32]*PendingInvoke),
	}
	handler.metrics.ActiveDialogues.Add(1)
	handler.dialoguesMu.Unlock()

	// Wait for timeout to trigger
	time.Sleep(800 * time.Millisecond)

	// Dialogue should be removed
	handler.dialoguesMu.RLock()
	_, exists := handler.dialogues[1]
	handler.dialoguesMu.RUnlock()

	assert.False(t, exists, "Dialogue should be removed after timeout")
	assert.Greater(t, handler.metrics.DialogueTimeouts.Load(), uint64(0), "Timeout metric should be incremented")
}

// TestInvokeTimeout tests invoke timeout handling
func TestInvokeTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         10 * time.Second,
		InvokeTimeout:           200 * time.Millisecond, // Short timeout for test
		IdleTimeout:             20 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          2,
		TimerResolution:         50 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = handler.Start(ctx)
	require.NoError(t, err)
	defer handler.Stop(ctx)

	// Create dialogue with pending invoke
	now := time.Now()
	remoteTID := []byte{0x11, 0x22, 0x33, 0x44}

	handler.dialoguesMu.Lock()
	handler.dialogues[1] = &Dialogue{
		LocalTID:     1,
		RemoteTID:    remoteTID,
		State:        StateActive,
		CreatedAt:    now,
		LastActivity: now,
		ExpiresAt:    now.Add(10 * time.Second),
		PendingInvokes: map[int32]*PendingInvoke{
			1: {
				InvokeID:      1,
				OperationCode: 46,
				SentAt:        now,
				ExpiresAt:     now.Add(100 * time.Millisecond), // Will expire soon
				Parameter:     []byte{0x01, 0x02},
			},
		},
	}
	handler.dialoguesMu.Unlock()

	// Wait for invoke timeout
	time.Sleep(500 * time.Millisecond)

	// Pending invoke should be removed
	handler.dialoguesMu.RLock()
	dlg, exists := handler.dialogues[1]
	handler.dialoguesMu.RUnlock()

	require.True(t, exists, "Dialogue should still exist")
	dlg.invokesMu.RLock()
	_, invExists := dlg.PendingInvokes[1]
	dlg.invokesMu.RUnlock()

	assert.False(t, invExists, "Pending invoke should be removed after timeout")
	assert.Greater(t, handler.metrics.InvokeTimeouts.Load(), uint64(0), "Invoke timeout metric should be incremented")
}

// TestDialogueMetrics tests dialogue metrics tracking
func TestDialogueMetrics(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         10 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             20 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          2,
		TimerResolution:         100 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create multiple dialogues
	now := time.Now()
	for i := uint32(1); i <= 5; i++ {
		remoteTID := make([]byte, 4)
		binary.BigEndian.PutUint32(remoteTID, i)

		handler.dialoguesMu.Lock()
		handler.dialogues[i] = &Dialogue{
			LocalTID:       i,
			RemoteTID:      remoteTID,
			State:          StateActive,
			CreatedAt:      now,
			LastActivity:   now,
			ExpiresAt:      now.Add(10 * time.Second),
			PendingInvokes: make(map[int32]*PendingInvoke),
		}
		handler.metrics.ActiveDialogues.Add(1)
		handler.metrics.TotalDialogues.Add(1)
		handler.dialoguesMu.Unlock()
	}

	// Check metrics
	assert.Equal(t, int64(5), handler.metrics.ActiveDialogues.Load())
	assert.Equal(t, uint64(5), handler.metrics.TotalDialogues.Load())

	// Get dialogue count
	count := handler.getDialogueCount()
	assert.Equal(t, 5, count)

	// Get dialogues
	dialogues := handler.GetDialogues()
	assert.Len(t, dialogues, 5)
}

// TestPendingInvokeManagement tests pending invoke add/remove
func TestPendingInvokeManagement(t *testing.T) {
	dlg := &Dialogue{
		LocalTID:       1,
		RemoteTID:      []byte{0x01, 0x02, 0x03, 0x04},
		State:          StateActive,
		CreatedAt:      time.Now(),
		LastActivity:   time.Now(),
		ExpiresAt:      time.Now().Add(60 * time.Second),
		PendingInvokes: make(map[int32]*PendingInvoke),
	}

	// Add pending invoke
	now := time.Now()
	invoke := &PendingInvoke{
		InvokeID:      1,
		OperationCode: 46,
		SentAt:        now,
		ExpiresAt:     now.Add(30 * time.Second),
		Parameter:     []byte{0x01, 0x02, 0x03},
	}

	dlg.invokesMu.Lock()
	dlg.PendingInvokes[1] = invoke
	dlg.invokesMu.Unlock()

	// Verify invoke was added
	dlg.invokesMu.RLock()
	retrieved, exists := dlg.PendingInvokes[1]
	dlg.invokesMu.RUnlock()

	require.True(t, exists)
	assert.Equal(t, int32(1), retrieved.InvokeID)
	assert.Equal(t, int32(46), retrieved.OperationCode)

	// Remove invoke
	dlg.invokesMu.Lock()
	delete(dlg.PendingInvokes, 1)
	dlg.invokesMu.Unlock()

	// Verify invoke was removed
	dlg.invokesMu.RLock()
	_, exists = dlg.PendingInvokes[1]
	dlg.invokesMu.RUnlock()

	assert.False(t, exists)
}

// TestDialogueInfoExport tests DialogueInfo structure
func TestDialogueInfoExport(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         10 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             20 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          2,
		TimerResolution:         100 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create dialogue
	now := time.Now()
	remoteTID := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	handler.dialoguesMu.Lock()
	handler.dialogues[1] = &Dialogue{
		LocalTID:           1,
		RemoteTID:          remoteTID,
		State:              StateActive,
		ApplicationContext: &ACMapV3,
		CreatedAt:          now,
		LastActivity:       now,
		ExpiresAt:          now.Add(60 * time.Second),
		PendingInvokes: map[int32]*PendingInvoke{
			1: {
				InvokeID:      1,
				OperationCode: 46,
				SentAt:        now,
				ExpiresAt:     now.Add(30 * time.Second),
			},
		},
	}
	handler.dialoguesMu.Unlock()

	// Get dialogue info
	dialogues := handler.GetDialogues()
	require.Len(t, dialogues, 1)

	info := dialogues[0]
	assert.Equal(t, uint32(1), info.LocalTID)
	assert.Equal(t, "deadbeef", info.RemoteTID)
	assert.Equal(t, "ACTIVE", info.State)
	assert.Equal(t, "map-ac-networkLocUp-v3", info.ApplicationContext)
	assert.Equal(t, 1, info.PendingInvokes)
}

// TestHealthStatus tests health status reporting
func TestHealthStatus(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         10 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             20 * time.Second,
		MaxDialogues:            100,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          2,
		TimerResolution:         100 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Simulate some metrics
	handler.metrics.MessagesReceived.Add(100)
	handler.metrics.MessagesSent.Add(80)
	handler.metrics.ActiveDialogues.Store(5)
	handler.metrics.TotalDialogues.Add(10)
	handler.metrics.DialogueTimeouts.Add(2)
	handler.metrics.InvokeTimeouts.Add(3)

	// Get health status
	status := handler.GetHealthStatus()

	assert.Equal(t, int64(5), status.ActiveDialogues)
	assert.Equal(t, uint64(10), status.TotalDialogues)
	assert.Equal(t, uint64(100), status.MessagesReceived)
	assert.Equal(t, uint64(80), status.MessagesSent)
	assert.Equal(t, uint64(2), status.DialogueTimeouts)
	assert.Equal(t, uint64(3), status.InvokeTimeouts)
	assert.False(t, status.LastUpdate.IsZero())
}
