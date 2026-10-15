// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// insertSubscriberData (op 7), HLR → VLR, 3GPP TS 29.002 §8.8.1 and
// MAP-MS-DataTypes: the gateway, as HLR, encodes the argument and decodes
// the result. The inverse functions are in mobility_peer.go.
//
// The subscriber data covered is the circuit-switched profile a VLR keeps:
// the fields of SubscriberData except CUG, eMLPP, VBS/VGCS and CAMEL
// subscription information, plus networkAccessMode and
// accessRestrictionData from the extensions. GPRS, EPS, LCS and the other
// extension fields are not sent.

// SubscriberStatus is TS 29.002 SubscriberStatus.
type SubscriberStatus int

const (
	ServiceGranted            SubscriberStatus = 0
	OperatorDeterminedBarring SubscriberStatus = 1
)

// NetworkAccessMode is TS 29.002 NetworkAccessMode.
type NetworkAccessMode int

const (
	PacketAndCircuit NetworkAccessMode = 0
	OnlyCircuit      NetworkAccessMode = 1
	OnlyPacket       NetworkAccessMode = 2
)

// NamedBits is a BIT STRING given as the numbers of the bits set. nil is an
// absent bit string; an empty, non-nil one is present with no bit set.
type NamedBits []int

// SubscriberData is the subscriber data of an InsertSubscriberDataArg.
type SubscriberData struct {
	MSISDN           *AddressString    // [1]
	Category         []byte            // [2], 1 octet (ITU-T Q.763 calling party's category)
	SubscriberStatus *SubscriberStatus // [3]
	BearerServices   [][]byte          // [4] Ext-BearerServiceCode, 1..5 octets each
	Teleservices     [][]byte          // [6] Ext-TeleserviceCode
	ProvisionedSS    []SSInfo          // [7]
	ODB              *ODBData          // [8]
	// RoamingRestrictionDueToUnsupportedFeature is [9].
	RoamingRestrictionDueToUnsupportedFeature bool
	RegionalSubscription                      [][]byte           // [10] ZoneCode, 2 octets each
	NetworkAccessMode                         *NetworkAccessMode // [24]
	AccessRestrictionData                     NamedBits          // [19]
}

// SSInfo is one Ext-SS-Info, a CHOICE: exactly one field is set.
type SSInfo struct {
	Forwarding  *ForwardingInfo  // [0] Ext-ForwInfo
	CallBarring *CallBarringInfo // [1] Ext-CallBarInfo
	SSData      *SSData          // [3] Ext-SS-Data
}

// ForwardingInfo is Ext-ForwInfo.
type ForwardingInfo struct {
	SSCode   byte
	Features []ForwardingFeature
}

// ForwardingFeature is Ext-ForwFeature.
type ForwardingFeature struct {
	BasicService *BasicServiceCode
	SSStatus     byte // Ext-SS-Status, bits Q P R A
	// ForwardedToNumber goes as forwardedToNumber [5] when it fits an
	// ISDN-AddressString (9 octets), else as longForwardedToNumber [10]
	// (an FTN-AddressString, 15).
	ForwardedToNumber     *AddressString
	ForwardedToSubaddress []byte // [8]
	ForwardingOptions     []byte // [6] Ext-ForwOptions, 1..5 octets
	NoReplyConditionTime  *int   // [7], 5..30 seconds
}

// CallBarringInfo is Ext-CallBarInfo.
type CallBarringInfo struct {
	SSCode   byte
	Features []CallBarringFeature
}

// CallBarringFeature is Ext-CallBarringFeature.
type CallBarringFeature struct {
	BasicService *BasicServiceCode
	SSStatus     byte
}

// SSData is Ext-SS-Data. At most one of CLIRestrictionOption and
// OverrideCategory, the CHOICE SS-SubscriptionOption.
type SSData struct {
	SSCode               byte
	SSStatus             byte
	CLIRestrictionOption *int // [2]: 0 permanent, 1 temporaryDefaultRestricted, 2 temporaryDefaultAllowed
	OverrideCategory     *int // [1]: 0 overrideEnabled, 1 overrideDisabled
	BasicServices        []BasicServiceCode
}

// ODBData is ODB-Data.
type ODBData struct {
	General NamedBits // ODB-GeneralData
	HPLMN   NamedBits // ODB-HPLMN-Data, nil when absent
}

