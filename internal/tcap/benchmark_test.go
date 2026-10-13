// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"encoding/binary"
	"testing"

	gotcap "github.com/wmnsk/go-tcap"
	"go.uber.org/zap/zaptest"
)

// BenchmarkParseTCAPBegin benchmarks parsing TCAP Begin messages
func BenchmarkParseTCAPBegin(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test data
	otid := uint32(0x12345678)
	invID := 1
	opCode := 46
	payload := []byte{0x30, 0x0A, 0x04, 0x08, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}

	tcapMsg := gotcap.NewBeginInvoke(otid, invID, opCode, payload)
	data, _ := tcapMsg.MarshalBinary()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseTCAPEnd benchmarks parsing TCAP End messages
func BenchmarkParseTCAPEnd(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test data
	dtid := uint32(0x87654321)
	invID := 1
	opCode := 46
	payload := []byte{0x30, 0x05, 0x04, 0x03, 0x01, 0x02, 0x03}

	tcapMsg := gotcap.NewEndReturnResult(dtid, invID, opCode, true, payload)
	data, _ := tcapMsg.MarshalBinary()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseTCAPContinue benchmarks parsing TCAP Continue messages
func BenchmarkParseTCAPContinue(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test data
	otid := uint32(0x11111111)
	dtid := uint32(0x22222222)
	invID := 2
	opCode := 58
	payload := []byte{0x04, 0x07, 0x91, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	tcapMsg := gotcap.NewContinueInvoke(otid, dtid, invID, opCode, payload)
	data, _ := tcapMsg.MarshalBinary()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeTCAPBegin benchmarks encoding TCAP Begin messages
func BenchmarkEncodeTCAPBegin(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test message
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

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.encodeTCAPMessage(msg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeTCAPEnd benchmarks encoding TCAP End messages
func BenchmarkEncodeTCAPEnd(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test message
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

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.encodeTCAPMessage(msg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTCAPRoundTrip benchmarks full round-trip (encode -> parse)
func BenchmarkTCAPRoundTrip(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create test message
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x12345678)

	invID := int32(10)
	opCode := int32(59)
	payload := []byte{0x04, 0x08, 0x91, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}

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

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Encode
		encoded, err := handler.encodeTCAPMessage(msg)
		if err != nil {
			b.Fatal(err)
		}

		// Parse
		_, err = handler.parseTCAPMessage(encoded)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTCAPMultipleComponents benchmarks parsing messages with multiple components
func BenchmarkTCAPMultipleComponents(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	// Create message with 5 components
	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x12345678)

	components := make([]*Component, 5)
	for i := 0; i < 5; i++ {
		invID := int32(i + 1)
		opCode := int32(46 + i)
		payload := []byte{0x04, 0x02, byte(i), byte(i + 1)}

		components[i] = &Component{
			Type:          CompTypeInvoke,
			InvokeID:      &invID,
			OperationCode: &opCode,
			Parameter:     payload,
		}
	}

	msg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: otid,
		Components:     components,
	}

	// Pre-encode for benchmark
	data, _ := handler.encodeTCAPMessage(msg)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTCAPSmallPayload benchmarks with small payload
func BenchmarkTCAPSmallPayload(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x11111111)

	invID := int32(1)
	opCode := int32(46)
	payload := []byte{0x01} // Minimal payload

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

	data, _ := handler.encodeTCAPMessage(msg)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTCAPLargePayload benchmarks with large payload
func BenchmarkTCAPLargePayload(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := getDefaultBenchmarkConfig()
	sccpIn, sccpOut := stubChannels()
	handler, _ := NewHandler(config, sccpIn, sccpOut, NewDispatcher(logger), logger)

	otid := make([]byte, 4)
	binary.BigEndian.PutUint32(otid, 0x22222222)

	invID := int32(1)
	opCode := int32(46)
	// Use moderate size payload (100 bytes instead of 256)
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte(i)
	}

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

	data, _ := handler.encodeTCAPMessage(msg)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := handler.parseTCAPMessage(data)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseOID benchmarks OID parsing
func BenchmarkParseOID(b *testing.B) {
	testData := []byte{0x28, 0x04, 0x00, 0x00, 0x01, 0x00, 0x01, 0x03}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parseOID(testData)
	}
}

// Helper function to get default benchmark config
func getDefaultBenchmarkConfig() *Config {
	return &Config{
		NodeID:                  1,
		DialogueTimeout:         60,
		InvokeTimeout:           30,
		IdleTimeout:             120,
		MaxDialogues:            1000,
		MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue:   20,
		ChannelBufferSize:       100,
		WorkerPoolSize:          4,
		TimerResolution:         1,
	}
}
