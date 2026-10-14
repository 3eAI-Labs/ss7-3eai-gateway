// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

// Package wire implements M3UA (RFC 4666): the message codec, and an
// association that keeps the order in which the peer sent its messages.
//
// It replaces a library that parsed every received SCTP message, and
// handled every DATA, in a goroutine of its own, so the messages of one
// signalling relation reached SCCP in any order — against RFC 4666 §1.4.7,
// which keeps MTP's in-sequence delivery through the SCTP stream.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Version is the M3UA protocol version (RFC 4666 §3.1.1).
const Version = 1

// Message classes (RFC 4666 §3.1.2).
const (
	ClassMGMT     = 0
	ClassTransfer = 1
	ClassSSNM     = 2
	ClassASPSM    = 3
	ClassASPTM    = 4
	ClassRKM      = 9
)

// Message types within their class (RFC 4666 §3.1.2).
const (
	TypeERR  = 0 // MGMT
	TypeNTFY = 1 // MGMT

	TypeDATA = 1 // Transfer

	TypeDUNA = 1 // SSNM
	TypeDAVA = 2
	TypeDAUD = 3
	TypeSCON = 4
	TypeDUPU = 5
	TypeDRST = 6

	TypeASPUP    = 1 // ASPSM
	TypeASPDN    = 2
	TypeBEAT     = 3
	TypeASPUPAck = 4
	TypeASPDNAck = 5
	TypeBEATAck  = 6

	TypeASPAC    = 1 // ASPTM
	TypeASPIA    = 2
	TypeASPACAck = 3
	TypeASPIAAck = 4
)

// Parameter tags (RFC 4666 §3.2).
const (
	TagINFOString        = 0x0004
	TagRoutingContext    = 0x0006
	TagDiagnosticInfo    = 0x0007
	TagHeartbeatData     = 0x0009
	TagTrafficModeType   = 0x000b
	TagErrorCode         = 0x000c
	TagStatus            = 0x000d
	TagASPIdentifier     = 0x0011
	TagAffectedPointCode = 0x0012
	TagCorrelationID     = 0x0013
	TagNetworkAppearance = 0x0200
	TagProtocolData      = 0x0210
)

// Traffic Mode Type values (RFC 4666 §3.7.1).
const (
	TrafficModeOverride  = 1
	TrafficModeLoadshare = 2
	TrafficModeBroadcast = 3
)

// Error codes (RFC 4666 §3.8.1) the association sends.
const (
	ErrInvalidVersion          = 0x01
	ErrUnsupportedMessageClass = 0x03
	ErrUnsupportedMessageType  = 0x04
	ErrUnexpectedMessage       = 0x06
	ErrProtocolError           = 0x07
	ErrInvalidStreamIdentifier = 0x09
)

// headerLen is the common header: version, reserved, class, type, length.
const headerLen = 8

// Param is one TLV parameter; Value excludes the padding.
type Param struct {
	Tag   uint16
	Value []byte
}

// Message is an M3UA message: class, type, and its parameters in order.
type Message struct {
	Class, Type uint8
	Params      []Param
}

// Get returns the value of the first parameter with tag.
func (m *Message) Get(tag uint16) ([]byte, bool) {
	for _, p := range m.Params {
		if p.Tag == tag {
			return p.Value, true
		}
	}
	return nil, false
}