// InsertSubscriberDataArg is InsertSubscriberDataArg as the gateway sends
// it. IMSI is empty within a location update, where the dialogue names the
// subscriber (§8.8.1.3: it is "only included if the service is not used in
// an ongoing transaction").
type InsertSubscriberDataArg struct {
	IMSI IMSI
	Data SubscriberData
}

// InsertSubscriberDataRes is TS 29.002 InsertSubscriberDataRes: what the
// VLR did not take.
type InsertSubscriberDataRes struct {
	Teleservices                 [][]byte  // [1] not supported
	BearerServices               [][]byte  // [2] not supported
	SSCodes                      []byte    // [3] not supported
	ODBGeneralData               NamedBits // [4] barring not supported
	RegionalSubscriptionResponse *int      // [5]
	SupportedCAMELPhases         NamedBits // [6]
	OfferedCAMEL4CSIs            NamedBits // [8]
	SupportedFeatures            NamedBits // [9]
}

// InsertSubscriberDataErrors are the errors insertSubscriberData allows.
var InsertSubscriberDataErrors = map[ErrorCode]bool{
	ErrDataMissing: true, ErrUnexpectedDataValue: true, ErrUnidentifiedSubscriber: true,
}

// Size limits of the types above (TS 29.002 MAP-MS-DataTypes).
const (
	maxNumOfBearerServices        = 50
	maxNumOfTeleservices          = 20
	maxNumOfExtBasicServiceGroups = 32
	maxNumOfZoneCodes             = 10
	zoneCodeLength                = 2
	maxSSStatusLength             = 5
	maxForwOptionsLength          = 5
	minNoReplyConditionTime       = 5
	maxNoReplyConditionTime       = 30
	odbGeneralMinBits             = 15
	odbGeneralMaxBits             = 32
	odbHPLMNMinBits               = 4
	odbHPLMNMaxBits               = 32
	accessRestrictionMinBits      = 2
	accessRestrictionMaxBits      = 8
	maxCLIRestrictionOption       = 2
	maxOverrideCategory           = 1
	maxRegionalSubscriptionResult = 3
)

// ----- encoder (gateway → VLR) -----------------------------------------------

// EncodeInsertSubscriberDataArg encodes InsertSubscriberDataArg:
//
//	SEQUENCE { imsi [0] IMSI OPTIONAL, COMPONENTS OF SubscriberData,
//	           extensionContainer [14] OPTIONAL, …,
//	           accessRestrictionData [19] OPTIONAL,
//	           networkAccessMode [24] OPTIONAL, … }
//
// SubscriberData's fields are [1]..[13]; the extension fields follow in
// the order the ASN.1 declares them, which is not tag order:
// networkAccessMode [24] before accessRestrictionData [19].
func EncodeInsertSubscriberDataArg(a *InsertSubscriberDataArg) ([]byte, error) {
	if a == nil {
		return nil, errors.New("InsertSubscriberDataArg: nil")
	}
	var fields [][]byte
	if a.IMSI != "" {
		imsi, err := encodeIMSI(a.IMSI)
		if err != nil {
			return nil, fmt.Errorf("InsertSubscriberDataArg: %w", err)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 0, false, imsi))
	}
	data, err := encodeSubscriberData(&a.Data)
	if err != nil {
		return nil, fmt.Errorf("InsertSubscriberDataArg: %w", err)
	}
	return sequence(append(fields, data...)...), nil
}

