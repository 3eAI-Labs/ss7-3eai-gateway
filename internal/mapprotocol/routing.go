// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// SendRoutingInfo (op 22, GMSC → HLR, TS 29.002 §10.1) under
// locationInfoRetrievalContext-v3. Module MAP-CH-DataTypes is IMPLICIT TAGS;
// as in location.go, a tagged CHOICE (basicService [5]) is tagged
// explicitly, and the structures are walked TLV by TLV.

// InterrogationType is TS 29.002 InterrogationType.
type InterrogationType uint8

const (
	InterrogationBasicCall  InterrogationType = 0
	InterrogationForwarding InterrogationType = 1
)

// SendRoutingInfoRequest is SendRoutingInfoArg, the fields the gateway sends.
type SendRoutingInfoRequest struct {
	MSISDN            MSISDN
	InterrogationType InterrogationType
	// GMSCAddress is gmsc-OrGsmSCF-Address: the GMSC's own E.164 address.
	GMSCAddress AddressString
}

// SendRoutingInfoResponse is SendRoutingInfoRes, the fields the gateway
// reads. At most one of RoamingNumber, ForwardingData and CamelRoutingInfo
// (ExtendedRoutingInfo) is set.
type SendRoutingInfoResponse struct {
	IMSI             IMSI // "" when absent
	RoamingNumber    *AddressString
	ForwardingData   *ForwardingData
	CamelRoutingInfo *CamelRoutingInfo
	SubscriberInfo   *SubscriberInfo
	// SSList holds the SS-Codes of ss-List, one octet each.
	SSList                          []byte
	BasicService                    *BasicServiceCode
	ForwardingInterrogationRequired bool
	VMSCAddress                     *AddressString
	MSISDN                          *AddressString
	// NumberPortabilityStatus is the value's ASN.1 name; "" when absent or
	// a value the specification does not list.
	NumberPortabilityStatus string
}

// ForwardingData is TS 29.002 ForwardingData.
type ForwardingData struct {
	ForwardedToNumber     *AddressString // [5] ISDN-AddressString
	ForwardedToSubaddress []byte         // [4]
	ForwardingOptions     *byte          // [6], one octet
	LongForwardedToNumber *AddressString // [8] FTN-AddressString
}

// CamelRoutingInfo is TS 29.002 CamelRoutingInfo. GMSCCamelSubscriptionInfo
// is the [0] element as received, tag and length included.
type CamelRoutingInfo struct {
	ForwardingData            *ForwardingData
	GMSCCamelSubscriptionInfo []byte
}

// BasicServiceCode is Ext-BasicServiceCode: ext-BearerService [2] (Bearer)
// or ext-Teleservice [3].
type BasicServiceCode struct {
	Bearer bool
	Code   []byte
}

// Size limits of TS 29.002 MAP-CommonDataTypes.
const (
	maxISDNAddressLength       = 9
	maxFTNAddressLength        = 15
	maxISDNSubaddressLength    = 21
	maxNumOfSS                 = 30
	maxExtBasicServiceCodeSize = 5
)

// EncodeSendRoutingInfoArg encodes SendRoutingInfoArg:
//
//	SEQUENCE { msisdn [0] ISDN-AddressString,
//	           interrogationType [3] InterrogationType,
//	           gmsc-OrGsmSCF-Address [6] ISDN-AddressString, … }
func EncodeSendRoutingInfoArg(r *SendRoutingInfoRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("SendRoutingInfoArg: nil")
	}
	if r.InterrogationType > InterrogationForwarding {
		return nil, fmt.Errorf("SendRoutingInfoArg: interrogationType %d", r.InterrogationType)
	}
	msisdn, err := encodeISDNAddressString(&AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: string(r.MSISDN)})
	if err != nil {
		return nil, fmt.Errorf("SendRoutingInfoArg: msisdn: %w", err)
	}
	gmsc, err := encodeISDNAddressString(&r.GMSCAddress)
	if err != nil {
		return nil, fmt.Errorf("SendRoutingInfoArg: gmsc-OrGsmSCF-Address: %w", err)
	}
	return sequence(
		tlv(asn1.ClassContextSpecific, 0, false, msisdn),
		tlv(asn1.ClassContextSpecific, 3, false, []byte{byte(r.InterrogationType)}),
		tlv(asn1.ClassContextSpecific, 6, false, gmsc),
	), nil
}

