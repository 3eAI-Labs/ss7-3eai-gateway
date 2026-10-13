// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"errors"
	"fmt"
)

// SCCP wire codec — ITU-T Q.713. Lifted from *Handler so test peers and
// codec unit tests can encode/decode SCCP UDT/XUDT/UDTS frames without
// instantiating the full Handler (which pulls in M3UA channels, GTT
// engine, and worker pool). Single source of truth for the wire format;
// *Handler workers call into these.
//
// All entry points take a NetworkVariant. PC field width is variant-
// dependent (Q.713 §3.4.1 — ITU 14-bit/2 octets vs ANSI 24-bit/3 octets);
// passing the wrong variant silently corrupts the address parameter,
// which is why callers must thread the operator's network choice in
// rather than relying on a global default.

// ParseSCCP parses a complete SCCP message starting from the message-type
// byte. Returns the typed *SccpMessage or an error for malformed/unsupported
// message types. UDT/XUDT/UDTS are supported (Q.713 §4.2/4.3, §4.5).
func ParseSCCP(data []byte, variant NetworkVariant) (*SccpMessage, error) {
	if len(data) < 1 {
		return nil, ErrInvalidMessage
	}

	mt := MessageType(data[0])
	rest := data[1:]

	switch mt {
	case MsgTypeUDT:
		return parseUDT(rest, variant)
	case MsgTypeXUDT:
		return parseXUDT(rest, variant)
	case MsgTypeUDTS:
		return parseUDTS(rest, variant)
	default:
		return nil, fmt.Errorf("unsupported SCCP message type: 0x%02X", uint8(mt))
	}
}

// EncodeSCCP encodes an *SccpMessage to its Q.713 wire form.
func EncodeSCCP(msg *SccpMessage, variant NetworkVariant) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("nil SCCP message")
	}
	switch msg.MessageType {
	case MsgTypeUDT:
		return encodeUDT(msg, variant)
	case MsgTypeXUDT:
		return encodeXUDT(msg, variant)
	case MsgTypeUDTS:
		return encodeUDTS(msg, variant)
	default:
		return nil, fmt.Errorf("unsupported SCCP message type for encoding: 0x%02X", uint8(msg.MessageType))
	}
}

// --- UDT (Unitdata) — Q.713 §4.2 ---------------------------------------

// parseUDT parses an SCCP UDT message (Q.713 §4.2). Pointer convention is
// Q.713 §3.3: pointer value = offset in octets from the pointer field's
// own first octet to the first octet of the addressed parameter. The
// `data` slice here starts AFTER the message-type byte (so the protocol
// class is at data[0] and pointers occupy data[1..3]).
func parseUDT(data []byte, variant NetworkVariant) (*SccpMessage, error) {
	if len(data) < 4 {
		return nil, ErrInvalidMessage
	}

	msg := &SccpMessage{MessageType: MsgTypeUDT}

	msg.ProtocolClass = ProtocolClass(data[0] & 0x0F)
	msg.ReturnOnError = data[0]&returnOnErrorBit != 0
	ptrCdPA := int(data[1])
	ptrCgPA := int(data[2])
	ptrData := int(data[3])

	if ptrCdPA > 0 {
		// Pointer at data[1]; param starts at data[1+ptrCdPA] (Q.713 §3.3).
		addrStart := 1 + ptrCdPA
		if addrStart < len(data) {
			addr, _, err := parseAddress(data[addrStart:], variant)
			if err != nil {
				return nil, fmt.Errorf("called address: %w", err)
			}
			msg.CalledAddress = addr
		}
	}

	if ptrCgPA > 0 {
		addrStart := 2 + ptrCgPA
		if addrStart < len(data) {
			addr, _, err := parseAddress(data[addrStart:], variant)
			if err != nil {
				return nil, fmt.Errorf("calling address: %w", err)
			}
			msg.CallingAddress = addr
		}
	}

	if ptrData > 0 {
		dataStart := 3 + ptrData
		if dataStart < len(data) {
			dataLen := int(data[dataStart])
			dataStart++
			if dataStart+dataLen <= len(data) {
				msg.Data = make([]byte, dataLen)
				copy(msg.Data, data[dataStart:dataStart+dataLen])
			}
		}
	}

	return msg, nil
}

