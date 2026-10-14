// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The HLR / VLR side of AnyTimeInterrogation and ProvideSubscriberInfo:
// argument decoders and result encoders. The gateway does not answer these
// operations; test peers do, with the same codec, so that a wrong tag shows
// up as a decode failure on one side instead of agreeing with itself.

// DecodeAnyTimeInterrogationArg decodes AnyTimeInterrogationArg.
func DecodeAnyTimeInterrogationArg(b []byte) (*AnyTimeInterrogationRequest, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("AnyTimeInterrogationArg: %w", err)
	}
	req := &AnyTimeInterrogationRequest{}
	var haveIdentity, haveRequested, haveSCF bool
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue
		}
		switch {
		case f.Tag == 0 && f.IsCompound:
			alt, err := onlyChild(f.Bytes, "subscriberIdentity")
			if err != nil {
				return nil, fmt.Errorf("AnyTimeInterrogationArg: %w", err)
			}
			switch {
			case alt.Class == asn1.ClassContextSpecific && alt.Tag == 0 && !alt.IsCompound:
				imsi, err := decodeIMSI(alt.Bytes)
				if err != nil {
					return nil, fmt.Errorf("AnyTimeInterrogationArg: %w", err)
				}
				req.SubscriberIdentity.IMSI = &imsi
			case alt.Class == asn1.ClassContextSpecific && alt.Tag == 1 && !alt.IsCompound:
				addr, err := decodeAddressString(alt.Bytes)
				if err != nil {
					return nil, fmt.Errorf("AnyTimeInterrogationArg: msisdn: %w", err)
				}
				msisdn := MSISDN(addr.Digits)
				req.SubscriberIdentity.MSISDN = &msisdn
			default:
				return nil, fmt.Errorf("AnyTimeInterrogationArg: subscriberIdentity alternative [%d]", alt.Tag)
			}
			haveIdentity = true
		case f.Tag == 1 && f.IsCompound:
			if req.RequestedInfo, err = decodeRequestedInfo(f.Bytes); err != nil {
				return nil, fmt.Errorf("AnyTimeInterrogationArg: %w", err)
			}
			haveRequested = true
		case f.Tag == 3 && !f.IsCompound:
			scf, err := decodeAddressString(f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("AnyTimeInterrogationArg: gsmSCF-Address: %w", err)
			}
			req.GSMSCFAddress = *scf
			haveSCF = true
		}
	}
	if !haveIdentity || !haveRequested || !haveSCF {
		return nil, errors.New("AnyTimeInterrogationArg: mandatory field missing")
	}
	return req, nil
}

// DecodeProvideSubscriberInfoArg decodes ProvideSubscriberInfoArg.
func DecodeProvideSubscriberInfoArg(b []byte) (*ProvideSubscriberInfoRequest, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("ProvideSubscriberInfoArg: %w", err)
	}
	req := &ProvideSubscriberInfoRequest{}
	var haveIMSI, haveRequested bool
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue
		}
		switch {
		case f.Tag == 0 && !f.IsCompound:
			if req.IMSI, err = decodeIMSI(f.Bytes); err != nil {
				return nil, fmt.Errorf("ProvideSubscriberInfoArg: %w", err)
			}
			haveIMSI = true
		case f.Tag == 1 && !f.IsCompound:
			if req.LMSI, err = fixedOctets("lmsi", f.Bytes, 4, 4); err != nil {
				return nil, fmt.Errorf("ProvideSubscriberInfoArg: %w", err)
			}
		case f.Tag == 2 && f.IsCompound:
			if req.RequestedInfo, err = decodeRequestedInfo(f.Bytes); err != nil {
				return nil, fmt.Errorf("ProvideSubscriberInfoArg: %w", err)
			}
			haveRequested = true
		}
	}
	if !haveIMSI || !haveRequested {
		return nil, errors.New("ProvideSubscriberInfoArg: mandatory field missing")
	}
	return req, nil
}

