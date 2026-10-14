// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The HLR side of SendRoutingInfo: the argument decoder and the result
// encoder, for test peers (see location_peer.go).

// DecodeSendRoutingInfoArg decodes SendRoutingInfoArg: the fields
// SendRoutingInfoRequest holds, the three mandatory ones required.
func DecodeSendRoutingInfoArg(b []byte) (*SendRoutingInfoRequest, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("SendRoutingInfoArg: %w", err)
	}
	req := &SendRoutingInfoRequest{}
	var haveMSISDN, haveType, haveGMSC bool
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific || f.IsCompound {
			continue
		}
		switch f.Tag {
		case 0:
			a, err := decodeISDNAddressString("msisdn", f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("SendRoutingInfoArg: %w", err)
			}
			req.MSISDN = MSISDN(a.Digits)
			haveMSISDN = true
		case 3:
			v, err := berInt(f.Bytes)
			if err != nil || v < 0 || v > int(InterrogationForwarding) {
				return nil, fmt.Errorf("SendRoutingInfoArg: interrogationType %v", f.Bytes)
			}
			req.InterrogationType = InterrogationType(v)
			haveType = true
		case 6:
			a, err := decodeISDNAddressString("gmsc-OrGsmSCF-Address", f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("SendRoutingInfoArg: %w", err)
			}
			req.GMSCAddress = *a
			haveGMSC = true
		}
	}
	if !haveMSISDN || !haveType || !haveGMSC {
		return nil, errors.New("SendRoutingInfoArg: mandatory field missing")
	}
	return req, nil
}

// EncodeSendRoutingInfoRes encodes SendRoutingInfoRes ::= [3] SEQUENCE,
// its fields in declaration order.
func EncodeSendRoutingInfoRes(r *SendRoutingInfoResponse) ([]byte, error) {
	if r == nil {
		return nil, errors.New("SendRoutingInfoRes: nil")
	}
	var content []byte
	add := func(tag int, compound bool, v []byte) {
		content = append(content, tlv(asn1.ClassContextSpecific, tag, compound, v)...)
	}
	addr := func(tag int, a *AddressString) error {
		b, err := encodeISDNAddressString(a)
		if err == nil {
			add(tag, false, b)
		}
		return err
	}
	if r.IMSI != "" {
		b, err := encodeIMSI(r.IMSI)
		if err != nil {
			return nil, err
		}
		add(9, false, b)
	}
	switch n := btoi(r.RoamingNumber != nil) + btoi(r.ForwardingData != nil) + btoi(r.CamelRoutingInfo != nil); {
	case n > 1:
		return nil, errors.New("SendRoutingInfoRes: more than one extendedRoutingInfo alternative")
	case r.RoamingNumber != nil:
		b, err := encodeISDNAddressString(r.RoamingNumber)
		if err != nil {
			return nil, fmt.Errorf("roamingNumber: %w", err)
		}
		content = append(content, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, b)...)
	case r.ForwardingData != nil:
		b, err := encodeForwardingData(r.ForwardingData)
		if err != nil {
			return nil, err
		}
		content = append(content, tlv(asn1.ClassUniversal, asn1.TagSequence, true, b)...)
	case r.CamelRoutingInfo != nil:
		var b []byte
		if fd := r.CamelRoutingInfo.ForwardingData; fd != nil {
			f, err := encodeForwardingData(fd)
			if err != nil {
				return nil, err
			}
			b = tlv(asn1.ClassUniversal, asn1.TagSequence, true, f)
		}
		if len(r.CamelRoutingInfo.GMSCCamelSubscriptionInfo) == 0 {
			return nil, errors.New("camelRoutingInfo: gmscCamelSubscriptionInfo required")
		}
		b = append(b, r.CamelRoutingInfo.GMSCCamelSubscriptionInfo...)
		add(8, true, b)
	}
	if r.SubscriberInfo != nil {
		seq, err := encodeSubscriberInfo(r.SubscriberInfo)
		if err != nil {
			return nil, err
		}
		var rv asn1.RawValue
		if _, err := asn1.Unmarshal(seq, &rv); err != nil {
			return nil, err
		}
		add(7, true, rv.Bytes)
	}
	if len(r.SSList) > 0 {
		var codes []byte
		for _, c := range r.SSList {
			codes = append(codes, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{c})...)
		}
		add(1, true, codes)
	}
	if bs := r.BasicService; bs != nil {
		tag := 3
		if bs.Bearer {
			tag = 2
		}
		add(5, true, tlv(asn1.ClassContextSpecific, tag, false, bs.Code))
	}
	if r.ForwardingInterrogationRequired {
		add(4, false, nil)
	}
	if r.VMSCAddress != nil {
		if err := addr(2, r.VMSCAddress); err != nil {
			return nil, fmt.Errorf("vmsc-Address: %w", err)
		}
	}
	if r.MSISDN != nil {
		if err := addr(12, r.MSISDN); err != nil {
			return nil, fmt.Errorf("msisdn: %w", err)
		}
	}
	if r.NumberPortabilityStatus != "" {
		v := -1
		for n, name := range numberPortabilityStatusNames {
			if name == r.NumberPortabilityStatus {
				v = n
			}
		}
		if v < 0 {
			return nil, fmt.Errorf("numberPortabilityStatus %q", r.NumberPortabilityStatus)
		}
		add(13, false, intContent(v))
	}
	return tlv(asn1.ClassContextSpecific, 3, true, content), nil
}

func encodeForwardingData(fd *ForwardingData) ([]byte, error) {
	var out []byte
	if fd.ForwardedToNumber != nil {
		b, err := encodeISDNAddressString(fd.ForwardedToNumber)
		if err != nil {
			return nil, fmt.Errorf("forwardedToNumber: %w", err)
		}
		out = append(out, tlv(asn1.ClassContextSpecific, 5, false, b)...)
	}
	if fd.ForwardedToSubaddress != nil {
		out = append(out, tlv(asn1.ClassContextSpecific, 4, false, fd.ForwardedToSubaddress)...)
	}
	if fd.ForwardingOptions != nil {
		out = append(out, tlv(asn1.ClassContextSpecific, 6, false, []byte{*fd.ForwardingOptions})...)
	}
	if fd.LongForwardedToNumber != nil {
		b, err := encodeAddressString(fd.LongForwardedToNumber)
		if err != nil || len(b) > maxFTNAddressLength {
			return nil, fmt.Errorf("longForwardedToNumber: %d octets, %v", len(b), err)
		}
		out = append(out, tlv(asn1.ClassContextSpecific, 8, false, b)...)
	}
	return out, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
