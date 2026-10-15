// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// ASN.1 BER codec for MAP operations (3GPP TS 29.002 v17, MAP-MS-DataTypes).
//
// Scope:
//   - Mandatory IEs of the in-scope ops are decoded into typed *Request structs.
//   - Mandatory IEs of responses are encoded from typed *Response structs.
//   - OPTIONAL fields beyond the extension marker (`...`) are read as RawValue
//     and preserved into ExtensionContainer-shaped []byte fields where present;
//     unknown trailing bytes are accepted and discarded (forward-compat per the
//     module extension marker semantics).
//
// Out of scope here:
//   - Indefinite-length BER and constructed OCTET STRING (not seen in real GSM
//     traffic from any compliant stack).
//   - SCCP UDTS / TC-USER ABORT round-trip.
//
// Wire-format anchors:
//   - 3GPP TS 29.002 v17 §17.7 (MAP-SM-DataTypes, MAP-MS-DataTypes)
//   - ITU-T Q.713 Annex E — TBCD digit packing for IMSI / AddressString
//   - ITU-T X.690 — BER/DER tag/length/value rules
//
// The MAP module declares `IMPLICIT TAGS ::=`, so context-tagged fields
// `[N] X` appear on the wire as primitive `[CONTEXT N]` octets — matching
// `asn1:"tag:N,implicit"` in encoding/asn1.

var (
	errCodecShortBuffer = errors.New("map: ASN.1 buffer too short")
	errCodecBadTBCD     = errors.New("map: invalid TBCD digit")
	errCodecBadAddrLen  = errors.New("map: AddressString length out of range")
	errCodecBadIMSILen  = errors.New("map: IMSI length out of range")
)

// ----- TBCD / AddressString helpers (Q.713 Annex E) ------------------------

// decodeTBCD unpacks the low-nibble-first digit pairs into a digit string.
// Each byte holds two BCD digits: low nibble = N(2k+1), high nibble = N(2k+2).
// 0xF in the high nibble of the last byte indicates an odd-length string.
// Digits 0-9 are '0'..'9'; 10/11/12/13/14 map to '*','#','a','b','c' per Q.713.
func decodeTBCD(b []byte) (string, error) {
	out := make([]byte, 0, len(b)*2)
	for i, octet := range b {
		lo, hi := octet&0x0F, (octet>>4)&0x0F
		d, err := tbcdDigit(lo)
		if err != nil {
			return "", err
		}
		out = append(out, d)
		if hi == 0x0F {
			if i != len(b)-1 {
				return "", errCodecBadTBCD
			}
			break
		}
		d, err = tbcdDigit(hi)
		if err != nil {
			return "", err
		}
		out = append(out, d)
	}
	return string(out), nil
}

func encodeTBCD(s string) ([]byte, error) {
	out := make([]byte, 0, (len(s)+1)/2)
	for i := 0; i < len(s); i += 2 {
		lo, err := tbcdNibble(s[i])
		if err != nil {
			return nil, err
		}
		var hi byte = 0x0F
		if i+1 < len(s) {
			hi, err = tbcdNibble(s[i+1])
			if err != nil {
				return nil, err
			}
		}
		out = append(out, (hi<<4)|lo)
	}
	return out, nil
}

func tbcdDigit(n byte) (byte, error) {
	switch {
	case n <= 9:
		return '0' + n, nil
	case n == 10:
		return '*', nil
	case n == 11:
		return '#', nil
	case n == 12:
		return 'a', nil
	case n == 13:
		return 'b', nil
	case n == 14:
		return 'c', nil
	}
	return 0, errCodecBadTBCD
}

func tbcdNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c == '*':
		return 10, nil
	case c == '#':
		return 11, nil
	case c == 'a' || c == 'A':
		return 12, nil
	case c == 'b' || c == 'B':
		return 13, nil
	case c == 'c' || c == 'C':
		return 14, nil
	}
	return 0, errCodecBadTBCD
}

// decodeAddressString reads an AddressString (Q.713 Annex E.1):
//
//	octet 0: ext(1) | NoA(3) | NPI(4)
//	octets 1..N: TBCD digits
//
// AddressString length range is 1..20 octets per the ASN.1.
func decodeAddressString(b []byte) (*AddressString, error) {
	if len(b) < 1 || len(b) > 20 {
		return nil, errCodecBadAddrLen
	}
	digits, err := decodeTBCD(b[1:])
	if err != nil {
		return nil, fmt.Errorf("AddressString: %w", err)
	}
	return &AddressString{
		Nature:     (b[0] >> 4) & 0x07,
		NumberPlan: b[0] & 0x0F,
		Digits:     digits,
	}, nil
}