// Uint32 returns a 32-bit parameter value.
func (m *Message) Uint32(tag uint16) (uint32, bool) {
	v, ok := m.Get(tag)
	if !ok || len(v) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

// Marshal encodes m: each parameter padded to a multiple of four octets,
// the message length counting the padding (RFC 4666 §3.1.4, §3.2).
func (m *Message) Marshal() []byte {
	n := headerLen
	for _, p := range m.Params {
		n += pad4(4 + len(p.Value))
	}
	b := make([]byte, headerLen, n)
	b[0], b[1], b[2], b[3] = Version, 0, m.Class, m.Type
	binary.BigEndian.PutUint32(b[4:], uint32(n))
	for _, p := range m.Params {
		var tl [4]byte
		binary.BigEndian.PutUint16(tl[0:], p.Tag)
		binary.BigEndian.PutUint16(tl[2:], uint16(4+len(p.Value)))
		b = append(b, tl[:]...)
		b = append(b, p.Value...)
		b = append(b, make([]byte, pad4(4+len(p.Value))-4-len(p.Value))...)
	}
	return b
}

// Parse decodes one M3UA message. A receiver should accept a message whose
// length leaves out the final parameter's padding (RFC 4666 §3.1.4); the
// octets after the stated length are ignored.
func Parse(b []byte) (*Message, error) {
	if len(b) < headerLen {
		return nil, &ProtocolError{Code: ErrProtocolError, Reason: fmt.Sprintf("%d octets, shorter than the common header", len(b))}
	}
	if b[0] != Version {
		return nil, &ProtocolError{Code: ErrInvalidVersion, Reason: fmt.Sprintf("version %d", b[0])}
	}
	length := binary.BigEndian.Uint32(b[4:])
	if length < headerLen || int64(length) > int64(len(b)) {
		return nil, &ProtocolError{Code: ErrProtocolError, Reason: fmt.Sprintf("message length %d, %d octets received", length, len(b))}
	}
	m := &Message{Class: b[2], Type: b[3]}
	rest := b[headerLen:length]
	for len(rest) > 0 {
		if len(rest) < 4 {
			return nil, &ProtocolError{Code: ErrProtocolError, Reason: "truncated parameter header"}
		}
		tag := binary.BigEndian.Uint16(rest[0:])
		plen := int(binary.BigEndian.Uint16(rest[2:]))
		if plen < 4 || plen > len(rest) {
			return nil, &ProtocolError{Code: ErrProtocolError, Reason: fmt.Sprintf("parameter %#04x of length %d, %d octets left", tag, plen, len(rest))}
		}
		m.Params = append(m.Params, Param{Tag: tag, Value: append([]byte(nil), rest[4:plen]...)})
		if adv := pad4(plen); adv <= len(rest) {
			rest = rest[adv:]
		} else {
			rest = nil // the final parameter's padding left out of the length
		}
	}
	return m, nil
}

func pad4(n int) int { return (n + 3) &^ 3 }

// ProtocolError is a malformed or unexpected message; Code is the M3UA
// error code to answer it with (RFC 4666 §3.8.1).
type ProtocolError struct {
	Code   uint32
	Reason string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("m3ua: protocol error %#02x: %s", e.Code, e.Reason)
}

// ProtocolData is the Protocol Data parameter of a DATA message (RFC 4666
// §3.3.1): the MTP3 routing label and the user data.
type ProtocolData struct {
	OPC, DPC uint32
	SI, NI   uint8
	MP, SLS  uint8
	Data     []byte
}

func (pd *ProtocolData) marshal() []byte {
	b := make([]byte, 12, 12+len(pd.Data))
	binary.BigEndian.PutUint32(b[0:], pd.OPC)
	binary.BigEndian.PutUint32(b[4:], pd.DPC)
	b[8], b[9], b[10], b[11] = pd.SI, pd.NI, pd.MP, pd.SLS
	return append(b, pd.Data...)
}

func parseProtocolData(v []byte) (*ProtocolData, error) {
	if len(v) < 12 {
		return nil, &ProtocolError{Code: ErrProtocolError, Reason: fmt.Sprintf("protocol data of %d octets", len(v))}
	}
	return &ProtocolData{
		OPC: binary.BigEndian.Uint32(v[0:]), DPC: binary.BigEndian.Uint32(v[4:]),
		SI: v[8], NI: v[9], MP: v[10], SLS: v[11],
		Data: append([]byte(nil), v[12:]...),
	}, nil
}

// DataOptions are the optional parameters of a DATA message.
type DataOptions struct {
	// NetworkAppearance, when non-zero, goes first, as RFC 4666 §3.3.1
	// requires of it.
	NetworkAppearance uint32
	// RoutingContext, when non-zero, identifies the traffic flow.
	RoutingContext uint32
}

// NewData builds a DATA message.
func NewData(pd *ProtocolData, opt DataOptions) *Message {
	m := &Message{Class: ClassTransfer, Type: TypeDATA}
	if opt.NetworkAppearance != 0 {
		m.Params = append(m.Params, u32Param(TagNetworkAppearance, opt.NetworkAppearance))
	}
	if opt.RoutingContext != 0 {
		m.Params = append(m.Params, u32Param(TagRoutingContext, opt.RoutingContext))
	}
	m.Params = append(m.Params, Param{Tag: TagProtocolData, Value: pd.marshal()})
	return m
}

// ProtocolData returns a DATA message's Protocol Data.
func (m *Message) ProtocolData() (*ProtocolData, error) {
	if m.Class != ClassTransfer || m.Type != TypeDATA {
		return nil, errors.New("m3ua: not a DATA message")
	}
	v, ok := m.Get(TagProtocolData)
	if !ok {
		return nil, &ProtocolError{Code: ErrProtocolError, Reason: "DATA without Protocol Data"}
	}
	return parseProtocolData(v)
}

func u32Param(tag uint16, v uint32) Param {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return Param{Tag: tag, Value: b}
}

// NewError builds an ERR message (RFC 4666 §3.8.1).
func NewError(code uint32, diagnostic []byte) *Message {
	m := &Message{Class: ClassMGMT, Type: TypeERR, Params: []Param{u32Param(TagErrorCode, code)}}
	if len(diagnostic) > 0 {
		m.Params = append(m.Params, Param{Tag: TagDiagnosticInfo, Value: diagnostic})
	}
	return m
}