// encodeUDT writes an SCCP UDT message (Q.713 §4.2). Pointer values are
// Q.713 §3.3 compliant: each is the octet offset from its own pointer
// byte (positions 2/3/4 in the full buffer) to the first byte of the
// referenced variable part.
func encodeUDT(msg *SccpMessage, variant NetworkVariant) ([]byte, error) {
	if len(msg.Data) > maxUnitdataData {
		return nil, fmt.Errorf("%w: %d octets of user data, a UDT carries at most %d (segmentation is not implemented)",
			ErrMessageTooLarge, len(msg.Data), maxUnitdataData)
	}
	buf := make([]byte, 0, MaxSCCPMessageSize)
	buf = append(buf, byte(MsgTypeUDT))
	buf = append(buf, classOctet(msg))

	ptrOffset := len(buf) // = 2; first pointer byte goes here
	buf = append(buf, 0, 0, 0)

	// CdPA: pointer at full[2], param will be at full[len(buf)] (current end).
	cdPAStart := len(buf) - ptrOffset
	buf = append(buf, encodeAddress(msg.CalledAddress, variant)...)

	// CgPA: pointer at full[3], param at full[len(buf)] (current end).
	cgPAStart := len(buf) - (ptrOffset + 1)
	buf = append(buf, encodeAddress(msg.CallingAddress, variant)...)

	// Data: pointer at full[4], param (length-prefixed) at full[len(buf)].
	dataStart := len(buf) - (ptrOffset + 2)
	buf = append(buf, byte(len(msg.Data)))
	buf = append(buf, msg.Data...)

	buf[ptrOffset] = byte(cdPAStart)
	buf[ptrOffset+1] = byte(cgPAStart)
	buf[ptrOffset+2] = byte(dataStart)

	return buf, nil
}

// --- XUDT (Extended Unitdata) — Q.713 §4.3 -----------------------------

// parseXUDT parses XUDT (Q.713 §4.3). XUDT prepends ProtocolClass (1) +
// HopCounter (1) before the four pointer bytes (CdPA/CgPA/Data/Optional).
// Pointers use the same Q.713 §3.3 convention as UDT: offset from the
// pointer byte itself.
func parseXUDT(data []byte, variant NetworkVariant) (*SccpMessage, error) {
	if len(data) < 6 {
		return nil, ErrInvalidMessage
	}

	msg := &SccpMessage{MessageType: MsgTypeXUDT}

	msg.ProtocolClass = ProtocolClass(data[0] & 0x0F)
	msg.ReturnOnError = data[0]&returnOnErrorBit != 0
	msg.HopCounter = data[1]
	ptrCdPA := int(data[2])
	ptrCgPA := int(data[3])
	ptrData := int(data[4])
	// data[5] = ptrOpt — optional parameters parsing omitted

	if ptrCdPA > 0 {
		addrStart := 2 + ptrCdPA
		if addrStart < len(data) {
			addr, _, err := parseAddress(data[addrStart:], variant)
			if err == nil {
				msg.CalledAddress = addr
			}
		}
	}

	if ptrCgPA > 0 {
		addrStart := 3 + ptrCgPA
		if addrStart < len(data) {
			addr, _, err := parseAddress(data[addrStart:], variant)
			if err == nil {
				msg.CallingAddress = addr
			}
		}
	}

	if ptrData > 0 {
		dataStart := 4 + ptrData
		if dataStart < len(data) {
			dataLen := int(data[dataStart])
			dataStart++
			if dataStart+dataLen <= len(data) {
				msg.Data = make([]byte, dataLen)
				copy(msg.Data, data[dataStart:dataStart+dataLen])
			}
		}
	}

	return msg, nil
}

func encodeXUDT(msg *SccpMessage, variant NetworkVariant) ([]byte, error) {
	if len(msg.Data) > maxUnitdataData {
		return nil, fmt.Errorf("%w: %d octets of user data, an unsegmented XUDT carries at most %d",
			ErrMessageTooLarge, len(msg.Data), maxUnitdataData)
	}
	buf := make([]byte, 0, MaxSCCPMessageSize)
	buf = append(buf, byte(MsgTypeXUDT))
	buf = append(buf, classOctet(msg))
	buf = append(buf, msg.HopCounter)

	ptrOffset := len(buf) // first of 4 pointer bytes
	buf = append(buf, 0, 0, 0, 0)

	cdPAStart := len(buf) - ptrOffset
	buf = append(buf, encodeAddress(msg.CalledAddress, variant)...)

	cgPAStart := len(buf) - (ptrOffset + 1)
	buf = append(buf, encodeAddress(msg.CallingAddress, variant)...)

	dataStart := len(buf) - (ptrOffset + 2)
	buf = append(buf, byte(len(msg.Data)))
	buf = append(buf, msg.Data...)

	buf[ptrOffset] = byte(cdPAStart)
	buf[ptrOffset+1] = byte(cgPAStart)
	buf[ptrOffset+2] = byte(dataStart)
	buf[ptrOffset+3] = 0 // optional parameters absent

	return buf, nil
}

// --- UDTS (Unitdata Service) — Q.713 §4.11 ----------------------------

// A UDTS has the layout of a UDT with the return cause in place of the
// protocol class: message type, return cause, three pointers, then called
// address, calling address and data.

