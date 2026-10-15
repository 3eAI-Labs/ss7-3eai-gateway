// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The VLR side of sendAuthenticationInfo and purgeMS: what a test peer
// playing the VLR encodes and decodes. The decoders are strict — fields in
// declaration order, lengths as the types fix them — so that the peer
// reports what a real VLR would refuse.

// EncodeSendAuthenticationInfoArg encodes SendAuthenticationInfoArg in
// declaration order.
func EncodeSendAuthenticationInfoArg(a *SendAuthenticationInfoArg) ([]byte, error) {
	if a == nil {
		return nil, errors.New("SendAuthenticationInfoArg: nil")
	}
	if a.NumberOfRequestedVectors < 1 || a.NumberOfRequestedVectors > MaxVectors {
		return nil, fmt.Errorf("SendAuthenticationInfoArg: numberOfRequestedVectors %d", a.NumberOfRequestedVectors)
	}
	imsi, err := encodeIMSI(a.IMSI)
	if err != nil {
		return nil, err
	}
	octet := func(b []byte) []byte { return tlv(asn1.ClassUniversal, asn1.TagOctetString, false, b) }
	fields := [][]byte{
		tlv(asn1.ClassContextSpecific, 0, false, imsi),
		tlv(asn1.ClassUniversal, asn1.TagInteger, false, []byte{byte(a.NumberOfRequestedVectors)}),
	}
	if a.SegmentationProhibited {
		fields = append(fields, tlv(asn1.ClassUniversal, asn1.TagNull, false, nil))
	}
	if a.ImmediateResponsePreferred {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 1, false, nil))
	}
	if a.ResyncRAND != nil || a.ResyncAUTS != nil {
		fields = append(fields, sequence(octet(a.ResyncRAND), octet(a.ResyncAUTS)))
	}
	if t := a.RequestingNodeType; t != nil {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 3, false, []byte{byte(*t)}))
	}
	if a.RequestingPLMNID != nil {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 4, false, a.RequestingPLMNID))
	}
	if a.AdditionalVectors > 0 {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 5, false, []byte{byte(a.AdditionalVectors)}))
	}
	if a.AdditionalVectorsAreForEPS {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 6, false, nil))
	}
	if a.UEUsageTypeRequested {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 7, false, nil))
	}
	return sequence(fields...), nil
}

// DecodeSendAuthenticationInfoRes decodes SendAuthenticationInfoRes, A3
// with its elements in declaration order. A ReturnResult without a
// parameter is the empty response too.
func DecodeSendAuthenticationInfoRes(b []byte) (*SendAuthenticationInfoRes, error) {
	r := &SendAuthenticationInfoRes{}
	if len(b) == 0 {
		return r, nil
	}
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(b, &outer)
	if err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoRes: %w", err)
	}
	if len(rest) > 0 || !isContext(outer, 3, true) {
		return nil, errors.New("SendAuthenticationInfoRes: not one [3] SEQUENCE")
	}
	fields, err := berChildren(outer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoRes: %w", err)
	}
	last := -1
	for _, f := range fields {
		if isUniversal(f, asn1.TagSequence, true) {
			continue // extensionContainer
		}
		if f.Class != asn1.ClassContextSpecific || f.IsCompound == (f.Tag == 3) {
			return nil, fmt.Errorf("SendAuthenticationInfoRes: unexpected element class %d tag %d", f.Class, f.Tag)
		}
		if f.Tag <= last {
			return nil, fmt.Errorf("SendAuthenticationInfoRes: [%d] after [%d]", f.Tag, last)
		}
		last = f.Tag
		if f.Tag <= 1 {
			last = 1 // the CHOICE: [0] and [1] exclude each other
		}
		vectors, err := berChildren(f.Bytes)
		if f.Tag != 3 && (err != nil || len(vectors) == 0) {
			return nil, fmt.Errorf("SendAuthenticationInfoRes: list [%d] empty or malformed", f.Tag)
		}
		switch f.Tag {
		case 0:
			for _, v := range vectors {
				o, err := vectorOctets(v, 3)
				if err != nil {
					return nil, fmt.Errorf("SendAuthenticationInfoRes: triplet: %w", err)
				}
				r.Triplets = append(r.Triplets, AuthenticationTriplet{RAND: o[0], SRES: o[1], Kc: o[2]})
			}
		case 1:
			for _, v := range vectors {
				o, err := vectorOctets(v, 5)
				if err != nil {
					return nil, fmt.Errorf("SendAuthenticationInfoRes: quintuplet: %w", err)
				}
				r.Quintuplets = append(r.Quintuplets, AuthenticationQuintuplet{RAND: o[0], XRES: o[1], CK: o[2], IK: o[3], AUTN: o[4]})
			}
		case 2:
			for _, v := range vectors {
				o, err := vectorOctets(v, 4)
				if err != nil {
					return nil, fmt.Errorf("SendAuthenticationInfoRes: EPC-AV: %w", err)
				}
				r.EPSVectors = append(r.EPSVectors, EPCAV{RAND: o[0], XRES: o[1], AUTN: o[2], KASME: o[3]})
			}
		case 3:
			r.UEUsageType = append([]byte(nil), f.Bytes...)
		default:
			return nil, fmt.Errorf("SendAuthenticationInfoRes: unexpected [%d]", f.Tag)
		}
	}
	if err := r.Validate(nil); err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoRes: %w", err)
	}
	return r, nil
}

