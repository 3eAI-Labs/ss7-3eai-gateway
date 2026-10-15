// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// The VLR side of updateLocation, insertSubscriberData, cancelLocation and
// provideRoamingNumber: what a test peer playing the VLR encodes and
// decodes. The decoders are strict — fields in declaration order, the
// mandatory ones present — so that the peer reports what a real VLR would
// refuse.

// EncodeUpdateLocationArg encodes UpdateLocationArg with the fields of
// UpdateLocationArg the gateway reads.
func EncodeUpdateLocationArg(a *UpdateLocationArg) ([]byte, error) {
	if a == nil {
		return nil, errors.New("UpdateLocationArg: nil")
	}
	imsi, err := encodeIMSI(a.IMSI)
	if err != nil {
		return nil, err
	}
	msc, err := encodeISDNAddressString(&a.MSCNumber)
	if err != nil {
		return nil, fmt.Errorf("msc-Number: %w", err)
	}
	vlr, err := encodeISDNAddressString(&a.VLRNumber)
	if err != nil {
		return nil, fmt.Errorf("vlr-Number: %w", err)
	}
	fields := [][]byte{
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, imsi),
		tlv(asn1.ClassContextSpecific, 1, false, msc),
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, vlr),
	}
	if a.LMSI != nil {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 10, false, a.LMSI))
	}
	null := func(tag int) []byte { return tlv(asn1.ClassContextSpecific, tag, false, nil) }
	if c := a.VLRCapability; c != nil {
		var content []byte
		if c.SupportedCAMELPhases != nil {
			bits, err := encodeNamedBits(c.SupportedCAMELPhases, 1, 16)
			if err != nil {
				return nil, err
			}
			content = append(content, tlv(asn1.ClassContextSpecific, 0, false, bits)...)
		}
		if c.SOLSASupport {
			content = append(content, null(2)...)
		}
		if c.ISTSupport != nil {
			content = append(content, tlv(asn1.ClassContextSpecific, 1, false, []byte{byte(*c.ISTSupport)})...)
		}
		if s := c.SuperCharger; s != nil {
			alt := null(0)
			if !s.SendSubscriberData {
				alt = tlv(asn1.ClassContextSpecific, 1, false, s.SubscriberDataAge)
			}
			content = append(content, tlv(asn1.ClassContextSpecific, 3, true, alt)...)
		}
		if c.LongFTNSupported {
			content = append(content, null(4)...)
		}
		for _, b := range []struct {
			tag  int
			bits []int
			min  int
		}{{5, c.SupportedLCSCapabilitySets, 2}, {6, c.OfferedCAMEL4CSIs, 7}, {7, c.SupportedRATTypes, 2}} {
			if b.bits != nil {
				enc, err := encodeNamedBits(b.bits, b.min, 16)
				if err != nil {
					return nil, err
				}
				content = append(content, tlv(asn1.ClassContextSpecific, b.tag, false, enc)...)
			}
		}
		if c.LongGroupIDSupported {
			content = append(content, null(8)...)
		}
		if c.MTRoamingForwardingSupported {
			content = append(content, null(9)...)
		}
		if c.MSISDNLessOperationSupported {
			content = append(content, null(10)...)
		}
		if c.ResetIDsSupported {
			content = append(content, null(11)...)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 6, true, content))
	}
	if a.InformPreviousNetworkEntity {
		fields = append(fields, null(11))
	}
	if a.CSLCSNotSupportedByUE {
		fields = append(fields, null(12))
	}
	if a.IMEISV != "" {
		imei, err := encodeTBCD(a.IMEISV)
		if err != nil || len(imei) != 8 {
			return nil, fmt.Errorf("imeisv %q is not 16 digits", a.IMEISV)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 13, true, tlv(asn1.ClassContextSpecific, 0, false, imei)))
	}
	if a.SkipSubscriberDataUpdate {
		fields = append(fields, null(15))
	}
	if a.RestorationIndicator {
		fields = append(fields, null(16))
	}
	return sequence(fields...), nil
}

// DecodeUpdateLocationRes decodes UpdateLocationRes.
func DecodeUpdateLocationRes(b []byte) (*UpdateLocationRes, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationRes: %w", err)
	}
	if len(fields) == 0 || !isUniversal(fields[0], asn1.TagOctetString, false) {
		return nil, errors.New("UpdateLocationRes: hlr-Number missing")
	}
	hlr, err := decodeISDNAddressString("hlr-Number", fields[0].Bytes)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationRes: %w", err)
	}
	return &UpdateLocationRes{HLRNumber: *hlr}, nil
}