func parseUDTS(data []byte, variant NetworkVariant) (*SccpMessage, error) {
	msg, err := parseUDT(data, variant)
	if err != nil {
		return nil, err
	}
	msg.MessageType = MsgTypeUDTS
	msg.ReturnCause = ReturnCause(data[0])
	msg.ProtocolClass = 0
	msg.ReturnOnError = false
	return msg, nil
}

func encodeUDTS(msg *SccpMessage, variant NetworkVariant) ([]byte, error) {
	buf, err := encodeUDT(&SccpMessage{
		CalledAddress:  msg.CalledAddress,
		CallingAddress: msg.CallingAddress,
		Data:           msg.Data,
	}, variant)
	if err != nil {
		return nil, err
	}
	buf[0] = byte(MsgTypeUDTS)
	buf[1] = byte(msg.ReturnCause)
	return buf, nil
}

// returnOnErrorBit is the "return message on error" option in the protocol
// class octet (Q.713 §3.6).
const returnOnErrorBit = 0x80

// maxUnitdataData is the most user data a UDT or an unsegmented XUDT can
// carry: the data parameter's length indicator is one octet.
const maxUnitdataData = 255

func classOctet(msg *SccpMessage) byte {
	b := byte(msg.ProtocolClass & 0x0F)
	if msg.ReturnOnError {
		b |= returnOnErrorBit
	}
	return b
}

// --- Address — Q.713 §3.4 ----------------------------------------------

// parseAddress decodes one SCCP address parameter (Q.713 §3.4). The
// PC field width depends on the network variant — ITU lays it out as 2
// octets (14 bits used + 2 reserved), ANSI as 3 octets (24 bits). PC and
// SSN are little-endian per Q.713.
func parseAddress(data []byte, variant NetworkVariant) (*SccpAddress, int, error) {
	if len(data) < 2 {
		return nil, 0, ErrInvalidAddress
	}

	addr := &SccpAddress{}
	offset := 0

	length := int(data[offset])
	offset++

	if length == 0 || offset+length > len(data) {
		return nil, 0, ErrInvalidAddress
	}

	addr.AddressIndicator = AddressIndicator(data[offset])
	offset++
	remaining := length - 1

	if addr.AddressIndicator&AIPCPresent != 0 {
		pcWidth := variant.PCWidth()
		if remaining < pcWidth {
			return nil, 0, ErrInvalidAddress
		}
		switch pcWidth {
		case 3: // ANSI 24-bit
			addr.PointCode = uint32(data[offset]) |
				(uint32(data[offset+1]) << 8) |
				(uint32(data[offset+2]) << 16)
		default: // ITU 14-bit / 2 octets
			addr.PointCode = uint32(data[offset]) |
				(uint32(data[offset+1]) << 8)
		}
		offset += pcWidth
		remaining -= pcWidth
	}

	if addr.AddressIndicator&AISSNPresent != 0 && remaining >= 1 {
		addr.SubsystemNumber = SubsystemNumber(data[offset])
		offset++
		remaining--
	}

	// GT presence is signaled by GTI nibble != 0 per Q.713 §3.4.1 — there is
	// no dedicated "GT present" flag bit on the wire. AIGTPresent is a
	// caller-side hint only; the wire-truthful check uses the GTI nibble.
	if (byte(addr.AddressIndicator)>>2)&0x0F != 0 && remaining > 0 {
		gt, gtLen := parseGlobalTitle(data[offset:offset+remaining], addr.AddressIndicator)
		addr.GlobalTitle = gt
		offset += gtLen
	}

	return addr, offset, nil
}

func encodeAddress(addr *SccpAddress, variant NetworkVariant) []byte {
	if addr == nil {
		return []byte{0}
	}

	buf := make([]byte, 0, 32)

	lengthPos := len(buf)
	buf = append(buf, 0)

	// Sync GTI nibble (AI bits 2-5) with GlobalTitle.GTI before writing.
	// Per Q.713 §3.4.1 the AI byte's bits 2-5 are the GTI field — there is no
	// separate "GT present" flag bit on the wire. Without this, callers that
	// set AddressIndicator = AIGTPresent|AISSNPresent (= 0x06, GTI nibble = 1)
	// but encode the GT body per gt.GTI = GTI0100 produce wire frames where
	// the peer parses only NAI prefix and BCD-decodes the TT/NP-ES bytes as
	// address digits — silently corrupting the called GT. Single source of
	// truth: gt.GTI drives the AI byte.
	ai := byte(addr.AddressIndicator) &^ 0x3C
	if addr.GlobalTitle != nil {
		ai |= byte(addr.GlobalTitle.GTI) << 2
	}
	buf = append(buf, ai)

	if addr.AddressIndicator&AIPCPresent != 0 {
		// Mask to variant width — guards against operators stuffing 24-bit
		// PCs into ITU-14 networks (would silently overflow into the next
		// octet). With the mask applied, a misconfigured PC at least drops
		// its high bits in a predictable way rather than corrupting SSN.
		pc := addr.PointCode & variant.PCMask()
		buf = append(buf, byte(pc&0xFF))
		buf = append(buf, byte((pc>>8)&0xFF))
		if variant.PCWidth() == 3 {
			buf = append(buf, byte((pc>>16)&0xFF))
		}
	}

	if addr.AddressIndicator&AISSNPresent != 0 {
		buf = append(buf, byte(addr.SubsystemNumber))
	}

	if addr.GlobalTitle != nil && addr.GlobalTitle.GTI != 0 {
		buf = append(buf, encodeGlobalTitle(addr.GlobalTitle)...)
	}

	buf[lengthPos] = byte(len(buf) - 1)
	return buf
}

