// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// The frames below are built by hand from the ASN.1 of ITU-T Q.773 §3
// (TCAPMessages, DialoguePDUs) and its encoding rules (§4.1.1), not produced
// by the encoder: the codec is checked against the Recommendation, and a
// component's parameter is the argument's own TLV, carried verbatim.

func frame(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func i32(v int32) *int32 { return &v }

var tid5 = []byte{0, 0, 0, 5}

// External = EXTERNAL { direct-reference dialogue-as-id, single-ASN1-type [0] }.
const dialogueAsIDHex = "06 07 00 11 86 05 01 01 01" // { 0 0 17 773 1 1 1 }

var berFrames = map[string]struct {
	hex string
	msg *Message
}{
	"BEGIN, AARQ, Invoke": {
		"62 35" +
			" 48 04 00 00 00 05" + // otid
			" 6B 1E 28 1C " + dialogueAsIDHex + // dialogue portion, EXTERNAL
			" A0 11 60 0F" + // single-ASN1-type [0], AARQ-apdu
			" 80 02 07 80" + // protocol-version version1
			" A1 09 06 07 04 00 00 01 00 14 03" + // shortMsgGatewayContext-v3
			" 6C 0D A1 0B" + // component portion, invoke
			" 02 01 01 02 01 2D" + // invokeID 1, operationCode 45
			" 30 03 80 01 00", // parameter: the argument's own SEQUENCE
		&Message{MessageType: MsgTypeBegin, OriginatingTID: tid5,
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 20, 3}}, PDU: DialoguePDUAARQ},
			Components:      []*Component{{Type: CompTypeInvoke, InvokeID: i32(1), OperationCode: i32(45), Parameter: []byte{0x30, 0x03, 0x80, 0x01, 0x00}}}},
	},
	"END, AARE accepted, Return Result Last with a [3] result": {
		"64 43" +
			" 49 04 00 00 00 05" +
			" 6B 2A 28 28 " + dialogueAsIDHex +
			" A0 1D 61 1B" + // AARE-apdu
			" 80 02 07 80" +
			" A1 09 06 07 04 00 00 01 00 05 03" + // locationInfoRetrievalContext-v3
			" A2 03 02 01 00" + // result accepted
			" A3 05 A1 03 02 01 00" + // dialogue-service-user null
			" 6C 0F A2 0D 02 01 01" +
			" 30 08 02 01 16 A3 03 84 01 00", // result { operationCode 22, SendRoutingInfoRes [3] }
		&Message{MessageType: MsgTypeEnd, DestinationTID: tid5,
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 5, 3}}, PDU: DialoguePDUAARE},
			Components:      []*Component{{Type: CompTypeReturnResult, InvokeID: i32(1), OperationCode: i32(22), Parameter: []byte{0xA3, 0x03, 0x84, 0x01, 0x00}}}},
	},
	"CONTINUE, Return Result Not Last": {
		"65 1D 48 04 0A 0B 0C 0D 49 04 00 00 00 05" +
			" 6C 0F A7 0D 02 01 01 30 08 02 01 16 A3 03 84 01 00",
		&Message{MessageType: MsgTypeContinue, OriginatingTID: []byte{0x0A, 0x0B, 0x0C, 0x0D}, DestinationTID: tid5,
			Components: []*Component{{Type: CompTypeReturnResult, NotLast: true, InvokeID: i32(1), OperationCode: i32(22), Parameter: []byte{0xA3, 0x03, 0x84, 0x01, 0x00}}}},
	},
	"END, Return Result without a result": {
		"64 0D 49 04 00 00 00 05 6C 05 A2 03 02 01 01",
		&Message{MessageType: MsgTypeEnd, DestinationTID: tid5,
			Components: []*Component{{Type: CompTypeReturnResult, InvokeID: i32(1)}}},
	},
	"END, Reject not derivable, general problem": {
		"64 0F 49 04 00 00 00 05 6C 07 A4 05 05 00 80 01 00",
		&Message{MessageType: MsgTypeEnd, DestinationTID: tid5,
			Components: []*Component{{Type: CompTypeReject, ProblemCode: &ProblemCode{Type: ProblemTypeGeneral, Code: 0}}}},
	},
	"END, one-octet DTID, no components": {
		"64 03 49 01 07",
		&Message{MessageType: MsgTypeEnd, DestinationTID: []byte{7}},
	},
	"BEGIN, Invoke with a linked ID and no parameter": {
		"62 13 48 04 00 00 00 05 6C 0B A1 09 02 01 02 80 01 01 02 01 2E",
		&Message{MessageType: MsgTypeBegin, OriginatingTID: tid5,
			Components: []*Component{{Type: CompTypeInvoke, InvokeID: i32(2), LinkedID: i32(1), OperationCode: i32(46)}}},
	},
	"BEGIN, AARQ with user-information, no components": {
		"62 2D 48 04 00 00 00 05" +
			" 6B 25 28 23 " + dialogueAsIDHex +
			" A0 18 60 16 80 02 07 80 A1 09 06 07 04 00 00 01 00 14 03" +
			" BE 05 28 03 02 01 00", // user-information [30] { EXTERNAL }
		&Message{MessageType: MsgTypeBegin, OriginatingTID: tid5,
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 20, 3}},
				PDU: DialoguePDUAARQ, UserInformation: []byte{0x28, 0x03, 0x02, 0x01, 0x00}}},
	},
	"ABORT without a reason": {
		"67 06 49 04 00 00 00 05",
		&Message{MessageType: MsgTypeAbort, DestinationTID: tid5},
	},
	"TC-U-ABORT, ABRT from the provider": {
		"67 1A 49 04 00 00 00 05 6B 12 28 10 " + dialogueAsIDHex + " A0 05 64 03 80 01 01",
		&Message{MessageType: MsgTypeAbort, DestinationTID: tid5,
			DialoguePortion: &DialoguePortion{PDU: DialoguePDUABRT, AbortSourceProvider: true}},
	},
	"END, Return Error absentSubscriber": {
		"64 10 49 04 00 00 00 05 6C 08 A3 06 02 01 01 02 01 1B",
		&Message{MessageType: MsgTypeEnd, DestinationTID: tid5,
			Components: []*Component{{Type: CompTypeReturnError, InvokeID: i32(1), ErrorCode: i32(27)}}},
	},
	"END, Reject unrecognizedOperation": {
		"64 10 49 04 00 00 00 05 6C 08 A4 06 02 01 01 81 01 01",
		&Message{MessageType: MsgTypeEnd, DestinationTID: tid5,
			Components: []*Component{{Type: CompTypeReject, InvokeID: i32(1), ProblemCode: &ProblemCode{Type: ProblemTypeInvoke, Code: 1}}}},
	},
	"P-ABORT unrecognizedTransactionID": {
		"67 09 49 04 00 00 00 05 4A 01 01",
		&Message{MessageType: MsgTypeAbort, DestinationTID: tid5, PAbort: true, AbortCause: func() *AbortCause { c := AbortUnrecognizedTransactionID; return &c }()},
	},
	"TC-U-ABORT, AARE refusing the context": {
		"67 32 49 04 00 00 00 05" +
			" 6B 2A 28 28 " + dialogueAsIDHex +
			" A0 1D 61 1B 80 02 07 80" +
			" A1 09 06 07 04 00 00 01 00 14 02" + // the version the responder supports
			" A2 03 02 01 01" + // reject-permanent
			" A3 05 A1 03 02 01 02", // application-context-name-not-supported
		&Message{MessageType: MsgTypeAbort, DestinationTID: tid5,
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 20, 2}}, Rejected: true, PDU: DialoguePDUAARE}},
	},
	"TC-U-ABORT, ABRT from the user": {
		"67 1A 49 04 00 00 00 05" +
			" 6B 12 28 10 " + dialogueAsIDHex +
			" A0 05 64 03 80 01 00", // ABRT-apdu { abort-source dialogue-service-user }
		&Message{MessageType: MsgTypeAbort, DestinationTID: tid5,
			DialoguePortion: &DialoguePortion{PDU: DialoguePDUABRT}},
	},
}