// DecodeCancelLocationArg decodes CancelLocationArg, [3] SEQUENCE
// { identity Identity, cancellationType OPTIONAL, … }.
func DecodeCancelLocationArg(b []byte) (*CancelLocationArg, error) {
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(b, &outer)
	if err != nil {
		return nil, fmt.Errorf("CancelLocationArg: %w", err)
	}
	if len(rest) > 0 || !isContext(outer, 3, true) {
		return nil, errors.New("CancelLocationArg: not a single [3] SEQUENCE")
	}
	fields, err := berChildren(outer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CancelLocationArg: %w", err)
	}
	if len(fields) == 0 {
		return nil, errors.New("CancelLocationArg: identity missing")
	}
	arg := &CancelLocationArg{}
	switch id := fields[0]; {
	case isUniversal(id, asn1.TagOctetString, false):
		if arg.IMSI, err = decodeIMSI(id.Bytes); err != nil {
			return nil, fmt.Errorf("CancelLocationArg: %w", err)
		}
	case isUniversal(id, asn1.TagSequence, true):
		parts, err := berChildren(id.Bytes)
		if err != nil || len(parts) < 2 || !isUniversal(parts[0], asn1.TagOctetString, false) ||
			!isUniversal(parts[1], asn1.TagOctetString, false) {
			return nil, errors.New("CancelLocationArg: imsi-WithLMSI is not { imsi, lmsi }")
		}
		if arg.IMSI, err = decodeIMSI(parts[0].Bytes); err != nil {
			return nil, fmt.Errorf("CancelLocationArg: %w", err)
		}
		if arg.LMSI, err = fixedOctets("lmsi", parts[1].Bytes, lmsiLength, lmsiLength); err != nil {
			return nil, fmt.Errorf("CancelLocationArg: %w", err)
		}
	default:
		return nil, errors.New("CancelLocationArg: identity is neither imsi nor imsi-WithLMSI")
	}
	for _, f := range fields[1:] {
		if isUniversal(f, asn1.TagEnum, false) {
			v, err := berInt(f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("CancelLocationArg: cancellationType: %w", err)
			}
			t := CancellationType(v)
			arg.CancellationType = &t
		}
	}
	return arg, nil
}

// EncodeCancelLocationRes encodes an empty CancelLocationRes.
func EncodeCancelLocationRes() []byte { return sequence() }

// DecodeProvideRoamingNumberArg decodes ProvideRoamingNumberArg, the fields
// ProvideRoamingNumberArg carries; imsi [0] and msc-Number [1] are required.
func DecodeProvideRoamingNumberArg(b []byte) (*ProvideRoamingNumberArg, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
	}
	if len(fields) < 2 || !isContext(fields[0], 0, false) || !isContext(fields[1], 1, false) {
		return nil, errors.New("ProvideRoamingNumberArg: imsi [0] and msc-Number [1] required, first")
	}
	arg := &ProvideRoamingNumberArg{}
	if arg.IMSI, err = decodeIMSI(fields[0].Bytes); err != nil {
		return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
	}
	msc, err := decodeISDNAddressString("msc-Number", fields[1].Bytes)
	if err != nil {
		return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
	}
	arg.MSCNumber = *msc
	last := 1
	for _, f := range fields[2:] {
		if f.Class != asn1.ClassContextSpecific {
			return nil, errors.New("ProvideRoamingNumberArg: untagged field")
		}
		// Up to the extension marker the fields are declared in tag order.
		if f.Tag <= 11 {
			if f.Tag <= last {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: [%d] after [%d]", f.Tag, last)
			}
			last = f.Tag
		}
		switch {
		case isContext(f, 2, false):
			if arg.MSISDN, err = decodeISDNAddressString("msisdn", f.Bytes); err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
			}
		case isContext(f, 4, false):
			if arg.LMSI, err = fixedOctets("lmsi", f.Bytes, lmsiLength, lmsiLength); err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
			}
		case isContext(f, 5, true):
			parts, err := berChildren(f.Bytes)
			if err != nil || len(parts) < 2 || !isUniversal(parts[0], asn1.TagEnum, false) ||
				!isUniversal(parts[1], asn1.TagOctetString, false) {
				return nil, errors.New("ProvideRoamingNumberArg: gsm-BearerCapability is not { protocolId, signalInfo }")
			}
			id, err := berInt(parts[0].Bytes)
			if err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: protocolId: %w", err)
			}
			arg.GSMBearerCapability = &ExternalSignalInfo{ProtocolID: id, SignalInfo: append([]byte(nil), parts[1].Bytes...)}
		case isContext(f, 7, false):
			arg.SuppressionOfAnnouncement = true
		case isContext(f, 8, false):
			if arg.GMSCAddress, err = decodeISDNAddressString("gmsc-Address", f.Bytes); err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
			}
		case isContext(f, 9, false):
			if arg.CallReferenceNumber, err = fixedOctets("callReferenceNumber", f.Bytes, 1, maxCallReferenceLength); err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberArg: %w", err)
			}
		case isContext(f, 10, false):
			arg.ORInterrogation = true
		}
	}
	return arg, nil
}