func parseGlobalTitle(data []byte, ai AddressIndicator) (*GlobalTitle, int) {
	if len(data) == 0 {
		return nil, 0
	}

	gt := &GlobalTitle{}
	offset := 0
	gti := GlobalTitleIndicator((ai >> 2) & 0x0F)
	gt.GTI = gti

	switch gti {
	case GTI0001:
		if len(data) >= 2 {
			gt.NAI = NatureOfAddressIndicator(data[offset] & 0x7F)
			offset++
			gt.Address = decodeBCDDigits(data[offset:])
			offset = len(data)
		}
	case GTI0010:
		if len(data) >= 2 {
			gt.TranslationType = data[offset]
			offset++
			gt.Address = decodeBCDDigits(data[offset:])
			offset = len(data)
		}
	case GTI0011:
		if len(data) >= 3 {
			gt.TranslationType = data[offset]
			offset++
			gt.NumberingPlan = NumberingPlan((data[offset] >> 4) & 0x0F)
			gt.EncodingScheme = EncodingScheme(data[offset] & 0x0F)
			offset++
			gt.Address = decodeBCDDigits(data[offset:])
			offset = len(data)
		}
	case GTI0100:
		if len(data) >= 4 {
			gt.TranslationType = data[offset]
			offset++
			gt.NumberingPlan = NumberingPlan((data[offset] >> 4) & 0x0F)
			gt.EncodingScheme = EncodingScheme(data[offset] & 0x0F)
			offset++
			gt.NAI = NatureOfAddressIndicator(data[offset] & 0x7F)
			offset++
			gt.Address = decodeBCDDigits(data[offset:])
			offset = len(data)
		}
	default:
		gt.Address = decodeBCDDigits(data)
		offset = len(data)
	}

	return gt, offset
}

func encodeGlobalTitle(gt *GlobalTitle) []byte {
	if gt == nil || gt.Address == "" {
		return nil
	}

	buf := make([]byte, 0, 16)

	switch gt.GTI {
	case GTI0001:
		buf = append(buf, byte(gt.NAI&0x7F))
		buf = append(buf, encodeBCDDigits(gt.Address)...)
	case GTI0010:
		buf = append(buf, gt.TranslationType)
		buf = append(buf, encodeBCDDigits(gt.Address)...)
	case GTI0011:
		buf = append(buf, gt.TranslationType)
		buf = append(buf, byte((uint8(gt.NumberingPlan)<<4)|(uint8(gt.EncodingScheme)&0x0F)))
		buf = append(buf, encodeBCDDigits(gt.Address)...)
	case GTI0100:
		buf = append(buf, gt.TranslationType)
		buf = append(buf, byte((uint8(gt.NumberingPlan)<<4)|(uint8(gt.EncodingScheme)&0x0F)))
		buf = append(buf, byte(gt.NAI&0x7F))
		buf = append(buf, encodeBCDDigits(gt.Address)...)
	default:
		buf = append(buf, encodeBCDDigits(gt.Address)...)
	}

	return buf
}

// --- BCD digits — Q.713 §3.4.2.3 ---------------------------------------

func decodeBCDDigits(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	digits := make([]byte, 0, len(data)*2)
	for _, b := range data {
		low := b & 0x0F
		if low <= 9 {
			digits = append(digits, '0'+low)
		}
		high := (b >> 4) & 0x0F
		if high <= 9 {
			digits = append(digits, '0'+high)
		} else if high == 0x0F {
			break // odd-digit filler
		}
	}

	return string(digits)
}

func encodeBCDDigits(digits string) []byte {
	if len(digits) == 0 {
		return nil
	}

	byteLen := (len(digits) + 1) / 2
	buf := make([]byte, byteLen)

	for i := 0; i < byteLen; i++ {
		var low, high byte
		if i*2 < len(digits) {
			low = digits[i*2] - '0'
		}
		if i*2+1 < len(digits) {
			high = digits[i*2+1] - '0'
		} else {
			high = 0x0F // odd-digit filler
		}
		buf[i] = low | (high << 4)
	}

	return buf
}
