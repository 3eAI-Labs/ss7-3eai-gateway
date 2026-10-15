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

func sessionTestHandler(t *testing.T, timeout time.Duration) *Handler {
	t.Helper()
	logger := zaptest.NewLogger(t)
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: timeout, InvokeTimeout: timeout, IdleTimeout: 5 * time.Second,
		MaxDialogues: 100, MaxComponentsPerMessage: 10, MaxInvokesPerDialogue: 20,
		ChannelBufferSize: 100, WorkerPoolSize: 1, TimerResolution: 20 * time.Millisecond,
	}, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	return h
}

// A dialogue the peer never answered times out without an ABORT — no
// transaction ID could address it — and the lost sink is told, whoever
// opened it.
func TestDialogueTimeoutWithoutPeerTID(t *testing.T) {
	h := sessionTestHandler(t, 100*time.Millisecond)
	var mu sync.Mutex
	var lost []FailureReason
	h.SetLostSink(func(d *Dialogue, r FailureReason, _ string) {
		mu.Lock()
		defer mu.Unlock()
		lost = append(lost, r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, h.Start(ctx))
	defer h.Stop(ctx)

	called, calling := sampleAddrs()
	_, err := h.Originate(&ACMapV3, called, calling, 45, []byte{0x30, 0x00}, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(lost) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, FailureTimeout, lost[0])
	assert.Len(t, h.sccpOutgoingCh, 1, "the BEGIN only: no ABORT without the peer's TID")
}

// EndLocally drops the dialogue without sending anything.
func TestEndLocally(t *testing.T) {
	h := sessionTestHandler(t, time.Minute)
	called, calling := sampleAddrs()
	d, err := h.Originate(&ACMapV3, called, calling, 45, []byte{0x30, 0x00}, "")
	require.NoError(t, err)
	h.EndLocally(d)
	assert.Nil(t, h.getDialogue(d.LocalTID))
	assert.Len(t, h.appOutgoingCh, 1, "the BEGIN only")
}

// PeerTID may be read from another goroutine — a USSD session's — while a
// CONTINUE sets the peer's TID (go test -race).
func TestPeerTIDWhileContinueArrives(t *testing.T) {
	h := sessionTestHandler(t, time.Minute)
	called, calling := sampleAddrs()
	d, err := h.Originate(&ACMapV3, called, calling, 45, []byte{0x30, 0x00}, "")
	require.NoError(t, err)

	stop := make(chan struct{})
	done := make(chan []byte)
	go func() {
		var last []byte
		for {
			select {
			case <-stop:
				done <- last
				return
			default:
				last = d.PeerTID()
			}
		}
	}()
	called2, calling2 := sampleAddrs()
	h.handleContinue(&sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, CalledAddress: calling2, CallingAddress: called2}, &Message{MessageType: MsgTypeContinue,
		OriginatingTID: tid32(0x0A0B0C0D), DestinationTID: tid32(d.LocalTID)})
	close(stop)
	<-done
	assert.Equal(t, tid32(0x0A0B0C0D), d.PeerTID())
}