func encodeSubscriberData(d *SubscriberData) ([][]byte, error) {
	var out [][]byte
	ctx := func(tag int, compound bool, content []byte) {
		out = append(out, tlv(asn1.ClassContextSpecific, tag, compound, content))
	}
	if d.MSISDN != nil {
		m, err := encodeISDNAddressString(d.MSISDN)
		if err != nil {
			return nil, fmt.Errorf("msisdn: %w", err)
		}
		ctx(1, false, m)
	}
	if d.Category != nil {
		if len(d.Category) != 1 {
			return nil, fmt.Errorf("category of %d octets, want 1", len(d.Category))
		}
		ctx(2, false, d.Category)
	}
	if s := d.SubscriberStatus; s != nil {
		if *s != ServiceGranted && *s != OperatorDeterminedBarring {
			return nil, fmt.Errorf("subscriberStatus %d", *s)
		}
		ctx(3, false, []byte{byte(*s)})
	}
	if d.BearerServices != nil {
		l, err := encodeServiceCodeList("bearerServiceList", d.BearerServices, maxNumOfBearerServices)
		if err != nil {
			return nil, err
		}
		ctx(4, true, l)
	}
	if d.Teleservices != nil {
		l, err := encodeServiceCodeList("teleserviceList", d.Teleservices, maxNumOfTeleservices)
		if err != nil {
			return nil, err
		}
		ctx(6, true, l)
	}
	if d.ProvisionedSS != nil {
		if len(d.ProvisionedSS) < 1 || len(d.ProvisionedSS) > maxNumOfSS {
			return nil, fmt.Errorf("provisionedSS: %d entries, want 1..%d", len(d.ProvisionedSS), maxNumOfSS)
		}
		var l []byte
		for i := range d.ProvisionedSS {
			e, err := encodeSSInfo(&d.ProvisionedSS[i])
			if err != nil {
				return nil, fmt.Errorf("provisionedSS[%d]: %w", i, err)
			}
			l = append(l, e...)
		}
		ctx(7, true, l)
	}
	if o := d.ODB; o != nil {
		g, err := encodeNamedBits(o.General, odbGeneralMinBits, odbGeneralMaxBits)
		if err != nil {
			return nil, fmt.Errorf("odb-GeneralData: %w", err)
		}
		content := tlv(asn1.ClassUniversal, asn1.TagBitString, false, g)
		if o.HPLMN != nil {
			h, err := encodeNamedBits(o.HPLMN, odbHPLMNMinBits, odbHPLMNMaxBits)
			if err != nil {
				return nil, fmt.Errorf("odb-HPLMN-Data: %w", err)
			}
			content = append(content, tlv(asn1.ClassUniversal, asn1.TagBitString, false, h)...)
		}
		ctx(8, true, content)
	}
	if d.RoamingRestrictionDueToUnsupportedFeature {
		ctx(9, false, nil)
	}
	if d.RegionalSubscription != nil {
		if len(d.RegionalSubscription) < 1 || len(d.RegionalSubscription) > maxNumOfZoneCodes {
			return nil, fmt.Errorf("regionalSubscriptionData: %d zone codes, want 1..%d", len(d.RegionalSubscription), maxNumOfZoneCodes)
		}
		var l []byte
		for _, z := range d.RegionalSubscription {
			if len(z) != zoneCodeLength {
				return nil, fmt.Errorf("regionalSubscriptionData: zone code of %d octets, want %d", len(z), zoneCodeLength)
			}
			l = append(l, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, z)...)
		}
		ctx(10, true, l)
	}
	// The extension fields, in the order InsertSubscriberDataArg declares
	// them: networkAccessMode [24] comes before accessRestrictionData [19].
	if m := d.NetworkAccessMode; m != nil {
		if *m < PacketAndCircuit || *m > OnlyPacket {
			return nil, fmt.Errorf("networkAccessMode %d", *m)
		}
		ctx(24, false, []byte{byte(*m)})
	}
	if d.AccessRestrictionData != nil {
		a, err := encodeNamedBits(d.AccessRestrictionData, accessRestrictionMinBits, accessRestrictionMaxBits)
		if err != nil {
			return nil, fmt.Errorf("accessRestrictionData: %w", err)
		}
		ctx(19, false, a)
	}
	return out, nil
}

func encodeServiceCodeList(name string, codes [][]byte, max int) ([]byte, error) {
	if len(codes) < 1 || len(codes) > max {
		return nil, fmt.Errorf("%s: %d codes, want 1..%d", name, len(codes), max)
	}
	var l []byte
	for _, c := range codes {
		if len(c) < 1 || len(c) > maxExtBasicServiceCodeSize {
			return nil, fmt.Errorf("%s: code of %d octets, want 1..%d", name, len(c), maxExtBasicServiceCodeSize)
		}
		l = append(l, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, c)...)
	}
	return l, nil
}