// EncodeProvideRoamingNumberRes encodes ProvideRoamingNumberRes.
func EncodeProvideRoamingNumberRes(r *ProvideRoamingNumberRes) ([]byte, error) {
	rn, err := encodeISDNAddressString(&r.RoamingNumber)
	if err != nil {
		return nil, fmt.Errorf("roamingNumber: %w", err)
	}
	fields := [][]byte{tlv(asn1.ClassUniversal, asn1.TagOctetString, false, rn)}
	if r.ReleaseResourcesSupported {
		fields = append(fields, tlv(asn1.ClassUniversal, asn1.TagNull, false, nil))
	}
	if r.VMSCAddress != nil {
		v, err := encodeISDNAddressString(r.VMSCAddress)
		if err != nil {
			return nil, fmt.Errorf("vmsc-Address: %w", err)
		}
		fields = append(fields, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, v))
	}
	return sequence(fields...), nil
}

// insertSubscriberDataOrder is the declaration position of the
// InsertSubscriberDataArg fields a decoder may meet, by tag: SubscriberData
// [1]..[13] in tag order after imsi [0], then extensionContainer [14] and
// the extension fields in their (non-tag) order.
var insertSubscriberDataOrder = func() map[int]int {
	order := map[int]int{}
	for i, tag := range []int{0, 1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 23, 24, 25, 21, 22,
		26, 27, 28, 29, 17, 18, 19, 20, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46,
		47, 48, 49, 50, 51, 52, 53, 54} {
		order[tag] = i
	}
	return order
}()

// DecodeInsertSubscriberDataArg decodes InsertSubscriberDataArg, the
// fields SubscriberData carries. Fields must come in declaration order.
func DecodeInsertSubscriberDataArg(b []byte) (*InsertSubscriberDataArg, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("InsertSubscriberDataArg: %w", err)
	}
	arg := &InsertSubscriberDataArg{}
	d := &arg.Data
	last := -1
	wrap := func(err error) error { return fmt.Errorf("InsertSubscriberDataArg: %w", err) }
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			return nil, wrap(errors.New("untagged field"))
		}
		pos, known := insertSubscriberDataOrder[f.Tag]
		if !known {
			continue // a later extension
		}
		if pos <= last {
			return nil, wrap(fmt.Errorf("[%d] out of declaration order", f.Tag))
		}
		last = pos
		switch {
		case isContext(f, 0, false):
			arg.IMSI, err = decodeIMSI(f.Bytes)
		case isContext(f, 1, false):
			d.MSISDN, err = decodeISDNAddressString("msisdn", f.Bytes)
		case isContext(f, 2, false):
			d.Category, err = fixedOctets("category", f.Bytes, 1, 1)
		case isContext(f, 3, false):
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				s := SubscriberStatus(v)
				d.SubscriberStatus = &s
			}
		case isContext(f, 4, true):
			d.BearerServices, err = decodeServiceCodeList("bearerServiceList", f.Bytes, maxNumOfBearerServices)
		case isContext(f, 6, true):
			d.Teleservices, err = decodeServiceCodeList("teleserviceList", f.Bytes, maxNumOfTeleservices)
		case isContext(f, 7, true):
			d.ProvisionedSS, err = decodeSSInfoList(f.Bytes)
		case isContext(f, 8, true):
			d.ODB, err = decodeODBData(f.Bytes)
		case isContext(f, 9, false):
			d.RoamingRestrictionDueToUnsupportedFeature = true
		case isContext(f, 10, true):
			var zones []asn1.RawValue
			if zones, err = berChildren(f.Bytes); err == nil {
				for _, z := range zones {
					if !isUniversal(z, asn1.TagOctetString, false) || len(z.Bytes) != zoneCodeLength {
						err = errors.New("regionalSubscriptionData: zone code is not a 2-octet OCTET STRING")
						break
					}
					d.RegionalSubscription = append(d.RegionalSubscription, append([]byte(nil), z.Bytes...))
				}
			}
		case isContext(f, 24, false):
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				m := NetworkAccessMode(v)
				d.NetworkAccessMode = &m
			}
		case isContext(f, 19, false):
			if d.AccessRestrictionData, err = decodeNamedBits(f.Bytes); err == nil && d.AccessRestrictionData == nil {
				d.AccessRestrictionData = NamedBits{}
			}
		}
		if err != nil {
			return nil, wrap(err)
		}
	}
	return arg, nil
}