// DecodeSendRoutingInfoRes decodes SendRoutingInfoRes ::= [3] SEQUENCE { … }.
// Fields the gateway does not use — CUG, CCBS, IST, the CAMEL phases of the
// VMSC, the second service's routingInfo2 / ss-List2 / basicService2 /
// allowedServices / unavailabilityCause (SCUDIF), extension containers —
// are skipped.
func DecodeSendRoutingInfoRes(b []byte) (*SendRoutingInfoResponse, error) {
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(b, &outer)
	if err != nil {
		return nil, fmt.Errorf("SendRoutingInfoRes: %w", err)
	}
	if len(rest) > 0 {
		return nil, errors.New("SendRoutingInfoRes: trailing data")
	}
	if outer.Class != asn1.ClassContextSpecific || outer.Tag != 3 || !outer.IsCompound {
		return nil, errors.New("SendRoutingInfoRes: not a [3] SEQUENCE")
	}
	fields, err := berChildren(outer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SendRoutingInfoRes: %w", err)
	}
	r := &SendRoutingInfoResponse{}
	routing := 0
	for _, f := range fields {
		var err error
		switch {
		// extendedRoutingInfo: routingInfo is an untagged CHOICE of an
		// untagged ISDN-AddressString and an untagged SEQUENCE.
		case isUniversal(f, asn1.TagOctetString, false):
			routing++
			r.RoamingNumber, err = decodeISDNAddressString("roamingNumber", f.Bytes)
		case isUniversal(f, asn1.TagSequence, true):
			routing++
			r.ForwardingData, err = decodeForwardingData(f.Bytes)
		case f.Class != asn1.ClassContextSpecific:
		case f.Tag == 8 && f.IsCompound:
			routing++
			r.CamelRoutingInfo, err = decodeCamelRoutingInfo(f.Bytes)
		case f.Tag == 9 && !f.IsCompound:
			r.IMSI, err = decodeIMSI(f.Bytes)
		case f.Tag == 7 && f.IsCompound:
			r.SubscriberInfo, err = decodeSubscriberInfo(f.Bytes)
		case f.Tag == 1 && f.IsCompound:
			r.SSList, err = decodeSSList(f.Bytes)
		case f.Tag == 5 && f.IsCompound:
			r.BasicService, err = decodeExtBasicServiceCode(f.Bytes)
		case f.Tag == 4 && !f.IsCompound:
			if len(f.Bytes) != 0 {
				err = errors.New("forwardingInterrogationRequired: NULL with content")
			}
			r.ForwardingInterrogationRequired = true
		case f.Tag == 2 && !f.IsCompound:
			r.VMSCAddress, err = decodeISDNAddressString("vmsc-Address", f.Bytes)
		case f.Tag == 12 && !f.IsCompound:
			r.MSISDN, err = decodeISDNAddressString("msisdn", f.Bytes)
		case f.Tag == 13 && !f.IsCompound:
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				// "reception of other values … the receiver shall ignore the
				// whole NumberPortabilityStatus".
				r.NumberPortabilityStatus = numberPortabilityStatusNames[v]
			}
		}
		if err != nil {
			return nil, fmt.Errorf("SendRoutingInfoRes: %w", err)
		}
	}
	if routing > 1 {
		return nil, errors.New("SendRoutingInfoRes: extendedRoutingInfo has more than one alternative")
	}
	return r, nil
}