func TestParseMessageFrames(t *testing.T) {
	for name, c := range berFrames {
		got, err := ParseMessage(frame(t, c.hex))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.msg) {
			t.Errorf("%s:\n got %#v\nwant %#v", name, got, c.msg)
			for i := range got.Components {
				t.Logf("  component %d: %+v", i, got.Components[i])
			}
		}
	}
}

func TestEncodeMessageFrames(t *testing.T) {
	for name, c := range berFrames {
		got, err := EncodeMessage(c.msg)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := frame(t, c.hex); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got % X\nwant % X", name, got, want)
		}
	}
}

// Q.773 §4.1.1: a length of 128 octets or more takes the long form, in
// the fewest octets — here one (0x81) for every enclosing element.
func TestLongFormLengths(t *testing.T) {
	param := append([]byte{0x04, 0x81, 0xC8}, make([]byte, 200)...) // OCTET STRING of 200
	msg := &Message{MessageType: MsgTypeBegin, OriginatingTID: tid5,
		Components: []*Component{{Type: CompTypeInvoke, InvokeID: i32(1), OperationCode: i32(44), Parameter: param}}}
	got, err := EncodeMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := frame(t, "62 81 DD 48 04 00 00 00 05 6C 81 D4 A1 81 D1 02 01 01 02 01 2C")
	if !bytes.Equal(got[:len(want)], want) || len(got) != 3+0xDD {
		t.Fatalf("header % X (len %d), want % X…", got[:len(want)], len(got), want)
	}
	back, err := ParseMessage(got)
	if err != nil || !reflect.DeepEqual(back, msg) {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}

// Shapes the encoder refuses rather than put on the wire.
func TestEncodeMessageRefuses(t *testing.T) {
	for name, m := range map[string]*Message{
		"linked ID out of range": {MessageType: MsgTypeBegin, OriginatingTID: tid5,
			Components: []*Component{{Type: CompTypeInvoke, InvokeID: i32(1), LinkedID: i32(200), OperationCode: i32(46)}}},
		"parameter of two elements": {MessageType: MsgTypeBegin, OriginatingTID: tid5,
			Components: []*Component{{Type: CompTypeInvoke, InvokeID: i32(1), OperationCode: i32(46), Parameter: []byte{0x05, 0x00, 0x05, 0x00}}}},
		"UNI with a dialogue portion": {MessageType: MsgTypeUnidirectional,
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: []int{0, 4, 0, 0, 1, 0, 20, 3}}},
			Components:      []*Component{{Type: CompTypeInvoke, InvokeID: i32(1), OperationCode: i32(46)}}},
		"UNI without components": {MessageType: MsgTypeUnidirectional},
		"TID of 5 octets":        {MessageType: MsgTypeEnd, DestinationTID: []byte{1, 2, 3, 4, 5}},
	} {
		if b, err := EncodeMessage(m); err == nil {
			t.Errorf("%s: encoded % X, want an error", name, b)
		}
	}
}