func encodeAddressString(a *AddressString) ([]byte, error) {
	if a == nil {
		return nil, errors.New("AddressString: nil")
	}
	digits, err := encodeTBCD(a.Digits)
	if err != nil {
		return nil, fmt.Errorf("AddressString: %w", err)
	}
	header := byte(0x80) | ((a.Nature & 0x07) << 4) | (a.NumberPlan & 0x0F)
	out := make([]byte, 1+len(digits))
	out[0] = header
	copy(out[1:], digits)
	if len(out) > 20 {
		return nil, errCodecBadAddrLen
	}
	return out, nil
}

// decodeIMSI reads a TBCD-only IMSI (3..8 octets ⇒ 5..16 digits).
func decodeIMSI(b []byte) (IMSI, error) {
	if len(b) < 3 || len(b) > 8 {
		return "", errCodecBadIMSILen
	}
	d, err := decodeTBCD(b)
	if err != nil {
		return "", fmt.Errorf("IMSI: %w", err)
	}
	return IMSI(d), nil
}

func encodeIMSI(imsi IMSI) ([]byte, error) {
	b, err := encodeTBCD(string(imsi))
	if err != nil {
		return nil, fmt.Errorf("IMSI: %w", err)
	}
	if len(b) < 3 || len(b) > 8 {
		return nil, errCodecBadIMSILen
	}
	return b, nil
}

// ----- SMS path: SendRoutingInfoForSM / MtForwardSM / MoForwardSM ----------
//
// 3GPP TS 29.002 v17 §17.7.13 (SRI-for-SM), §17.7.4 (MT-Fwd-SM),
// §17.7.5 (MO-Fwd-SM). MAP-SM-DataTypes module — IMPLICIT TAGS, so
// context-tagged fields appear as primitive [CONTEXT N] octets.
//
// SM-RP-DA / SM-RP-OA are CHOICE on the wire. encoding/asn1 doesn't model
// CHOICE natively; we emit the chosen alternative directly as the field's
// tagged primitive (since MAP-SM uses IMPLICIT, the CHOICE alternative
// itself carries the tag — no outer SEQUENCE/CHOICE wrapper). On decode we
// inspect the leading tag byte and dispatch.

// ---- SendRoutingInfoForSM (op 45) ----

type wireSendRoutingInfoForSMArg struct {
	MSISDN               []byte          `asn1:"tag:0,implicit"`
	SMRPPRI              bool            `asn1:"tag:1,implicit"`
	ServiceCentreAddress []byte          `asn1:"tag:2,implicit"`
	ExtensionContainer   asn1.RawValue   `asn1:"tag:6,optional"`
	GPRSSupportIndicator asn1.RawValue   `asn1:"tag:7,optional"`
	SMRPMTI              asn1.RawValue   `asn1:"tag:8,optional"`
	SMRPSMEA             asn1.RawValue   `asn1:"tag:9,optional"`
	Rest                 []asn1.RawValue `asn1:"optional"`
}

type wireLocationInfoWithLMSI struct {
	NetworkNodeNumber []byte          `asn1:"tag:1,implicit"`
	LMSI              []byte          `asn1:"optional"`
	GPRSNodeIndicator asn1.RawValue   `asn1:"tag:5,optional"`
	Rest              []asn1.RawValue `asn1:"optional"`
}

type wireSendRoutingInfoForSMRes struct {
	IMSI                 []byte                   `asn1:""`
	LocationInfoWithLMSI wireLocationInfoWithLMSI `asn1:"tag:0"`
	ExtensionContainer   asn1.RawValue            `asn1:"tag:4,optional"`
	Rest                 []asn1.RawValue          `asn1:"optional"`
}