// MergeSendRoutingInfoRes combines the segments of a result sent as
// TC-Result-NL components and a TC-Result-L (TS 29.002 §C.2.3: each segment
// is itself a SendRoutingInfoRes; table 7.5/4 note 4: they make one
// confirmation). A field takes its value from the first segment that has
// it; the ss-Lists are concatenated.
func MergeSendRoutingInfoRes(segments []*SendRoutingInfoResponse) (*SendRoutingInfoResponse, error) {
	out := &SendRoutingInfoResponse{}
	routing := false
	for _, s := range segments {
		if out.IMSI == "" {
			out.IMSI = s.IMSI
		}
		if s.RoamingNumber != nil || s.ForwardingData != nil || s.CamelRoutingInfo != nil {
			if routing {
				return nil, errors.New("SendRoutingInfoRes: extendedRoutingInfo in more than one segment")
			}
			routing = true
			out.RoamingNumber, out.ForwardingData, out.CamelRoutingInfo = s.RoamingNumber, s.ForwardingData, s.CamelRoutingInfo
		}
		if out.SubscriberInfo == nil {
			out.SubscriberInfo = s.SubscriberInfo
		}
		out.SSList = append(out.SSList, s.SSList...)
		if out.BasicService == nil {
			out.BasicService = s.BasicService
		}
		out.ForwardingInterrogationRequired = out.ForwardingInterrogationRequired || s.ForwardingInterrogationRequired
		if out.VMSCAddress == nil {
			out.VMSCAddress = s.VMSCAddress
		}
		if out.MSISDN == nil {
			out.MSISDN = s.MSISDN
		}
		if out.NumberPortabilityStatus == "" {
			out.NumberPortabilityStatus = s.NumberPortabilityStatus
		}
	}
	if len(out.SSList) > maxNumOfSS {
		return nil, fmt.Errorf("SendRoutingInfoRes: ss-List of %d codes, at most %d", len(out.SSList), maxNumOfSS)
	}
	return out, nil
}

// DecodeSendRoutingInfoResult decodes the result of a SendRoutingInfo: one
// SendRoutingInfoRes, or the segments of one sent as TC-Result-NL
// components and a TC-Result-L, merged. A result without the IMSI is
// incomplete — "IMSI must be present if SendRoutingInfoRes is not
// segmented … in one segmented transmission" (TS 29.002) — which is also
// what a segment overtaken by the last one leaves.
func DecodeSendRoutingInfoResult(params [][]byte) (*SendRoutingInfoResponse, error) {
	segments := make([]*SendRoutingInfoResponse, 0, len(params))
	for i, p := range params {
		seg, err := DecodeSendRoutingInfoRes(p)
		if err != nil {
			if len(params) > 1 {
				return nil, fmt.Errorf("segment %d: %w", i+1, err)
			}
			return nil, err
		}
		segments = append(segments, seg)
	}
	r, err := MergeSendRoutingInfoRes(segments)
	if err != nil {
		return nil, err
	}
	if r.IMSI == "" {
		return nil, errors.New("SendRoutingInfoRes without the IMSI: incomplete result")
	}
	return r, nil
}

// decodeForwardingData reads the content of ForwardingData.
func decodeForwardingData(b []byte) (*ForwardingData, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("forwardingData: %w", err)
	}
	fd := &ForwardingData{}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific || f.IsCompound {
			continue
		}
		var err error
		switch f.Tag {
		case 5:
			fd.ForwardedToNumber, err = decodeISDNAddressString("forwardedToNumber", f.Bytes)
		case 4:
			fd.ForwardedToSubaddress, err = fixedOctets("forwardedToSubaddress", f.Bytes, 1, maxISDNSubaddressLength)
		case 6:
			if len(f.Bytes) != 1 {
				err = fmt.Errorf("forwardingOptions: %d octets, want 1", len(f.Bytes))
			} else {
				o := f.Bytes[0]
				fd.ForwardingOptions = &o
			}
		case 8:
			if len(f.Bytes) > maxFTNAddressLength {
				err = fmt.Errorf("longForwardedToNumber: %d octets, at most %d", len(f.Bytes), maxFTNAddressLength)
			} else {
				fd.LongForwardedToNumber, err = decodeAddressString(f.Bytes)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("forwardingData: %w", err)
		}
	}
	return fd, nil
}