func decodeSSInfoList(b []byte) ([]SSInfo, error) {
	items, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("provisionedSS: %w", err)
	}
	if len(items) < 1 || len(items) > maxNumOfSS {
		return nil, fmt.Errorf("provisionedSS: %d entries, want 1..%d", len(items), maxNumOfSS)
	}
	out := make([]SSInfo, 0, len(items))
	for _, it := range items {
		if it.Class != asn1.ClassContextSpecific || !it.IsCompound {
			return nil, errors.New("provisionedSS: Ext-SS-Info alternative is not a constructed context tag")
		}
		parts, err := berChildren(it.Bytes)
		if err != nil || len(parts) < 2 || !isUniversal(parts[0], asn1.TagOctetString, false) || len(parts[0].Bytes) != 1 {
			return nil, fmt.Errorf("provisionedSS [%d]: ss-Code missing", it.Tag)
		}
		code := parts[0].Bytes[0]
		switch it.Tag {
		case 0:
			if !isUniversal(parts[1], asn1.TagSequence, true) {
				return nil, errors.New("forwardingInfo: forwardingFeatureList missing")
			}
			feats, err := berChildren(parts[1].Bytes)
			if err != nil || len(feats) == 0 {
				return nil, errors.New("forwardingInfo: empty forwardingFeatureList")
			}
			fi := &ForwardingInfo{SSCode: code}
			for _, ft := range feats {
				f, err := decodeForwardingFeature(ft)
				if err != nil {
					return nil, err
				}
				fi.Features = append(fi.Features, *f)
			}
			out = append(out, SSInfo{Forwarding: fi})
		case 1:
			if !isUniversal(parts[1], asn1.TagSequence, true) {
				return nil, errors.New("callBarringInfo: callBarringFeatureList missing")
			}
			feats, err := berChildren(parts[1].Bytes)
			if err != nil || len(feats) == 0 {
				return nil, errors.New("callBarringInfo: empty callBarringFeatureList")
			}
			cb := &CallBarringInfo{SSCode: code}
			for _, ft := range feats {
				elems, err := berChildren(ft.Bytes)
				if err != nil || !isUniversal(ft, asn1.TagSequence, true) {
					return nil, errors.New("callBarringInfo: feature is not a SEQUENCE")
				}
				var cf CallBarringFeature
				var haveStatus bool
				for _, e := range elems {
					switch {
					case isContext(e, 2, false) || isContext(e, 3, false):
						cf.BasicService = &BasicServiceCode{Bearer: e.Tag == 2, Code: append([]byte(nil), e.Bytes...)}
					case isContext(e, 4, false) && len(e.Bytes) >= 1:
						cf.SSStatus, haveStatus = e.Bytes[0], true
					}
				}
				if !haveStatus {
					return nil, errors.New("callBarringInfo: ss-Status missing")
				}
				cb.Features = append(cb.Features, cf)
			}
			out = append(out, SSInfo{CallBarring: cb})
		case 3:
			sd := &SSData{SSCode: code}
			if !isContext(parts[1], 4, false) || len(parts[1].Bytes) < 1 {
				return nil, errors.New("ss-Data: ss-Status missing")
			}
			sd.SSStatus = parts[1].Bytes[0]
			for _, e := range parts[2:] {
				switch {
				case isContext(e, 2, false):
					v, err := berInt(e.Bytes)
					if err != nil {
						return nil, err
					}
					sd.CLIRestrictionOption = &v
				case isContext(e, 1, false):
					v, err := berInt(e.Bytes)
					if err != nil {
						return nil, err
					}
					sd.OverrideCategory = &v
				case isUniversal(e, asn1.TagSequence, true):
					alts, err := berChildren(e.Bytes)
					if err != nil {
						return nil, err
					}
					for _, a := range alts {
						if !isContext(a, 2, false) && !isContext(a, 3, false) {
							return nil, errors.New("ss-Data: basicServiceGroupList element is not ext-BearerService or ext-Teleservice")
						}
						sd.BasicServices = append(sd.BasicServices, BasicServiceCode{Bearer: a.Tag == 2, Code: append([]byte(nil), a.Bytes...)})
					}
				}
			}
			out = append(out, SSInfo{SSData: sd})
		default:
			return nil, fmt.Errorf("provisionedSS: Ext-SS-Info alternative [%d] not decoded here", it.Tag)
		}
	}
	return out, nil
}