func decodeSendRoutingInfoForSMArg(b []byte) (*SendRoutingInfoForSMRequest, error) {
	var w wireSendRoutingInfoForSMArg
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("SendRoutingInfoForSMArg: %w", err)
	}
	msisdn, err := decodeAddressString(w.MSISDN)
	if err != nil {
		return nil, fmt.Errorf("msisdn: %w", err)
	}
	sca, err := decodeAddressString(w.ServiceCentreAddress)
	if err != nil {
		return nil, fmt.Errorf("serviceCentreAddress: %w", err)
	}
	req := &SendRoutingInfoForSMRequest{
		MSISDN:               MSISDN(msisdn.Digits),
		SMRPPRI:              w.SMRPPRI,
		ServiceCentreAddress: *sca,
		GPRSSupportIndicator: len(w.GPRSSupportIndicator.FullBytes) > 0,
	}
	if len(w.SMRPMTI.Bytes) >= 1 {
		mti := w.SMRPMTI.Bytes[0]
		req.SMRPMTI = &mti
	}
	if len(w.SMRPSMEA.Bytes) > 0 {
		req.SMRPSMEA = append([]byte(nil), w.SMRPSMEA.Bytes...)
	}
	return req, nil
}

// EncodeSendRoutingInfoForSMArg is the public encoder used by composition
// roots that originate SRI-for-SM (e.g. NATS bridge). Wraps the internal
// codec; carrier-grade callers should not roll their own BER.
func EncodeSendRoutingInfoForSMArg(r *SendRoutingInfoForSMRequest) ([]byte, error) {
	return encodeSendRoutingInfoForSMArg(r)
}

// DecodeSendRoutingInfoForSMArg is the public decoder used by simulators
// and test peers that need to parse an inbound SRI-for-SM Invoke parameter
// (e.g. an in-process HLR fixture). Carrier-grade peers must use the same
// codec as the gateway to avoid wire-format drift.
func DecodeSendRoutingInfoForSMArg(b []byte) (*SendRoutingInfoForSMRequest, error) {
	return decodeSendRoutingInfoForSMArg(b)
}

// EncodeSendRoutingInfoForSMRes is the public encoder used by simulators
// and test peers that need to build an SRI-for-SM ReturnResult parameter.
func EncodeSendRoutingInfoForSMRes(r *SendRoutingInfoForSMResponse) ([]byte, error) {
	return encodeSendRoutingInfoForSMRes(r)
}

func encodeSendRoutingInfoForSMArg(r *SendRoutingInfoForSMRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("SendRoutingInfoForSMArg: nil")
	}
	msisdn, err := encodeAddressString(&AddressString{
		Nature:     NatureInternational, // SRI-for-SM always uses E.164
		NumberPlan: NumberingPlanISDN,
		Digits:     string(r.MSISDN),
	})
	if err != nil {
		return nil, fmt.Errorf("msisdn: %w", err)
	}
	sca, err := encodeAddressString(&r.ServiceCentreAddress)
	if err != nil {
		return nil, fmt.Errorf("serviceCentreAddress: %w", err)
	}
	w := wireSendRoutingInfoForSMArg{
		MSISDN:               msisdn,
		SMRPPRI:              r.SMRPPRI,
		ServiceCentreAddress: sca,
	}
	return asn1.Marshal(w)
}

func decodeSendRoutingInfoForSMRes(b []byte) (*SendRoutingInfoForSMResponse, error) {
	var w wireSendRoutingInfoForSMRes
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("SendRoutingInfoForSMRes: %w", err)
	}
	imsi, err := decodeIMSI(w.IMSI)
	if err != nil {
		return nil, err
	}
	nnn, err := decodeAddressString(w.LocationInfoWithLMSI.NetworkNodeNumber)
	if err != nil {
		return nil, fmt.Errorf("networkNode-Number: %w", err)
	}
	resp := &SendRoutingInfoForSMResponse{
		IMSI: imsi,
		LocationInfoWithLMSI: LocationInfoWithLMSI{
			NetworkNodeNumber: nnn,
			LMSI:              append([]byte(nil), w.LocationInfoWithLMSI.LMSI...),
			GPRSNodeIndicator: len(w.LocationInfoWithLMSI.GPRSNodeIndicator.FullBytes) > 0,
		},
	}
	return resp, nil
}