// decodeCamelRoutingInfo reads the content of CamelRoutingInfo:
// SEQUENCE { forwardingData OPTIONAL, gmscCamelSubscriptionInfo [0], … }.
func decodeCamelRoutingInfo(b []byte) (*CamelRoutingInfo, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("camelRoutingInfo: %w", err)
	}
	c := &CamelRoutingInfo{}
	for _, f := range fields {
		switch {
		case isUniversal(f, asn1.TagSequence, true):
			if c.ForwardingData, err = decodeForwardingData(f.Bytes); err != nil {
				return nil, fmt.Errorf("camelRoutingInfo: %w", err)
			}
		case f.Class == asn1.ClassContextSpecific && f.Tag == 0 && f.IsCompound:
			c.GMSCCamelSubscriptionInfo = append([]byte(nil), f.FullBytes...)
		}
	}
	if c.GMSCCamelSubscriptionInfo == nil {
		return nil, errors.New("camelRoutingInfo: gmscCamelSubscriptionInfo missing")
	}
	return c, nil
}

// decodeSSList reads SS-List ::= SEQUENCE SIZE (1..maxNumOfSS) OF SS-Code,
// SS-Code ::= OCTET STRING (SIZE (1)).
func decodeSSList(b []byte) ([]byte, error) {
	codes, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("ss-List: %w", err)
	}
	if len(codes) < 1 || len(codes) > maxNumOfSS {
		return nil, fmt.Errorf("ss-List: %d codes, want 1..%d", len(codes), maxNumOfSS)
	}
	out := make([]byte, 0, len(codes))
	for _, c := range codes {
		if !isUniversal(c, asn1.TagOctetString, false) || len(c.Bytes) != 1 {
			return nil, errors.New("ss-List: SS-Code is not a one-octet OCTET STRING")
		}
		out = append(out, c.Bytes[0])
	}
	return out, nil
}

// decodeExtBasicServiceCode reads the content of the explicitly tagged
// CHOICE Ext-BasicServiceCode.
func decodeExtBasicServiceCode(b []byte) (*BasicServiceCode, error) {
	alt, err := onlyChild(b, "basicService")
	if err != nil {
		return nil, err
	}
	if alt.Class != asn1.ClassContextSpecific || alt.IsCompound || (alt.Tag != 2 && alt.Tag != 3) {
		return nil, fmt.Errorf("basicService: unknown alternative [%d]", alt.Tag)
	}
	code, err := fixedOctets("basicService", alt.Bytes, 1, maxExtBasicServiceCodeSize)
	if err != nil {
		return nil, err
	}
	return &BasicServiceCode{Bearer: alt.Tag == 2, Code: code}, nil
}

// decodeISDNAddressString reads an ISDN-AddressString. The type allows
// maxISDN-AddressLength (9) octets; a received one is accepted up to the
// AddressString limit (20), so that a roaming number an NPLR built from a
// routing number and the MSISDN is not lost over its length. The encoder
// keeps to 9.
func decodeISDNAddressString(name string, b []byte) (*AddressString, error) {
	a, err := decodeAddressString(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return a, nil
}

func encodeISDNAddressString(a *AddressString) ([]byte, error) {
	b, err := encodeAddressString(a)
	if err != nil {
		return nil, err
	}
	if len(b) > maxISDNAddressLength {
		return nil, fmt.Errorf("ISDN-AddressString of %d octets, at most %d", len(b), maxISDNAddressLength)
	}
	return b, nil
}
