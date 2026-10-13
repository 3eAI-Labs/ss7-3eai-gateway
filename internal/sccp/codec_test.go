// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUDTRoundTrip_ITU14 exercises the ITU 14-bit PC wire path:
// encode SccpMessage → bytes → ParseSCCP → SccpMessage. Asserts every
// field survives intact, including PCs at the upper edge of the 14-bit
// range. Catches the off-by-3 pointer-formula class of bug TIER 1 Item 4
// originally surfaced.
func TestUDTRoundTrip_ITU14(t *testing.T) {
	// Use PCs at the 14-bit edge (0x3FFF = 16383) — proves the codec
	// handles maximum legal values without truncating into SSN.
	in := &SccpMessage{
		MessageType:   MsgTypeUDT,
		ProtocolClass: ClassBasicConnectionless,
		CalledAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x3FFF,
			SubsystemNumber:  SSNHLR,
		},
		CallingAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x0102,
			SubsystemNumber:  SSNMAP,
		},
		Data: []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x12, 0x34, 0x56, 0x78},
	}

	wire, err := EncodeSCCP(in, VariantITU)
	require.NoError(t, err)

	out, err := ParseSCCP(wire, VariantITU)
	require.NoError(t, err)

	assert.Equal(t, in.MessageType, out.MessageType)
	assert.Equal(t, in.ProtocolClass, out.ProtocolClass)
	require.NotNil(t, out.CalledAddress, "CdPA must be parsed (was silently dropped before TIER 1/4 pointer fix)")
	assert.Equal(t, in.CalledAddress.PointCode, out.CalledAddress.PointCode)
	assert.Equal(t, in.CalledAddress.SubsystemNumber, out.CalledAddress.SubsystemNumber)
	require.NotNil(t, out.CallingAddress)
	assert.Equal(t, in.CallingAddress.PointCode, out.CallingAddress.PointCode)
	assert.Equal(t, in.CallingAddress.SubsystemNumber, out.CallingAddress.SubsystemNumber)
	assert.Equal(t, in.Data, out.Data)
}

// TestUDTRoundTrip_ANSI24 proves 24-bit PCs round-trip without truncation.
// Before the variant-aware codec, a PC like 0x010101 would be silently
// written as 0x0101 on the wire — gateway's isLocalAddress comparison
// then failed on response paths, producing infinite SCCP routing loops.
func TestUDTRoundTrip_ANSI24(t *testing.T) {
	in := &SccpMessage{
		MessageType:   MsgTypeUDT,
		ProtocolClass: ClassBasicConnectionless,
		CalledAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x010101, // 1-1-1, the docs example
			SubsystemNumber:  SSNHLR,
		},
		CallingAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0xABCDEF, // exercises all three octets
			SubsystemNumber:  SSNMAP,
		},
		Data: []byte{0x01, 0x02, 0x03},
	}

	wire, err := EncodeSCCP(in, VariantANSI)
	require.NoError(t, err)

	out, err := ParseSCCP(wire, VariantANSI)
	require.NoError(t, err)

	require.NotNil(t, out.CalledAddress)
	assert.Equal(t, uint32(0x010101), out.CalledAddress.PointCode,
		"ANSI 24-bit PC must round-trip without truncation")
	require.NotNil(t, out.CallingAddress)
	assert.Equal(t, uint32(0xABCDEF), out.CallingAddress.PointCode,
		"upper octet of ANSI PC must survive encode→decode")
	assert.Equal(t, in.Data, out.Data)
}

// TestEncode_ITU14_TruncatesOver14Bits documents the safety guarantee:
// a misconfigured 24-bit PC fed to the ITU codec drops its high bits
// instead of corrupting adjacent SSN/GT octets. Mismatch is still a bug
// (variant must match peer), but at least it fails predictably.
func TestEncode_ITU14_TruncatesOver14Bits(t *testing.T) {
	in := &SccpMessage{
		MessageType:   MsgTypeUDT,
		ProtocolClass: ClassBasicConnectionless,
		CalledAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x010101, // > 14 bits
			SubsystemNumber:  SSNHLR,
		},
		CallingAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x0102,
			SubsystemNumber:  SSNMAP,
		},
		Data: []byte{0xAA},
	}

	wire, err := EncodeSCCP(in, VariantITU)
	require.NoError(t, err)

	out, err := ParseSCCP(wire, VariantITU)
	require.NoError(t, err)

	require.NotNil(t, out.CalledAddress)
	assert.Equal(t, uint32(0x010101)&VariantITU.PCMask(), out.CalledAddress.PointCode,
		"misconfigured >14-bit PC under ITU variant must mask to 14 bits, "+
			"not corrupt the next byte (which would parse as a wrong SSN)")
	assert.Equal(t, SSNHLR, out.CalledAddress.SubsystemNumber,
		"SSN must remain intact even when PC was over-sized")
}