func encodeSendRoutingInfoForSMRes(r *SendRoutingInfoForSMResponse) ([]byte, error) {
	if r == nil || r.LocationInfoWithLMSI.NetworkNodeNumber == nil {
		return nil, errors.New("SendRoutingInfoForSMRes: networkNode-Number required")
	}
	imsi, err := encodeIMSI(r.IMSI)
	if err != nil {
		return nil, err
	}
	nnn, err := encodeAddressString(r.LocationInfoWithLMSI.NetworkNodeNumber)
	if err != nil {
		return nil, fmt.Errorf("networkNode-Number: %w", err)
	}
	w := wireSendRoutingInfoForSMRes{
		IMSI: imsi,
		LocationInfoWithLMSI: wireLocationInfoWithLMSI{
			NetworkNodeNumber: nnn,
			LMSI:              r.LocationInfoWithLMSI.LMSI,
		},
	}
	return asn1.Marshal(w)
}

// ---- SM-RP-DA / SM-RP-OA CHOICE encode/decode ----
//
// CHOICE with IMPLICIT tags: the chosen alternative is emitted as a single
// primitive [CONTEXT N] tag-length-value. The receiver inspects byte[0] to
// pick the variant:
//   0x80 = [0] imsi (primitive, OCTET STRING)        — DA only
//   0x81 = [1] lmsi (primitive, OCTET STRING)        — DA only
//   0x82 = [2] msisdn (primitive, ISDN-AddressString) — OA only
//   0x84 = [4] serviceCentreAddress(DA/OA)
//   0x85 = [5] noSM-RP-{DA,OA} (primitive, NULL — value 0x85 0x00)

func encodeSMRPDA(d SMRPDA) (asn1.RawValue, error) {
	switch {
	case d.IMSI != nil:
		b, err := encodeIMSI(*d.IMSI)
		if err != nil {
			return asn1.RawValue{}, err
		}
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: false, Bytes: b}, nil
	case len(d.LMSI) > 0:
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, IsCompound: false, Bytes: d.LMSI}, nil
	case d.ServiceCentreAddress != nil:
		b, err := encodeAddressString(d.ServiceCentreAddress)
		if err != nil {
			return asn1.RawValue{}, err
		}
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 4, IsCompound: false, Bytes: b}, nil
	case d.NoSMRPDA:
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 5, IsCompound: false, Bytes: nil}, nil
	}
	return asn1.RawValue{}, errors.New("SM-RP-DA: no variant set")
}

func decodeSMRPDA(rv asn1.RawValue) (SMRPDA, error) {
	if rv.Class != asn1.ClassContextSpecific {
		return SMRPDA{}, fmt.Errorf("SM-RP-DA: unexpected class %d", rv.Class)
	}
	switch rv.Tag {
	case 0:
		imsi, err := decodeIMSI(rv.Bytes)
		if err != nil {
			return SMRPDA{}, err
		}
		return SMRPDA{IMSI: &imsi}, nil
	case 1:
		return SMRPDA{LMSI: append([]byte(nil), rv.Bytes...)}, nil
	case 4:
		sca, err := decodeAddressString(rv.Bytes)
		if err != nil {
			return SMRPDA{}, fmt.Errorf("serviceCentreAddressDA: %w", err)
		}
		return SMRPDA{ServiceCentreAddress: sca}, nil
	case 5:
		return SMRPDA{NoSMRPDA: true}, nil
	}
	return SMRPDA{}, fmt.Errorf("SM-RP-DA: unknown tag %d", rv.Tag)
}

func encodeSMRPOA(o SMRPOA) (asn1.RawValue, error) {
	switch {
	case o.MSISDN != nil:
		b, err := encodeAddressString(&AddressString{
			Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: string(*o.MSISDN),
		})
		if err != nil {
			return asn1.RawValue{}, err
		}
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 2, IsCompound: false, Bytes: b}, nil
	case o.ServiceCentreAddress != nil:
		b, err := encodeAddressString(o.ServiceCentreAddress)
		if err != nil {
			return asn1.RawValue{}, err
		}
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 4, IsCompound: false, Bytes: b}, nil
	case o.NoSMRPOA:
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 5, IsCompound: false, Bytes: nil}, nil
	}
	return asn1.RawValue{}, errors.New("SM-RP-OA: no variant set")
}

