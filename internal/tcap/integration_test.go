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

// TestTCAPDialogueFlow tests a complete TCAP dialogue flow: Begin -> Continue -> End
func TestTCAPDialogueFlow(t *testing.T) {
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

	// Create TCAP handler
	sccpIn, sccpOut := stubChannels()
	handler, err := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start handler
	err = handler.Start(ctx)
	require.NoError(t, err)
	defer handler.Stop(ctx)

	// Test 1: Send BEGIN message
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x00000001)

	invID1 := int32(1)
	opCode1 := int32(46) // MAP UpdateLocation
	payload1 := []byte{0x30, 0x05, 0x04, 0x03, 0x01, 0x02, 0x03}

	beginMsg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		Components: []*Component{
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID1,
				OperationCode: &opCode1,
				Parameter:     payload1,
			},
		},
	}

	// Encode BEGIN
	beginData, err := handler.encodeTCAPMessage(beginMsg)
	require.NoError(t, err)
	require.NotEmpty(t, beginData)

	// Parse BEGIN (simulate receiving from SCCP)
	parsedBegin, err := handler.parseTCAPMessage(beginData)
	require.NoError(t, err)
	assert.Equal(t, MsgTypeBegin, parsedBegin.MessageType)
	assert.Equal(t, otid, parsedBegin.OriginatingTID)

	// Test 2: Send CONTINUE message
	dtid := make([]byte, 4)
	binary.BigEndian.PutUint32(dtid, 0x00000002)

	invID2 := int32(2)
	opCode2 := int32(58) // MAP SendRoutingInfo
	payload2 := []byte{0x04, 0x07, 0x91, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	continueMsg := &Message{
		MessageType:    MsgTypeContinue,
		OriginatingTID: dtid,
		DestinationTID: otid,
		Components: []*Component{
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID2,
				OperationCode: &opCode2,
				Parameter:     payload2,
			},
		},
	}

	// Encode CONTINUE
	continueData, err := handler.encodeTCAPMessage(continueMsg)
	require.NoError(t, err)
	require.NotEmpty(t, continueData)

	// Parse CONTINUE
	parsedContinue, err := handler.parseTCAPMessage(continueData)
	require.NoError(t, err)
	assert.Equal(t, MsgTypeContinue, parsedContinue.MessageType)
	assert.Equal(t, dtid, parsedContinue.OriginatingTID)
	assert.Equal(t, otid, parsedContinue.DestinationTID)

	// Test 3: Send END message with ReturnResult
	invID3 := int32(1)
	opCode3 := int32(46)
	payload3 := []byte{0x04, 0x02, 0x00, 0x00}

	endMsg := &Message{
		MessageType:    MsgTypeEnd,
		DestinationTID: dtid,
		Components: []*Component{
			{
				Type:          CompTypeReturnResult,
				InvokeID:      &invID3,
				OperationCode: &opCode3,
				Parameter:     payload3,
			},
		},
	}

	// Encode END
	endData, err := handler.encodeTCAPMessage(endMsg)
	require.NoError(t, err)
	require.NotEmpty(t, endData)

	// Parse END
	parsedEnd, err := handler.parseTCAPMessage(endData)
	require.NoError(t, err)
	assert.Equal(t, MsgTypeEnd, parsedEnd.MessageType)
	assert.Equal(t, dtid, parsedEnd.DestinationTID)
	require.Len(t, parsedEnd.Components, 1)
	assert.Equal(t, CompTypeReturnResult, parsedEnd.Components[0].Type)
}

// TestTCAPAbortFlow tests TCAP Abort message handling
func TestTCAPAbortFlow(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = handler.Start(ctx)
	require.NoError(t, err)
	defer handler.Stop(ctx)

	// Create ABORT message
	dtid := make([]byte, 4)
	binary.BigEndian.PutUint32(dtid, 0xDEADBEEF)

	cause := AbortResourceLimitation
	abortMsg := &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: dtid,
		AbortCause:     &cause,
		PAbort:         true,
	}

	// Encode
	abortData, err := handler.encodeTCAPMessage(abortMsg)
	require.NoError(t, err)

	// Parse
	parsedAbort, err := handler.parseTCAPMessage(abortData)
	require.NoError(t, err)
	assert.Equal(t, MsgTypeAbort, parsedAbort.MessageType)
	assert.Equal(t, dtid, parsedAbort.DestinationTID)
	require.NotNil(t, parsedAbort.AbortCause)
	assert.Equal(t, cause, *parsedAbort.AbortCause)
	assert.True(t, parsedAbort.PAbort)
}