// TestVariantMismatch_ITUReadingANSI documents the failure mode operators
// see when their variant config doesn't match the peer. We do NOT crash —
// we silently produce a wrong PC. The codec is correct; the diagnosis is
// that the caller chose the wrong variant. Unit test pins the behavior.
func TestVariantMismatch_ITUReadingANSI(t *testing.T) {
	in := &SccpMessage{
		MessageType:   MsgTypeUDT,
		ProtocolClass: ClassBasicConnectionless,
		CalledAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x010101,
			SubsystemNumber:  SSNHLR,
		},
		CallingAddress: &SccpAddress{
			AddressIndicator: AISSNPresent | AIPCPresent,
			PointCode:        0x020202,
			SubsystemNumber:  SSNMAP,
		},
		Data: []byte{0xAA},
	}

	// Encoder uses ANSI (3-byte PC), decoder uses ITU (2-byte PC).
	wire, err := EncodeSCCP(in, VariantANSI)
	require.NoError(t, err)

	out, err := ParseSCCP(wire, VariantITU)
	require.NoError(t, err)

	// Decoder reads only 2 bytes → drops the high byte → SSN slot reads
	// what was the PC's third byte. Result is a wrong but predictable
	// shape, NOT a crash. Catches the regression where a future codec
	// "improvement" might silently start raising on mismatch instead of
	// degrading in this documented way.
	require.NotNil(t, out.CalledAddress)
	assert.NotEqual(t, uint32(0x010101), out.CalledAddress.PointCode,
		"variant mismatch must NOT silently produce the right PC")
}

// UDTS layout (Q.713 §4.11): type, return cause, then three pointers — no
// protocol class octet.
func TestUDTS_WireLayout(t *testing.T) {
	msg := &SccpMessage{
		MessageType:    MsgTypeUDTS,
		ReturnCause:    CauseSubsystemFailure,
		CalledAddress:  &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 0x0102, SubsystemNumber: SSNMSC},
		CallingAddress: &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 0x0201, SubsystemNumber: SSNHLR},
		Data:           []byte{0xAA, 0xBB},
	}
	b, err := EncodeSCCP(msg, VariantITU)
	require.NoError(t, err)
	assert.Equal(t, byte(MsgTypeUDTS), b[0])
	assert.Equal(t, byte(CauseSubsystemFailure), b[1])
	assert.Equal(t, byte(3), b[2], "CdPA pointer: the called address starts right after the three pointers")

	got, err := ParseSCCP(b, VariantITU)
	require.NoError(t, err)
	assert.Equal(t, MsgTypeUDTS, got.MessageType)
	assert.Equal(t, CauseSubsystemFailure, got.ReturnCause)
	assert.Equal(t, uint32(0x0102), got.CalledAddress.PointCode)
	assert.Equal(t, []byte{0xAA, 0xBB}, got.Data)
}

func TestUDT_ReturnOptionRoundTrip(t *testing.T) {
	msg := &SccpMessage{
		MessageType: MsgTypeUDT, ProtocolClass: ClassInSequenceConnectionless, ReturnOnError: true,
		CalledAddress:  &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 1, SubsystemNumber: SSNMSC},
		CallingAddress: &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 2, SubsystemNumber: SSNHLR},
		Data:           []byte{1},
	}
	b, err := EncodeSCCP(msg, VariantITU)
	require.NoError(t, err)
	assert.Equal(t, byte(0x81), b[1], "class 1 + return message on error")
	got, err := ParseSCCP(b, VariantITU)
	require.NoError(t, err)
	assert.True(t, got.ReturnOnError)
	assert.Equal(t, ClassInSequenceConnectionless, got.ProtocolClass)
}

// The data length indicator is one octet: more than 255 octets would wrap
// it and corrupt the frame, so encoding refuses instead.
func TestUDT_RefusesOversizedData(t *testing.T) {
	msg := &SccpMessage{
		MessageType:    MsgTypeUDT,
		CalledAddress:  &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 1, SubsystemNumber: SSNMSC},
		CallingAddress: &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 2, SubsystemNumber: SSNHLR},
		Data:           make([]byte, 256),
	}
	_, err := EncodeSCCP(msg, VariantITU)
	assert.ErrorIs(t, err, ErrMessageTooLarge)
	msg.Data = msg.Data[:255]
	_, err = EncodeSCCP(msg, VariantITU)
	assert.NoError(t, err)
}

// Routing indicator is bit 7 of the address indicator (Q.713 §3.4.1) and
// survives the codec.
func TestAddress_RoutingIndicatorOnWire(t *testing.T) {
	msg := &SccpMessage{
		MessageType:    MsgTypeUDT,
		CalledAddress:  &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 1, SubsystemNumber: SSNMSC},
		CallingAddress: &SccpAddress{AddressIndicator: AISSNPresent, SubsystemNumber: SSNHLR, GlobalTitle: &GlobalTitle{GTI: GTI0100, NumberingPlan: NPISDN, EncodingScheme: ESBCD, NAI: NAIInternational, Address: "905"}},
		Data:           []byte{1},
	}
	b, err := EncodeSCCP(msg, VariantITU)
	require.NoError(t, err)
	assert.Equal(t, byte(0x43), b[6], "CdPA AI: PC + SSN + route on SSN")
	got, err := ParseSCCP(b, VariantITU)
	require.NoError(t, err)
	assert.False(t, got.CalledAddress.AddressIndicator.RoutesOnGT())
	assert.True(t, got.CallingAddress.AddressIndicator.RoutesOnGT())
	assert.Equal(t, "905", got.CallingAddress.GlobalTitle.Address)
}