func decodeForwardingFeature(ft asn1.RawValue) (*ForwardingFeature, error) {
	if !isUniversal(ft, asn1.TagSequence, true) {
		return nil, errors.New("forwardingFeature is not a SEQUENCE")
	}
	elems, err := berChildren(ft.Bytes)
	if err != nil {
		return nil, err
	}
	f := &ForwardingFeature{}
	var haveStatus bool
	for _, e := range elems {
		switch {
		case isContext(e, 2, false) || isContext(e, 3, false):
			f.BasicService = &BasicServiceCode{Bearer: e.Tag == 2, Code: append([]byte(nil), e.Bytes...)}
		case isContext(e, 4, false) && len(e.Bytes) >= 1:
			f.SSStatus, haveStatus = e.Bytes[0], true
		case isContext(e, 5, false) || isContext(e, 10, false):
			if f.ForwardedToNumber, err = decodeAddressString(e.Bytes); err != nil {
				return nil, fmt.Errorf("forwardedToNumber: %w", err)
			}
		case isContext(e, 8, false):
			f.ForwardedToSubaddress = append([]byte(nil), e.Bytes...)
		case isContext(e, 6, false):
			f.ForwardingOptions = append([]byte(nil), e.Bytes...)
		case isContext(e, 7, false):
			v, err := berInt(e.Bytes)
			if err != nil {
				return nil, err
			}
			f.NoReplyConditionTime = &v
		}
	}
	if !haveStatus {
		return nil, errors.New("forwardingFeature: ss-Status missing")
	}
	return f, nil
}

// decodeODBData reads ODB-Data: SEQUENCE { odb-GeneralData BIT STRING,
// odb-HPLMN-Data BIT STRING OPTIONAL, extensionContainer OPTIONAL, … }.
func decodeODBData(b []byte) (*ODBData, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("odb-Data: %w", err)
	}
	if len(fields) == 0 || !isUniversal(fields[0], asn1.TagBitString, false) {
		return nil, errors.New("odb-Data: odb-GeneralData missing")
	}
	o := &ODBData{}
	if o.General, err = decodeNamedBits(fields[0].Bytes); err != nil {
		return nil, fmt.Errorf("odb-GeneralData: %w", err)
	}
	if o.General == nil {
		o.General = NamedBits{}
	}
	if len(fields) > 1 && isUniversal(fields[1], asn1.TagBitString, false) {
		if o.HPLMN, err = decodeNamedBits(fields[1].Bytes); err != nil {
			return nil, fmt.Errorf("odb-HPLMN-Data: %w", err)
		}
		if o.HPLMN == nil {
			o.HPLMN = NamedBits{}
		}
	}
	return o, nil
}

// EncodeInsertSubscriberDataRes encodes InsertSubscriberDataRes.
func EncodeInsertSubscriberDataRes(r *InsertSubscriberDataRes) ([]byte, error) {
	var fields [][]byte
	if r.Teleservices != nil {
		l, err := encodeServiceCodeList("teleserviceList", r.Teleservices, maxNumOfTeleservices)
		if err != nil {
			return nil, err
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 1, true, l))
	}
	if r.BearerServices != nil {
		l, err := encodeServiceCodeList("bearerServiceList", r.BearerServices, maxNumOfBearerServices)
		if err != nil {
			return nil, err
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 2, true, l))
	}
	if r.SSCodes != nil {
		var l []byte
		for _, c := range r.SSCodes {
			l = append(l, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{c})...)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 3, true, l))
	}
	if r.ODBGeneralData != nil {
		g, err := encodeNamedBits(r.ODBGeneralData, odbGeneralMinBits, odbGeneralMaxBits)
		if err != nil {
			return nil, err
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 4, false, g))
	}
	if r.RegionalSubscriptionResponse != nil {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 5, false, []byte{byte(*r.RegionalSubscriptionResponse)}))
	}
	if r.SupportedCAMELPhases != nil {
		p, err := encodeNamedBits(r.SupportedCAMELPhases, 1, 16)
		if err != nil {
			return nil, err
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 6, false, p))
	}
	return sequence(fields...), nil
}