func decodeRequestedInfo(b []byte) (RequestedInfo, error) {
	fields, err := berChildren(b)
	if err != nil {
		return RequestedInfo{}, fmt.Errorf("requestedInfo: %w", err)
	}
	var ri RequestedInfo
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific || f.IsCompound {
			continue
		}
		switch f.Tag {
		case 0:
			ri.LocationInformation = true
		case 1:
			ri.SubscriberState = true
		case 3:
			ri.CurrentLocation = true
		case 4:
			v, err := berInt(f.Bytes)
			if err != nil {
				return RequestedInfo{}, fmt.Errorf("requestedDomain: %w", err)
			}
			d := DomainCS // "reception of values > 1 shall be mapped to 'cs-Domain'"
			if v == 1 {
				d = DomainPS
			}
			ri.RequestedDomain = &d
		case 5:
			ri.MSClassmark = true
		case 6:
			ri.IMEI = true
		case 7:
			ri.MNPRequestedInfo = true
		case 11:
			ri.LocationInformationEPSSupported = true
		}
	}
	return ri, nil
}

// EncodeAnyTimeInterrogationRes encodes AnyTimeInterrogationRes.
func EncodeAnyTimeInterrogationRes(r *AnyTimeInterrogationResponse) ([]byte, error) {
	if r == nil {
		return nil, errors.New("AnyTimeInterrogationRes: nil")
	}
	info, err := encodeSubscriberInfo(&r.SubscriberInfo)
	if err != nil {
		return nil, fmt.Errorf("AnyTimeInterrogationRes: %w", err)
	}
	return sequence(info), nil
}

// EncodeProvideSubscriberInfoRes encodes ProvideSubscriberInfoRes.
func EncodeProvideSubscriberInfoRes(r *ProvideSubscriberInfoResponse) ([]byte, error) {
	if r == nil {
		return nil, errors.New("ProvideSubscriberInfoRes: nil")
	}
	info, err := encodeSubscriberInfo(&r.SubscriberInfo)
	if err != nil {
		return nil, fmt.Errorf("ProvideSubscriberInfoRes: %w", err)
	}
	return sequence(info), nil
}

// encodeSubscriberInfo encodes SubscriberInfo (untagged SEQUENCE), its
// fields in declaration order.
func encodeSubscriberInfo(si *SubscriberInfo) ([]byte, error) {
	var fields [][]byte
	add := func(tag int, compound bool, content []byte) {
		fields = append(fields, tlv(asn1.ClassContextSpecific, tag, compound, content))
	}
	if si.LocationInformation != nil {
		b, err := encodeLocationInformation(si.LocationInformation, false)
		if err != nil {
			return nil, err
		}
		add(0, true, b)
	}
	if si.SubscriberState != nil {
		b, err := encodeSubscriberState(si.SubscriberState, false)
		if err != nil {
			return nil, err
		}
		add(1, true, b)
	}
	if si.LocationInformationGPRS != nil {
		b, err := encodeLocationInformation(si.LocationInformationGPRS, true)
		if err != nil {
			return nil, err
		}
		add(3, true, b)
	}
	if si.PSSubscriberState != nil {
		b, err := encodeSubscriberState(si.PSSubscriberState, true)
		if err != nil {
			return nil, err
		}
		add(4, true, b)
	}
	if si.IMEI != "" {
		b, err := encodeTBCD(si.IMEI)
		if err != nil || len(b) != 8 {
			return nil, fmt.Errorf("imei %q: not 15 or 16 digits", si.IMEI)
		}
		add(5, false, b)
	}
	if si.MSClassmark2 != nil {
		add(6, false, si.MSClassmark2)
	}
	if m := si.MNPInfoRes; m != nil {
		b, err := encodeMNPInfoRes(m)
		if err != nil {
			return nil, err
		}
		add(8, true, b)
	}
	if si.EPSSubscriberState != nil {
		b, err := encodeSubscriberState(si.EPSSubscriberState, true)
		if err != nil {
			return nil, err
		}
		add(12, true, b)
	}
	if si.LocationInformationEPS != nil {
		add(13, true, encodeLocationInformationEPS(si.LocationInformationEPS))
	}
	return sequence(fields...), nil
}

