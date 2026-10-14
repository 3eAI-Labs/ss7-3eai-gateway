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

// Test TCAP parser with Begin message
func TestParseTCAPMessage_Begin(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// A TCAP Begin message as a peer would send it
	otid := uint32(0x12345678)
	invID := 1
	opCode := 46 // MAP UpdateLocation
	payload := []byte{0x30, 0x0A, 0x04, 0x08, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}

	tcapMsg := beginInvoke(otid, invID, opCode, payload)
	data, err := EncodeMessage(tcapMsg)
	require.NoError(t, err)

	// Parse the message
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	// Verify message type
	assert.Equal(t, MsgTypeBegin, parsed.MessageType)

	// Verify OTID
	require.NotNil(t, parsed.OriginatingTID)
	assert.Equal(t, otid, binary.BigEndian.Uint32(parsed.OriginatingTID))

	// Verify components
	require.Len(t, parsed.Components, 1)
	comp := parsed.Components[0]
	assert.Equal(t, CompTypeInvoke, comp.Type)
	require.NotNil(t, comp.InvokeID)
	assert.Equal(t, int32(invID), *comp.InvokeID)
	require.NotNil(t, comp.OperationCode)
	assert.Equal(t, int32(opCode), *comp.OperationCode)
	assert.Equal(t, payload, comp.Parameter)
}

// Test TCAP parser with End message
func TestParseTCAPMessage_End(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create TCAP End message with ReturnResult
	dtid := uint32(0x87654321)
	invID := 1
	opCode := 46
	payload := []byte{0x30, 0x05, 0x04, 0x03, 0x01, 0x02, 0x03}

	tcapMsg := endReturnResult(dtid, invID, opCode, payload)
	data, err := EncodeMessage(tcapMsg)
	require.NoError(t, err)

	// Parse the message
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	// Verify message type
	assert.Equal(t, MsgTypeEnd, parsed.MessageType)

	// Verify DTID
	require.NotNil(t, parsed.DestinationTID)
	assert.Equal(t, dtid, binary.BigEndian.Uint32(parsed.DestinationTID))

	// Verify components
	require.Len(t, parsed.Components, 1)
	comp := parsed.Components[0]
	assert.Equal(t, CompTypeReturnResult, comp.Type)
	require.NotNil(t, comp.InvokeID)
	assert.Equal(t, int32(invID), *comp.InvokeID)
}

// Test TCAP parser with Continue message
func TestParseTCAPMessage_Continue(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create TCAP Continue message
	otid := uint32(0x11111111)
	dtid := uint32(0x22222222)
	invID := 2
	opCode := 58 // MAP SendRoutingInfo
	payload := []byte{0x04, 0x07, 0x91, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	tcapMsg := continueInvoke(otid, dtid, invID, opCode, payload)
	data, err := EncodeMessage(tcapMsg)
	require.NoError(t, err)

	// Parse the message
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	// Verify message type
	assert.Equal(t, MsgTypeContinue, parsed.MessageType)

	// Verify TIDs
	require.NotNil(t, parsed.OriginatingTID)
	assert.Equal(t, otid, binary.BigEndian.Uint32(parsed.OriginatingTID))
	require.NotNil(t, parsed.DestinationTID)
	assert.Equal(t, dtid, binary.BigEndian.Uint32(parsed.DestinationTID))

	// Verify components
	require.Len(t, parsed.Components, 1)
	comp := parsed.Components[0]
	assert.Equal(t, CompTypeInvoke, comp.Type)
	require.NotNil(t, comp.InvokeID)
	assert.Equal(t, int32(invID), *comp.InvokeID)
}

// Test TCAP encoder with Begin message
func TestEncodeTCAPMessage_Begin(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create internal message
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0xABCDEF01)

	invID := int32(5)
	opCode := int32(46)
	payload := []byte{0x30, 0x03, 0x01, 0x02, 0x03}

	msg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		Components: []*Component{
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID,
				OperationCode: &opCode,
				Parameter:     payload,
			},
		},
	}

	// Encode the message
	data, err := handler.encodeTCAPMessage(msg)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Parse back to verify
	parsed, err := ParseMessage(data)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	// Verify OTID
	assert.Equal(t, uint32(0xABCDEF01), binary.BigEndian.Uint32(parsed.OriginatingTID))

	// Verify components
	require.NotNil(t, parsed.Components)
	require.Len(t, parsed.Components, 1)
}