func decodeSMRPOA(rv asn1.RawValue) (SMRPOA, error) {
	if rv.Class != asn1.ClassContextSpecific {
		return SMRPOA{}, fmt.Errorf("SM-RP-OA: unexpected class %d", rv.Class)
	}
	switch rv.Tag {
	case 2:
		addr, err := decodeAddressString(rv.Bytes)
		if err != nil {
			return SMRPOA{}, fmt.Errorf("msisdn: %w", err)
		}
		m := MSISDN(addr.Digits)
		return SMRPOA{MSISDN: &m}, nil
	case 4:
		sca, err := decodeAddressString(rv.Bytes)
		if err != nil {
			return SMRPOA{}, fmt.Errorf("serviceCentreAddressOA: %w", err)
		}
		return SMRPOA{ServiceCentreAddress: sca}, nil
	case 5:
		return SMRPOA{NoSMRPOA: true}, nil
	}
	return SMRPOA{}, fmt.Errorf("SM-RP-OA: unknown tag %d", rv.Tag)
}

// ---- MtForwardSM (op 44) ----
//
// MT-ForwardSM-Arg ::= SEQUENCE {
//   sm-RP-DA           SM-RP-DA,
//   sm-RP-OA           SM-RP-OA,
//   sm-RP-UI           SignalInfo,         -- OCTET STRING (TPDU)
//   moreMessagesToSend NULL OPTIONAL,
//   extensionContainer ExtensionContainer OPTIONAL,
//   ...
// }

type wireMtForwardSMArg struct {
	SMRPDA             asn1.RawValue
	SMRPOA             asn1.RawValue
	SMRPUI             []byte
	MoreMessagesToSend asn1.RawValue   `asn1:"optional"`
	ExtensionContainer asn1.RawValue   `asn1:"optional"`
	Rest               []asn1.RawValue `asn1:"optional"`
}

type wireForwardSMRes struct {
	SMRPUI             []byte          `asn1:"optional"`
	ExtensionContainer asn1.RawValue   `asn1:"optional"`
	Rest               []asn1.RawValue `asn1:"optional"`
}

func decodeMtForwardSMArg(b []byte) (*MtForwardSMRequest, error) {
	var w wireMtForwardSMArg
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("MtForwardSMArg: %w", err)
	}
	da, err := decodeSMRPDA(w.SMRPDA)
	if err != nil {
		return nil, err
	}
	oa, err := decodeSMRPOA(w.SMRPOA)
	if err != nil {
		return nil, err
	}
	return &MtForwardSMRequest{
		SMRPDA:             da,
		SMRPOA:             oa,
		SMRPUI:             append([]byte(nil), w.SMRPUI...),
		MoreMessagesToSend: len(w.MoreMessagesToSend.FullBytes) > 0,
	}, nil
}

// DecodeMtForwardSMArg is the public decoder for MT-Forward-SM Invokes.
// Exposed for test peers / simulators that need to parse the inbound
// parameter off the wire — single source of truth, so peer codec cannot
// drift from the dispatcher's view of the same bytes.
func DecodeMtForwardSMArg(b []byte) (*MtForwardSMRequest, error) {
	return decodeMtForwardSMArg(b)
}

// EncodeMtForwardSMArg is the public encoder used by composition roots
// that originate MT-Forward-SM toward a roamer's VMSC.
func EncodeMtForwardSMArg(r *MtForwardSMRequest) ([]byte, error) {
	return encodeMtForwardSMArg(r)
}

func encodeMtForwardSMArg(r *MtForwardSMRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("MtForwardSMArg: nil")
	}
	if len(r.SMRPUI) == 0 {
		return nil, errors.New("MtForwardSMArg: sm-RP-UI required")
	}
	da, err := encodeSMRPDA(r.SMRPDA)
	if err != nil {
		return nil, err
	}
	oa, err := encodeSMRPOA(r.SMRPOA)
	if err != nil {
		return nil, err
	}
	w := wireMtForwardSMArg{SMRPDA: da, SMRPOA: oa, SMRPUI: r.SMRPUI}
	if r.MoreMessagesToSend {
		w.MoreMessagesToSend = asn1.RawValue{Tag: asn1.TagNull, FullBytes: []byte{0x05, 0x00}}
	}
	return asn1.Marshal(w)
}

func decodeMtForwardSMRes(b []byte) (*MtForwardSMResponse, error) {
	// The result is optional (TS 29.002 §17.6.3, mt-ForwardSM RESULT
	// "-- optional"), and version 2's forwardSM answers without one: a
	// ReturnResult with no parameter is a successful delivery.
	if len(b) == 0 {
		return &MtForwardSMResponse{}, nil
	}
	var w wireForwardSMRes
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("MtForwardSMRes: %w", err)
	}
	return &MtForwardSMResponse{SMRPUI: append([]byte(nil), w.SMRPUI...)}, nil
}