func encodeLocationInformation(li *LocationInformation, gprs bool) ([]byte, error) {
	var out []byte
	add := func(tag int, compound bool, content []byte) {
		out = append(out, tlv(asn1.ClassContextSpecific, tag, compound, content)...)
	}
	addr := func(tag int, a *AddressString) error {
		if a == nil {
			return nil
		}
		b, err := encodeAddressString(a)
		if err != nil {
			return err
		}
		add(tag, false, b)
		return nil
	}
	cell, isLAI, err := encodeCell(li)
	if err != nil {
		return nil, err
	}
	sai := li.ServiceAreaID != nil
	if !gprs {
		if li.AgeOfLocationInformation != nil {
			out = append(out, tlv(asn1.ClassUniversal, asn1.TagInteger, false, intContent(*li.AgeOfLocationInformation))...)
		}
		if li.GeographicalInformation != nil {
			add(0, false, li.GeographicalInformation)
		}
		if err := addr(1, li.VLRNumber); err != nil {
			return nil, err
		}
		if li.LocationNumber != nil {
			add(2, false, li.LocationNumber)
		}
		if cell != nil {
			add(3, true, cellChoice(cell, isLAI))
		}
		if err := addr(6, li.MSCNumber); err != nil {
			return nil, err
		}
		if li.GeodeticInformation != nil {
			add(7, false, li.GeodeticInformation)
		}
		if li.CurrentLocationRetrieved {
			add(8, false, nil)
		}
		if sai {
			add(9, false, nil)
		}
		if li.EPS != nil {
			add(10, true, encodeLocationInformationEPS(li.EPS))
		}
		return out, nil
	}
	if cell != nil {
		add(0, true, cellChoice(cell, isLAI))
	}
	if li.RouteingAreaIdentity != nil {
		add(1, false, li.RouteingAreaIdentity)
	}
	if li.GeographicalInformation != nil {
		add(2, false, li.GeographicalInformation)
	}
	if err := addr(3, li.SGSNNumber); err != nil {
		return nil, err
	}
	if sai {
		add(6, false, nil)
	}
	if li.GeodeticInformation != nil {
		add(7, false, li.GeodeticInformation)
	}
	if li.CurrentLocationRetrieved {
		add(8, false, nil)
	}
	if li.AgeOfLocationInformation != nil {
		add(9, false, intContent(*li.AgeOfLocationInformation))
	}
	return out, nil
}

func cellChoice(cell []byte, isLAI bool) []byte {
	tag := 0
	if isLAI {
		tag = 1
	}
	return tlv(asn1.ClassContextSpecific, tag, false, cell)
}

// encodeCell encodes whichever of CellGlobalID, ServiceAreaID and LAI is set.
func encodeCell(li *LocationInformation) (cell []byte, isLAI bool, err error) {
	var c *CellGlobalID
	switch {
	case li.CellGlobalID != nil:
		c = li.CellGlobalID
	case li.ServiceAreaID != nil:
		s := li.ServiceAreaID
		c = &CellGlobalID{MCC: s.MCC, MNC: s.MNC, LAC: s.LAC, CI: s.SAC}
	case li.LAI != nil:
		plmn, err := encodePLMN(li.LAI.MCC, li.LAI.MNC)
		if err != nil {
			return nil, false, err
		}
		return append(plmn, byte(li.LAI.LAC>>8), byte(li.LAI.LAC)), true, nil
	default:
		return nil, false, nil
	}
	plmn, err := encodePLMN(c.MCC, c.MNC)
	if err != nil {
		return nil, false, err
	}
	return append(plmn, byte(c.LAC>>8), byte(c.LAC), byte(c.CI>>8), byte(c.CI)), false, nil
}