// encodeSSInfo encodes one Ext-SS-Info. Its alternatives are SEQUENCEs, so
// under IMPLICIT TAGS the alternative's tag replaces the SEQUENCE tag.
func encodeSSInfo(s *SSInfo) ([]byte, error) {
	set := 0
	for _, p := range []bool{s.Forwarding != nil, s.CallBarring != nil, s.SSData != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("Ext-SS-Info: %d alternatives set, want 1", set)
	}
	ssCode := func(c byte) []byte { return tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{c}) }
	switch {
	case s.Forwarding != nil:
		f := s.Forwarding
		if len(f.Features) < 1 || len(f.Features) > maxNumOfExtBasicServiceGroups {
			return nil, fmt.Errorf("forwardingFeatureList: %d features, want 1..%d", len(f.Features), maxNumOfExtBasicServiceGroups)
		}
		var list []byte
		for i := range f.Features {
			e, err := encodeForwardingFeature(&f.Features[i])
			if err != nil {
				return nil, fmt.Errorf("forwardingFeatureList[%d]: %w", i, err)
			}
			list = append(list, e...)
		}
		return tlv(asn1.ClassContextSpecific, 0, true, concatBytes(ssCode(f.SSCode),
			tlv(asn1.ClassUniversal, asn1.TagSequence, true, list))), nil

	case s.CallBarring != nil:
		c := s.CallBarring
		if len(c.Features) < 1 || len(c.Features) > maxNumOfExtBasicServiceGroups {
			return nil, fmt.Errorf("callBarringFeatureList: %d features, want 1..%d", len(c.Features), maxNumOfExtBasicServiceGroups)
		}
		var list []byte
		for _, f := range c.Features {
			var content []byte
			if f.BasicService != nil {
				bs, err := encodeExtBasicServiceAlt(f.BasicService)
				if err != nil {
					return nil, err
				}
				content = bs
			}
			content = append(content, encodeSSStatus(f.SSStatus)...)
			list = append(list, tlv(asn1.ClassUniversal, asn1.TagSequence, true, content)...)
		}
		return tlv(asn1.ClassContextSpecific, 1, true, concatBytes(ssCode(c.SSCode),
			tlv(asn1.ClassUniversal, asn1.TagSequence, true, list))), nil
	}

	d := s.SSData
	content := concatBytes(ssCode(d.SSCode), encodeSSStatus(d.SSStatus))
	switch {
	case d.CLIRestrictionOption != nil && d.OverrideCategory != nil:
		return nil, errors.New("Ext-SS-Data: both cliRestrictionOption and overrideCategory (a CHOICE)")
	case d.CLIRestrictionOption != nil:
		if v := *d.CLIRestrictionOption; v < 0 || v > maxCLIRestrictionOption {
			return nil, fmt.Errorf("Ext-SS-Data: cliRestrictionOption %d", v)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 2, false, []byte{byte(*d.CLIRestrictionOption)})...)
	case d.OverrideCategory != nil:
		if v := *d.OverrideCategory; v < 0 || v > maxOverrideCategory {
			return nil, fmt.Errorf("Ext-SS-Data: overrideCategory %d", v)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 1, false, []byte{byte(*d.OverrideCategory)})...)
	}
	if d.BasicServices != nil {
		if len(d.BasicServices) < 1 || len(d.BasicServices) > maxNumOfExtBasicServiceGroups {
			return nil, fmt.Errorf("Ext-SS-Data: basicServiceGroupList of %d, want 1..%d", len(d.BasicServices), maxNumOfExtBasicServiceGroups)
		}
		var list []byte
		for i := range d.BasicServices {
			bs, err := encodeExtBasicServiceAlt(&d.BasicServices[i])
			if err != nil {
				return nil, err
			}
			list = append(list, bs...)
		}
		content = append(content, tlv(asn1.ClassUniversal, asn1.TagSequence, true, list)...)
	}
	return tlv(asn1.ClassContextSpecific, 3, true, content), nil
}