// vectorOctets reads a vector, a SEQUENCE whose first n elements are
// OCTET STRINGs.
func vectorOctets(v asn1.RawValue, n int) ([][]byte, error) {
	if !isUniversal(v, asn1.TagSequence, true) {
		return nil, errors.New("not a SEQUENCE")
	}
	fields, err := berChildren(v.Bytes)
	if err != nil {
		return nil, err
	}
	if len(fields) < n {
		return nil, fmt.Errorf("%d elements, want %d", len(fields), n)
	}
	out := make([][]byte, n)
	for i := range n {
		if !isUniversal(fields[i], asn1.TagOctetString, false) {
			return nil, fmt.Errorf("element %d is not an OCTET STRING", i)
		}
		out[i] = append([]byte(nil), fields[i].Bytes...)
	}
	return out, nil
}

// EncodePurgeMSArg encodes PurgeMS-Arg with the fields the gateway reads.
func EncodePurgeMSArg(a *PurgeMSArg) ([]byte, error) {
	if a == nil {
		return nil, errors.New("PurgeMS-Arg: nil")
	}
	imsi, err := encodeIMSI(a.IMSI)
	if err != nil {
		return nil, err
	}
	content := tlv(asn1.ClassUniversal, asn1.TagOctetString, false, imsi)
	for i, n := range []*AddressString{a.VLRNumber, a.SGSNNumber} {
		if n == nil {
			continue
		}
		enc, err := encodeISDNAddressString(n)
		if err != nil {
			return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, i, false, enc)...)
	}
	return tlv(asn1.ClassContextSpecific, 3, true, content), nil
}

// DecodePurgeMSRes decodes PurgeMS-Res, its NULLs in declaration order. A
// ReturnResult without a parameter is a result with nothing to freeze.
func DecodePurgeMSRes(b []byte) (*PurgeMSRes, error) {
	r := &PurgeMSRes{}
	if len(b) == 0 {
		return r, nil
	}
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("PurgeMS-Res: %w", err)
	}
	last := -1
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue // extensionContainer
		}
		if f.IsCompound || len(f.Bytes) != 0 || f.Tag > 2 || f.Tag <= last {
			return nil, fmt.Errorf("PurgeMS-Res: unexpected [%d]", f.Tag)
		}
		last = f.Tag
		switch f.Tag {
		case 0:
			r.FreezeTMSI = true
		case 1:
			r.FreezePTMSI = true
		case 2:
			r.FreezeMTMSI = true
		}
	}
	return r, nil
}