func encodePLMN(mcc, mnc string) ([]byte, error) {
	if len(mcc) != 3 || len(mnc) < 2 || len(mnc) > 3 {
		return nil, fmt.Errorf("PLMN %s/%s: want 3 MCC and 2 or 3 MNC digits", mcc, mnc)
	}
	d := func(c byte) byte { return c - '0' }
	for _, c := range []byte(mcc + mnc) {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("PLMN %s/%s: not digits", mcc, mnc)
		}
	}
	mnc3 := byte(0x0F)
	if len(mnc) == 3 {
		mnc3 = d(mnc[2])
	}
	return []byte{d(mcc[1])<<4 | d(mcc[0]), mnc3<<4 | d(mcc[2]), d(mnc[1])<<4 | d(mnc[0])}, nil
}

func encodeLocationInformationEPS(e *LocationInformationEPS) []byte {
	var out []byte
	add := func(tag int, content []byte) {
		out = append(out, tlv(asn1.ClassContextSpecific, tag, false, content)...)
	}
	if e.EUTRANCGI != nil {
		add(0, e.EUTRANCGI)
	}
	if e.TrackingAreaIdentity != nil {
		add(1, e.TrackingAreaIdentity)
	}
	if e.GeographicalInformation != nil {
		add(3, e.GeographicalInformation)
	}
	if e.GeodeticInformation != nil {
		add(4, e.GeodeticInformation)
	}
	if e.CurrentLocationRetrieved {
		add(5, nil)
	}
	if e.AgeOfLocationInformation != nil {
		add(6, intContent(*e.AgeOfLocationInformation))
	}
	if e.MMEName != "" {
		add(7, []byte(e.MMEName))
	}
	return out
}

// encodeSubscriberState encodes the CHOICE alternative named by s.State.
func encodeSubscriberState(s *SubscriberState, ps bool) ([]byte, error) {
	if s.State == netDetNotReachable {
		for v, name := range notReachableReasonNames {
			if name == s.NotReachableReason {
				return tlv(asn1.ClassUniversal, asn1.TagEnum, false, intContent(v)), nil
			}
		}
		return nil, fmt.Errorf("notReachableReason %q", s.NotReachableReason)
	}
	names := subscriberStateNames
	if ps {
		names = psSubscriberStateNames
	}
	for tag, name := range names {
		if name != s.State {
			continue
		}
		if tag >= 4 {
			return nil, fmt.Errorf("subscriber state %q carries a PDP context list, not encoded", s.State)
		}
		return tlv(asn1.ClassContextSpecific, tag, false, nil), nil
	}
	return nil, fmt.Errorf("subscriber state %q", s.State)
}

func encodeMNPInfoRes(m *MNPInfoRes) ([]byte, error) {
	var out []byte
	add := func(tag int, content []byte) {
		out = append(out, tlv(asn1.ClassContextSpecific, tag, false, content)...)
	}
	if m.RouteingNumber != "" {
		b, err := encodeTBCD(m.RouteingNumber)
		if err != nil {
			return nil, err
		}
		add(0, b)
	}
	if m.IMSI != "" {
		b, err := encodeIMSI(m.IMSI)
		if err != nil {
			return nil, err
		}
		add(1, b)
	}
	if m.MSISDN != nil {
		b, err := encodeAddressString(m.MSISDN)
		if err != nil {
			return nil, err
		}
		add(2, b)
	}
	if m.NumberPortabilityStatus != "" {
		found := false
		for v, name := range numberPortabilityStatusNames {
			if name == m.NumberPortabilityStatus {
				add(3, intContent(v))
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("numberPortabilityStatus %q", m.NumberPortabilityStatus)
		}
	}
	return out, nil
}

// intContent is the minimal two's-complement content of a non-negative int.
func intContent(v int) []byte {
	b, _ := asn1.Marshal(v)
	return b[2:] // tag and one-octet length: values here are < 2^31
}