// encodeForwardingFeature encodes Ext-ForwFeature:
//
//	SEQUENCE { basicService Ext-BasicServiceCode OPTIONAL,
//	           ss-Status [4], forwardedToNumber [5] OPTIONAL,
//	           forwardedToSubaddress [8] OPTIONAL,
//	           forwardingOptions [6] OPTIONAL, noReplyConditionTime [7]
//	           OPTIONAL, extensionContainer [9] OPTIONAL, …,
//	           longForwardedToNumber [10] OPTIONAL }
func encodeForwardingFeature(f *ForwardingFeature) ([]byte, error) {
	var content []byte
	if f.BasicService != nil {
		bs, err := encodeExtBasicServiceAlt(f.BasicService)
		if err != nil {
			return nil, err
		}
		content = bs
	}
	content = append(content, encodeSSStatus(f.SSStatus)...)
	var long []byte
	if f.ForwardedToNumber != nil {
		n, err := encodeAddressString(f.ForwardedToNumber)
		if err != nil {
			return nil, fmt.Errorf("forwardedToNumber: %w", err)
		}
		switch {
		case len(n) <= maxISDNAddressLength:
			content = append(content, tlv(asn1.ClassContextSpecific, 5, false, n)...)
		case len(n) <= maxFTNAddressLength:
			long = tlv(asn1.ClassContextSpecific, 10, false, n)
		default:
			return nil, fmt.Errorf("forwardedToNumber of %d octets, at most %d", len(n), maxFTNAddressLength)
		}
	}
	if f.ForwardedToSubaddress != nil {
		if len(f.ForwardedToSubaddress) < 1 || len(f.ForwardedToSubaddress) > maxISDNSubaddressLength {
			return nil, fmt.Errorf("forwardedToSubaddress of %d octets, want 1..%d", len(f.ForwardedToSubaddress), maxISDNSubaddressLength)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 8, false, f.ForwardedToSubaddress)...)
	}
	if f.ForwardingOptions != nil {
		if len(f.ForwardingOptions) < 1 || len(f.ForwardingOptions) > maxForwOptionsLength {
			return nil, fmt.Errorf("forwardingOptions of %d octets, want 1..%d", len(f.ForwardingOptions), maxForwOptionsLength)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 6, false, f.ForwardingOptions)...)
	}
	if t := f.NoReplyConditionTime; t != nil {
		if *t < minNoReplyConditionTime || *t > maxNoReplyConditionTime {
			return nil, fmt.Errorf("noReplyConditionTime %d, want %d..%d", *t, minNoReplyConditionTime, maxNoReplyConditionTime)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 7, false, []byte{byte(*t)})...)
	}
	content = append(content, long...)
	return tlv(asn1.ClassUniversal, asn1.TagSequence, true, content), nil
}

func encodeSSStatus(s byte) []byte { return tlv(asn1.ClassContextSpecific, 4, false, []byte{s}) }

// encodeExtBasicServiceAlt encodes an untagged Ext-BasicServiceCode: the
// alternative itself, ext-BearerService [2] or ext-Teleservice [3].
func encodeExtBasicServiceAlt(b *BasicServiceCode) ([]byte, error) {
	if len(b.Code) < 1 || len(b.Code) > maxExtBasicServiceCodeSize {
		return nil, fmt.Errorf("basicService: code of %d octets, want 1..%d", len(b.Code), maxExtBasicServiceCodeSize)
	}
	tag := 3
	if b.Bearer {
		tag = 2
	}
	return tlv(asn1.ClassContextSpecific, tag, false, b.Code), nil
}

// ----- decoder (VLR → gateway) -----------------------------------------------

// DecodeInsertSubscriberDataRes decodes InsertSubscriberDataRes:
//
//	SEQUENCE { teleserviceList [1] OPTIONAL, bearerServiceList [2]
//	           OPTIONAL, ss-List [3] OPTIONAL, odb-GeneralData [4]
//	           OPTIONAL, regionalSubscriptionResponse [5] OPTIONAL,
//	           supportedCamelPhases [6] OPTIONAL, extensionContainer [7]
//	           OPTIONAL, …, offeredCamel4CSIs [8] OPTIONAL,
//	           supportedFeatures [9] OPTIONAL, ext-SupportedFeatures [10]
//	           OPTIONAL }
//
// The result is optional: a ReturnResult without a parameter decodes to an
// empty result.
func DecodeInsertSubscriberDataRes(b []byte) (*InsertSubscriberDataRes, error) {
	res := &InsertSubscriberDataRes{}
	if len(b) == 0 {
		return res, nil
	}
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("InsertSubscriberDataRes: %w", err)
	}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue
		}
		switch {
		case f.Tag == 1 && f.IsCompound:
			res.Teleservices, err = decodeServiceCodeList("teleserviceList", f.Bytes, maxNumOfTeleservices)
		case f.Tag == 2 && f.IsCompound:
			res.BearerServices, err = decodeServiceCodeList("bearerServiceList", f.Bytes, maxNumOfBearerServices)
		case f.Tag == 3 && f.IsCompound:
			res.SSCodes, err = decodeSSList(f.Bytes)
		case f.Tag == 4 && !f.IsCompound:
			res.ODBGeneralData, err = decodeNamedBits(f.Bytes)
		case f.Tag == 5 && !f.IsCompound:
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				if v < 0 || v > maxRegionalSubscriptionResult {
					err = fmt.Errorf("regionalSubscriptionResponse %d", v)
				} else {
					res.RegionalSubscriptionResponse = &v
				}
			}
		case f.Tag == 6 && !f.IsCompound:
			res.SupportedCAMELPhases, err = decodeNamedBits(f.Bytes)
		case f.Tag == 8 && !f.IsCompound:
			res.OfferedCAMEL4CSIs, err = decodeNamedBits(f.Bytes)
		case f.Tag == 9 && !f.IsCompound:
			res.SupportedFeatures, err = decodeNamedBits(f.Bytes)
		}
		if err != nil {
			return nil, fmt.Errorf("InsertSubscriberDataRes: %w", err)
		}
	}
	return res, nil
}