func encodeMtForwardSMRes(r *MtForwardSMResponse) ([]byte, error) {
	w := wireForwardSMRes{}
	if r != nil {
		w.SMRPUI = r.SMRPUI
	}
	return asn1.Marshal(w)
}

// EncodeMtForwardSMRes is the public encoder for MT-Forward-SM ReturnResults.
// Exposed for test peers / simulators that need to produce the response
// parameter on the wire — paired with DecodeMtForwardSMArg so peer and
// gateway never disagree on the format.
func EncodeMtForwardSMRes(r *MtForwardSMResponse) ([]byte, error) {
	return encodeMtForwardSMRes(r)
}

// ---- MoForwardSM (op 46) ----
//
// MO-ForwardSM-Arg ::= SEQUENCE {
//   sm-RP-DA           SM-RP-DA,
//   sm-RP-OA           SM-RP-OA,
//   sm-RP-UI           SignalInfo,
//   extensionContainer ExtensionContainer OPTIONAL,
//   ...,
//   imsi               IMSI OPTIONAL  -- post-Rel-99
// }

type wireMoForwardSMArg struct {
	SMRPDA asn1.RawValue
	SMRPOA asn1.RawValue
	SMRPUI []byte
	// extensionContainer is technically OPTIONAL between sm-RP-UI and imsi
	// per TS 29.002 v17 §17.7.5, but a tag-less SEQUENCE OPTIONAL placed
	// between two OPTIONAL OCTET STRING positions confuses encoding/asn1's
	// sequential matcher (the optional RawValue greedy-eats the IMSI).
	// We accept the extension container only as part of the trailing Rest
	// pile — anything between sm-RP-UI and the optional IMSI is preserved
	// for forward-compat but not surfaced as a typed field. Carriers that
	// need to inspect the container can decode `Rest` themselves.
	IMSI []byte          `asn1:"optional"`
	Rest []asn1.RawValue `asn1:"optional"`
}

func decodeMoForwardSMArg(b []byte) (*MoForwardSMRequest, error) {
	var w wireMoForwardSMArg
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("MoForwardSMArg: %w", err)
	}
	da, err := decodeSMRPDA(w.SMRPDA)
	if err != nil {
		return nil, err
	}
	oa, err := decodeSMRPOA(w.SMRPOA)
	if err != nil {
		return nil, err
	}
	req := &MoForwardSMRequest{
		SMRPDA: da,
		SMRPOA: oa,
		SMRPUI: append([]byte(nil), w.SMRPUI...),
	}
	if len(w.IMSI) > 0 {
		imsi, err := decodeIMSI(w.IMSI)
		if err != nil {
			return nil, err
		}
		req.IMSI = &imsi
	}
	return req, nil
}

// EncodeMoForwardSMArg is the public encoder for MO-Forward-SM, exposed
// for symmetry with the Mt/SRI public encoders.
func EncodeMoForwardSMArg(r *MoForwardSMRequest) ([]byte, error) {
	return encodeMoForwardSMArg(r)
}

func encodeMoForwardSMArg(r *MoForwardSMRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("MoForwardSMArg: nil")
	}
	if len(r.SMRPUI) == 0 {
		return nil, errors.New("MoForwardSMArg: sm-RP-UI required")
	}
	da, err := encodeSMRPDA(r.SMRPDA)
	if err != nil {
		return nil, err
	}
	oa, err := encodeSMRPOA(r.SMRPOA)
	if err != nil {
		return nil, err
	}
	w := wireMoForwardSMArg{SMRPDA: da, SMRPOA: oa, SMRPUI: r.SMRPUI}
	if r.IMSI != nil {
		imsi, err := encodeIMSI(*r.IMSI)
		if err != nil {
			return nil, err
		}
		w.IMSI = imsi
	}
	return asn1.Marshal(w)
}

func decodeMoForwardSMRes(b []byte) (*MoForwardSMResponse, error) {
	var w wireForwardSMRes
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("MoForwardSMRes: %w", err)
	}
	return &MoForwardSMResponse{SMRPUI: append([]byte(nil), w.SMRPUI...)}, nil
}

func encodeMoForwardSMRes(r *MoForwardSMResponse) ([]byte, error) {
	w := wireForwardSMRes{}
	if r != nil {
		w.SMRPUI = r.SMRPUI
	}
	return asn1.Marshal(w)
}