// Test TCAP encoder with End message
func TestEncodeTCAPMessage_End(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create internal message
	dtid := make([]byte, 4)
	binary.BigEndian.PutUint32(dtid, 0xDEADBEEF)

	invID := int32(3)
	opCode := int32(46)
	payload := []byte{0x04, 0x01, 0x00}

	msg := &Message{
		MessageType:    MsgTypeEnd,
		DestinationTID: dtid,
		Components: []*Component{
			{
				Type:          CompTypeReturnResult,
				InvokeID:      &invID,
				OperationCode: &opCode,
				Parameter:     payload,
			},
		},
	}

	// Encode the message
	data, err := handler.encodeTCAPMessage(msg)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Parse back to verify
	parsed, err := ParseMessage(data)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	// Verify DTID
	assert.Equal(t, uint32(0xDEADBEEF), binary.BigEndian.Uint32(parsed.DestinationTID))
}

// Test TCAP round-trip (encode -> parse -> encode)
func TestTCAPRoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	// Create original message
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x12345678)

	invID := int32(10)
	opCode := int32(59) // MAP SendRoutingInfoForSM
	payload := []byte{0x04, 0x08, 0x91, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}

	originalMsg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		Components: []*Component{
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID,
				OperationCode: &opCode,
				Parameter:     payload,
			},
		},
	}

	// Encode
	encoded, err := handler.encodeTCAPMessage(originalMsg)
	require.NoError(t, err)

	// Parse
	parsed, err := handler.parseTCAPMessage(encoded)
	require.NoError(t, err)

	// Verify parsed matches original
	assert.Equal(t, originalMsg.MessageType, parsed.MessageType)
	assert.Equal(t, originalMsg.OriginatingTID, parsed.OriginatingTID)
	require.Len(t, parsed.Components, 1)
	assert.Equal(t, *originalMsg.Components[0].InvokeID, *parsed.Components[0].InvokeID)
	assert.Equal(t, *originalMsg.Components[0].OperationCode, *parsed.Components[0].OperationCode)
	assert.Equal(t, originalMsg.Components[0].Parameter, parsed.Components[0].Parameter)

	// Encode again
	reEncoded, err := handler.encodeTCAPMessage(parsed)
	require.NoError(t, err)

	// Should produce same bytes
	assert.Equal(t, encoded, reEncoded)
}

// Test invalid message parsing
func TestParseTCAPMessage_Invalid(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	tests := []struct {
		name  string
		input []byte
	}{
		{"nil input", nil},
		{"empty bytes", []byte{}},
		{"single byte", []byte{0x62}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := handler.parseTCAPMessage(tt.input)
			assert.Error(t, err)
		})
	}
}

// Test encoding nil message
func TestEncodeTCAPMessage_Nil(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1 * time.Second,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	_, err = handler.encodeTCAPMessage(nil)
	assert.Error(t, err)
	assert.Equal(t, ErrInvalidMessage, err)
}

// Test timer manager creation
func TestTimerManager_Creation(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         60 * time.Second,
		InvokeTimeout:           30 * time.Second,
		IdleTimeout:             120 * time.Second,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         100 * time.Millisecond,
	}

	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)
	require.NotNil(t, handler.timerManager)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Start handler
	err = handler.Start(ctx)
	require.NoError(t, err)

	// Let it run briefly
	time.Sleep(300 * time.Millisecond)

	// Stop handler
	err = handler.Stop(ctx)
	require.NoError(t, err)
}