func decodeServiceCodeList(name string, b []byte, max int) ([][]byte, error) {
	items, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(items) < 1 || len(items) > max {
		return nil, fmt.Errorf("%s: %d codes, want 1..%d", name, len(items), max)
	}
	out := make([][]byte, 0, len(items))
	for _, it := range items {
		if !isUniversal(it, asn1.TagOctetString, false) {
			return nil, fmt.Errorf("%s: element is not an OCTET STRING", name)
		}
		c, err := fixedOctets(name, it.Bytes, 1, maxExtBasicServiceCodeSize)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// SplitSubscriberData encodes data as the arguments of one or more
// insertSubscriberData within a location update (no IMSI), each of which
// fits: fits reports whether an encoded argument fits the message it is to
// go in. When the whole does not fit, the first part carries everything but
// the service lists and the supplementary services — networkAccessMode
// may only be in the first (the ASN.1's comment) — and those follow,
// packed in order. A service list is never divided: some bearer service
// groups must be sent as a pair (TS 29.002 §8.8.1.3). A part that cannot
// fit alone is an error.
func SplitSubscriberData(data *SubscriberData, fits func(param []byte) (bool, error)) ([][]byte, error) {
	encode := func(d *SubscriberData) ([]byte, error) {
		return EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{Data: *d})
	}
	whole, err := encode(data)
	if err != nil {
		return nil, err
	}
	if ok, err := fits(whole); err != nil || ok {
		return [][]byte{whole}, err
	}

	first := *data
	first.BearerServices, first.Teleservices, first.ProvisionedSS = nil, nil, nil
	var items []func(*SubscriberData)
	if data.BearerServices != nil {
		items = append(items, func(d *SubscriberData) { d.BearerServices = data.BearerServices })
	}
	if data.Teleservices != nil {
		items = append(items, func(d *SubscriberData) { d.Teleservices = data.Teleservices })
	}
	for i := range data.ProvisionedSS {
		ss := data.ProvisionedSS[i]
		items = append(items, func(d *SubscriberData) { d.ProvisionedSS = append(d.ProvisionedSS, ss) })
	}

	var parts [][]byte
	cur, curEnc := first, []byte(nil)
	if curEnc, err = encode(&cur); err != nil {
		return nil, err
	}
	empty := func(enc []byte) bool { return len(enc) == 2 } // 30 00
	if ok, err := fits(curEnc); err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("insertSubscriberData: the subscriber's own data (%d octets) does not fit one message", len(curEnc)))
	}
	for _, add := range items {
		next := cur
		next.ProvisionedSS = append([]SSInfo(nil), cur.ProvisionedSS...)
		add(&next)
		enc, err := encode(&next)
		if err != nil {
			return nil, err
		}
		ok, err := fits(enc)
		if err != nil {
			return nil, err
		}
		if ok {
			cur, curEnc = next, enc
			continue
		}
		if empty(curEnc) {
			return nil, fmt.Errorf("insertSubscriberData: one list or supplementary service (%d octets) does not fit one message", len(enc))
		}
		parts = append(parts, curEnc)
		cur = SubscriberData{}
		add(&cur)
		if curEnc, err = encode(&cur); err != nil {
			return nil, err
		}
		if ok, err := fits(curEnc); err != nil || !ok {
			return nil, errors.Join(err, fmt.Errorf("insertSubscriberData: one list or supplementary service (%d octets) does not fit one message", len(curEnc)))
		}
	}
	if !empty(curEnc) {
		parts = append(parts, curEnc)
	}
	return parts, nil
}