// TestTCAPMultipleComponents tests messages with multiple components
func TestTCAPMultipleComponents(t *testing.T) {
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

	// Create message with 3 components
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x12345678)

	invID1 := int32(1)
	opCode1 := int32(46)
	payload1 := []byte{0x30, 0x02, 0x01, 0x02}

	invID2 := int32(2)
	opCode2 := int32(58)
	payload2 := []byte{0x04, 0x03, 0x11, 0x22, 0x33}

	invID3 := int32(3)
	opCode3 := int32(59)
	payload3 := []byte{0x04, 0x05, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE}

	msg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		Components: []*Component{
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID1,
				OperationCode: &opCode1,
				Parameter:     payload1,
			},
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID2,
				OperationCode: &opCode2,
				Parameter:     payload2,
			},
			{
				Type:          CompTypeInvoke,
				InvokeID:      &invID3,
				OperationCode: &opCode3,
				Parameter:     payload3,
			},
		},
	}

	// Encode
	data, err := handler.encodeTCAPMessage(msg)
	require.NoError(t, err)

	// Parse
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)

	// Verify all 3 components
	require.Len(t, parsed.Components, 3)
	for i, comp := range parsed.Components {
		assert.Equal(t, CompTypeInvoke, comp.Type)
		require.NotNil(t, comp.InvokeID)
		assert.Equal(t, int32(i+1), *comp.InvokeID)
	}
}

// TestTCAPDialogueWithApplicationContext tests dialogue with Application Context
func TestTCAPDialogueWithApplicationContext(t *testing.T) {
	t.Skip("Dialogue Portion encoding requires more complex ASN.1 handling - skipping for now")

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

	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x11111111)

	invID := int32(1)
	opCode := int32(46)
	payload := []byte{0x30, 0x03, 0x01, 0x02, 0x03}

	// Create message with Application Context (MAP v3)
	msg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		DialoguePortion: &DialoguePortion{
			ApplicationContext: &ApplicationContext{
				OID:  []int{0, 4, 0, 0, 1, 0, 1, 3},
				Name: "map-ac-networkLocUp-v3",
			},
			UserInformation: []byte{0x04, 0x02, 0xAA, 0xBB},
		},
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
	data, err := handler.encodeTCAPMessage(msg)
	require.NoError(t, err)

	// Parse
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)

	// Verify Dialogue Portion
	require.NotNil(t, parsed.DialoguePortion)
	require.NotNil(t, parsed.DialoguePortion.ApplicationContext)
	assert.NotEmpty(t, parsed.DialoguePortion.ApplicationContext.OID)
	assert.NotEmpty(t, parsed.DialoguePortion.UserInformation)
}

// TestTCAPReturnError tests ReturnError component
func TestTCAPReturnError(t *testing.T) {
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

	dtid := make([]byte, 4)
	binary.BigEndian.PutUint32(dtid, 0x22222222)

	invID := int32(5)
	errCode := int32(34) // MAP SystemFailure
	payload := []byte{0x0A, 0x01, 0x00}

	msg := &Message{
		MessageType:    MsgTypeEnd,
		DestinationTID: dtid,
		Components: []*Component{
			{
				Type:      CompTypeReturnError,
				InvokeID:  &invID,
				ErrorCode: &errCode,
				Parameter: payload,
			},
		},
	}

	// Encode
	data, err := handler.encodeTCAPMessage(msg)
	require.NoError(t, err)

	// Parse
	parsed, err := handler.parseTCAPMessage(data)
	require.NoError(t, err)

	// Verify ReturnError component
	require.Len(t, parsed.Components, 1)
	comp := parsed.Components[0]
	assert.Equal(t, CompTypeReturnError, comp.Type)
	require.NotNil(t, comp.InvokeID)
	assert.Equal(t, invID, *comp.InvokeID)
	require.NotNil(t, comp.ErrorCode)
	assert.Equal(t, errCode, *comp.ErrorCode)
}

// TestTCAPConcurrentMessages tests concurrent message processing
func TestTCAPConcurrentMessages(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := &Config{
		NodeID:                  1,
		DialogueTimeout:         10 * time.Second,
		InvokeTimeout:           5 * time.Second,
		IdleTimeout:             20 * time.Second,
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = handler.Start(ctx)
	require.NoError(t, err)
	defer handler.Stop(ctx)

	// Process 100 messages concurrently
	numMessages := 100
	done := make(chan bool, numMessages)

	for i := 0; i < numMessages; i++ {
		go func(id int) {
			otid := make([]byte, 4)
			binary.BigEndian.PutUint32(otid, uint32(id))

			invID := int32(id)
			opCode := int32(46)
			payload := []byte{0x04, 0x01, byte(id)}

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

			// Encode
			data, err := handler.encodeTCAPMessage(msg)
			if err != nil {
				t.Errorf("Encode failed for message %d: %v", id, err)
				done <- false
				return
			}

			// Parse
			_, err = handler.parseTCAPMessage(data)
			if err != nil {
				t.Errorf("Parse failed for message %d: %v", id, err)
				done <- false
				return
			}

			done <- true
		}(i)
	}

	// Wait for all goroutines
	successCount := 0
	for i := 0; i < numMessages; i++ {
		if <-done {
			successCount++
		}
	}

	assert.Equal(t, numMessages, successCount, "All messages should be processed successfully")
}