// InvokeIdType is INTEGER (-128..127): signed, one octet.
func TestNegativeInvokeID(t *testing.T) {
	f := frame(t, "64 10 49 04 00 00 00 05 6C 08 A3 06 02 01 FF 02 01 22")
	m, err := ParseMessage(f)
	if err != nil || *m.Components[0].InvokeID != -1 || *m.Components[0].ErrorCode != 34 {
		t.Fatalf("%v %+v", err, m)
	}
	back, _ := EncodeMessage(m)
	if !bytes.Equal(back, f) {
		t.Fatalf("re-encoded % X", back)
	}
}

func TestParseMessageRejects(t *testing.T) {
	for name, h := range map[string]string{
		"empty":                    "",
		"truncated":                "62 35 48 04 00",
		"trailing octets":          "67 09 49 04 00 00 00 05 4A 01 01 00",
		"not application class":    "30 03 02 01 00",
		"unknown message type":     "63 06 48 04 00 00 00 05",
		"BEGIN without OTID":       "62 00",
		"CONTINUE without DTID":    "65 06 48 04 00 00 00 05",
		"TID of 5 octets":          "64 07 49 05 00 00 00 00 05",
		"invoke without op code":   "62 0D 48 04 00 00 00 05 6C 05 A1 03 02 01 01",
		"two result parameters":    "64 16 49 04 00 00 00 05 6C 0E A2 0C 02 01 01 30 07 02 01 16 05 00 05 00",
		"global operation code":    "62 10 48 04 00 00 00 05 6C 08 A1 06 02 01 01 06 01 00",
		"component of unknown tag": "64 0D 49 04 00 00 00 05 6C 05 A5 03 02 01 01",
	} {
		if m, err := ParseMessage(frame(t, h)); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, m)
		}
	}
}
