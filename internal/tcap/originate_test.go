// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// Originate is the public BEGIN-origination API used by NATS bridges and
// other composition roots that need to drive an outbound MAP/CAP transaction.
// These tests pin its observable contract: dialogue registered before the
// message ships, message landed on the outbound channel with correct shape,
// validation of mandatory inputs, and rollback on channel-full.

func newOriginateTestHandler(t *testing.T) (*Handler, chan *Message) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	cfg := &Config{
		NodeID:            1,
		DialogueTimeout:   60 * time.Second,
		InvokeTimeout:     30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxDialogues:      100,
		ChannelBufferSize: 4,
		WorkerPoolSize:    1,
		TimerResolution:   1 * time.Second,
	}
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(cfg, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	return h, h.appOutgoingCh
}

func sampleAddrs() (called, calling *sccp.SccpAddress) {
	called = &sccp.SccpAddress{
		AddressIndicator: sccp.AIGTPresent | sccp.AISSNPresent,
		PointCode:        100,
		SubsystemNumber:  sccp.SSNHLR,
		GlobalTitle: &sccp.GlobalTitle{
			GTI:           sccp.GTI0100,
			NumberingPlan: sccp.NPISDN,
			NAI:           sccp.NAIInternational,
			Address:       "905001112233",
		},
	}
	calling = &sccp.SccpAddress{
		AddressIndicator: sccp.AISSNPresent | sccp.AIPCPresent,
		PointCode:        200,
		SubsystemNumber:  sccp.SSNMAP,
	}
	return
}

func TestOriginate_Happy(t *testing.T) {
	h, outCh := newOriginateTestHandler(t)
	called, calling := sampleAddrs()

	const opCode int32 = 45 // SRI-for-SM
	param := []byte{0xAA, 0xBB, 0xCC}
	corrID := "corr-abc-123"

	dlg, err := h.Originate(&ACMapV3, called, calling, opCode, param, corrID)
	require.NoError(t, err)
	require.NotNil(t, dlg)

	// Dialogue contract.
	assert.Equal(t, opCode, dlg.OriginatingOpCode)
	assert.Equal(t, corrID, dlg.NATSCorrelationID)
	assert.Equal(t, StateInitWait, dlg.State)
	assert.Same(t, calling, dlg.LocalAddress, "calling pinned as Local")
	assert.Same(t, called, dlg.RemoteAddress, "called pinned as Remote")
	assert.NotZero(t, dlg.LocalTID, "TID must be allocated")
	assert.Equal(t, uint8(1), uint8(dlg.LocalTID>>24), "node id encoded in upper 8 bits")

	// Pending invoke registered for response correlation.
	dlg.invokesMu.RLock()
	pi, ok := dlg.PendingInvokes[1]
	dlg.invokesMu.RUnlock()
	require.True(t, ok, "invoke 1 should be pending")
	assert.Equal(t, opCode, pi.OperationCode)

	// Dialogue registered in handler map (so inbound END can find it).
	got := h.getDialogue(dlg.LocalTID)
	require.NotNil(t, got)
	assert.Same(t, dlg, got)

	// Outbound message shape.
	select {
	case msg := <-outCh:
		assert.Equal(t, MsgTypeBegin, msg.MessageType)
		require.NotNil(t, msg.DialoguePortion)
		require.NotNil(t, msg.DialoguePortion.ApplicationContext)
		assert.Equal(t, ACMapV3.OID, msg.DialoguePortion.ApplicationContext.OID)
		assert.Same(t, dlg, msg.Dialogue, "msg.Dialogue back-reference set so handleOutgoingMessage can pull addresses")
		require.Len(t, msg.Components, 1)
		comp := msg.Components[0]
		assert.Equal(t, CompTypeInvoke, comp.Type)
		require.NotNil(t, comp.OperationCode)
		assert.Equal(t, opCode, *comp.OperationCode)
		assert.Equal(t, param, comp.Parameter)
	case <-time.After(time.Second):
		t.Fatal("outbound message never arrived")
	}
}

func TestOriginate_Validation(t *testing.T) {
	h, _ := newOriginateTestHandler(t)
	called, calling := sampleAddrs()

	t.Run("nil ac", func(t *testing.T) {
		_, err := h.Originate(nil, called, calling, 45, nil, "")
		assert.Error(t, err)
	})
	t.Run("nil called", func(t *testing.T) {
		_, err := h.Originate(&ACMapV3, nil, calling, 45, nil, "")
		assert.Error(t, err)
	})
	t.Run("nil calling", func(t *testing.T) {
		_, err := h.Originate(&ACMapV3, called, nil, 45, nil, "")
		assert.Error(t, err)
	})
}

func TestOriginate_ChannelFullRollsBackDialogue(t *testing.T) {
	h, outCh := newOriginateTestHandler(t)
	called, calling := sampleAddrs()

	// Saturate the outbound channel (capacity = 4 from cfg).
	for i := 0; i < cap(outCh); i++ {
		outCh <- &Message{MessageType: MsgTypeBegin}
	}

	before := h.getDialogueCount()
	_, err := h.Originate(&ACMapV3, called, calling, 45, nil, "saturated")
	require.Error(t, err)

	// Critical: a failed originate must NOT leave a stale dialogue on the
	// map, otherwise the next allocateTID could collide and the dialogue
	// table would leak entries on every backpressure event.
	assert.Equal(t, before, h.getDialogueCount(), "channel-full must roll back dialogue registration")
}

func TestOriginate_MaxDialoguesEnforced(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := &Config{
		NodeID:            1,
		DialogueTimeout:   60 * time.Second,
		InvokeTimeout:     30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxDialogues:      1, // cap
		ChannelBufferSize: 16,
		WorkerPoolSize:    1,
		TimerResolution:   1 * time.Second,
	}
	sccpIn, sccpOut := stubChannels()
	h, err := NewHandler(cfg, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	called, calling := sampleAddrs()

	_, err = h.Originate(&ACMapV3, called, calling, 45, nil, "first")
	require.NoError(t, err)

	_, err = h.Originate(&ACMapV3, called, calling, 45, nil, "second")
	assert.ErrorIs(t, err, ErrDialogueLimitReached)
}
