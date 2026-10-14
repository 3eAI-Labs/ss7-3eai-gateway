// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// AnyTimeInterrogation (op 71, gsmSCF → HLR) and ProvideSubscriberInfo
// (op 70, HLR → VLR), 3GPP TS 29.002 §8.11.1–8.11.2 and MAP-MS-DataTypes
// (§17.7.1, IMPLICIT TAGS). The gateway originates both; it never answers
// them, so the argument encoders and result decoders are the production path
// and the inverse functions exist for test peers.
//
// A tagged CHOICE is tagged explicitly even under IMPLICIT TAGS (ITU-T X.680
// §31.2.7): SubscriberIdentity [0], SubscriberState [1], PS-SubscriberState
// [4]/[12] and CellGlobalIdOrServiceAreaIdOrLAI appear as a constructed
// context tag wrapping the chosen alternative. Everything else tagged is
// implicit.
//
// The structures are walked TLV by TLV rather than through encoding/asn1
// struct tags: most fields are OPTIONAL with context tags in an order that is
// not the tag order, and unknown fields after the extension marker must be
// skipped.

// RequestedInfo is TS 29.002 RequestedInfo, the subset the gateway asks for.
type RequestedInfo struct {
	LocationInformation             bool        // [0]
	SubscriberState                 bool        // [1]
	CurrentLocation                 bool        // [3]
	RequestedDomain                 *DomainType // [4]
	IMEI                            bool        // [6]
	MSClassmark                     bool        // [5]
	MNPRequestedInfo                bool        // [7]
	LocationInformationEPSSupported bool        // [11]
}

// DomainType is TS 29.002 DomainType.
type DomainType uint8

const (
	DomainCS DomainType = 0
	DomainPS DomainType = 1
)

// SubscriberIdentity is a CHOICE: exactly one is set.
type SubscriberIdentity struct {
	IMSI   *IMSI   // [0]
	MSISDN *MSISDN // [1]
}

// AnyTimeInterrogationRequest is AnyTimeInterrogationArg.
type AnyTimeInterrogationRequest struct {
	SubscriberIdentity SubscriberIdentity
	RequestedInfo      RequestedInfo
	GSMSCFAddress      AddressString
}

// ProvideSubscriberInfoRequest is ProvideSubscriberInfoArg.
type ProvideSubscriberInfoRequest struct {
	IMSI          IMSI
	LMSI          []byte // 4 octets, optional
	RequestedInfo RequestedInfo
}

// AnyTimeInterrogationResponse is AnyTimeInterrogationRes.
type AnyTimeInterrogationResponse struct {
	SubscriberInfo SubscriberInfo
}

// ProvideSubscriberInfoResponse is ProvideSubscriberInfoRes.
type ProvideSubscriberInfoResponse struct {
	SubscriberInfo SubscriberInfo
}

// SubscriberInfo is TS 29.002 SubscriberInfo, the fields the gateway
// decodes. Fields beyond these (GPRS MS class, T-ADS data, time zone, 5GS
// location, …) are skipped.
type SubscriberInfo struct {
	LocationInformation     *LocationInformation    // [0]
	SubscriberState         *SubscriberState        // [1]
	LocationInformationGPRS *LocationInformation    // [3]
	PSSubscriberState       *SubscriberState        // [4]
	IMEI                    string                  // [5]
	MSClassmark2            []byte                  // [6]
	MNPInfoRes              *MNPInfoRes             // [8]
	EPSSubscriberState      *SubscriberState        // [12]
	LocationInformationEPS  *LocationInformationEPS // [13]
}

// LocationInformation is LocationInformation (CS) or LocationInformationGPRS
// (PS): the fields of the other domain stay empty.
type LocationInformation struct {
	AgeOfLocationInformation *int
	GeographicalInformation  []byte
	VLRNumber                *AddressString // CS
	LocationNumber           []byte         // CS
	MSCNumber                *AddressString // CS
	SGSNNumber               *AddressString // PS
	RouteingAreaIdentity     []byte         // PS
	// At most one of CellGlobalID, ServiceAreaID and LAI: the CHOICE
	// cellGlobalIdOrServiceAreaIdOrLAI, its fixed-length alternative split
	// by sai-Present.
	CellGlobalID             *CellGlobalID
	ServiceAreaID            *ServiceAreaID
	LAI                      *LAI
	GeodeticInformation      []byte
	CurrentLocationRetrieved bool
	EPS                      *LocationInformationEPS // CS: [10]
}

// CellGlobalID is CellGlobalIdOrServiceAreaIdFixedLength decoded as a cell
// global identity.
type CellGlobalID struct {
	MCC, MNC string
	LAC, CI  uint16
}

// ServiceAreaID is CellGlobalIdOrServiceAreaIdFixedLength decoded as a
// service area identity (sai-Present).
type ServiceAreaID struct {
	MCC, MNC string
	LAC, SAC uint16
}

// LAI is LAIFixedLength decoded.
type LAI struct {
	MCC, MNC string
	LAC      uint16
}

// LocationInformationEPS is TS 29.002 LocationInformationEPS.
type LocationInformationEPS struct {
	EUTRANCGI                []byte // [0], TS 29.118
	TrackingAreaIdentity     []byte // [1], TS 29.118
	GeographicalInformation  []byte // [3]
	GeodeticInformation      []byte // [4]
	CurrentLocationRetrieved bool   // [5]
	AgeOfLocationInformation *int   // [6]
	MMEName                  string // [7]
}

// SubscriberState is SubscriberState or PS-SubscriberState: State is the
// ASN.1 name of the chosen alternative, NotReachableReason the
// NotReachableReason name accompanying netDetNotReachable.
type SubscriberState struct {
	State              string
	NotReachableReason string
}

// MNPInfoRes is TS 29.002 MNPInfoRes.
type MNPInfoRes struct {
	RouteingNumber          string
	IMSI                    IMSI
	MSISDN                  *AddressString
	NumberPortabilityStatus string
}

// Value names of the CHOICEs and ENUMERATEDs above (TS 29.002 MAP-MS-DataTypes).
var (
	subscriberStateNames   = map[int]string{0: "assumedIdle", 1: "camelBusy", 2: "notProvidedFromVLR"}
	psSubscriberStateNames = map[int]string{
		0: "notProvidedFromSGSNorMME", 1: "ps-Detached", 2: "ps-AttachedNotReachableForPaging",
		3: "ps-AttachedReachableForPaging", 4: "ps-PDP-ActiveNotReachableForPaging",
		5: "ps-PDP-ActiveReachableForPaging",
	}
	notReachableReasonNames      = map[int]string{0: "msPurged", 1: "imsiDetached", 2: "restrictedArea", 3: "notRegistered"}
	numberPortabilityStatusNames = map[int]string{
		0: "notKnownToBePorted", 1: "ownNumberPortedOut", 2: "foreignNumberPortedToForeignNetwork",
		4: "ownNumberNotPortedOut", 5: "foreignNumberPortedIn",
	}
)

const netDetNotReachable = "netDetNotReachable"

// ----- encoders (gateway → network) ----------------------------------------

// EncodeAnyTimeInterrogationArg encodes AnyTimeInterrogationArg:
//
//	SEQUENCE { subscriberIdentity [0] SubscriberIdentity,
//	           requestedInfo [1] RequestedInfo,
//	           gsmSCF-Address [3] ISDN-AddressString, … }
func EncodeAnyTimeInterrogationArg(r *AnyTimeInterrogationRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("AnyTimeInterrogationArg: nil")
	}
	var identity []byte
	switch id := r.SubscriberIdentity; {
	case id.IMSI != nil && id.MSISDN == nil:
		imsi, err := encodeIMSI(*id.IMSI)
		if err != nil {
			return nil, err
		}
		identity = tlv(asn1.ClassContextSpecific, 0, false, imsi)
	case id.MSISDN != nil && id.IMSI == nil:
		msisdn, err := encodeAddressString(&AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: string(*id.MSISDN)})
		if err != nil {
			return nil, fmt.Errorf("msisdn: %w", err)
		}
		identity = tlv(asn1.ClassContextSpecific, 1, false, msisdn)
	default:
		return nil, errors.New("AnyTimeInterrogationArg: exactly one of imsi and msisdn required")
	}
	requested, err := encodeRequestedInfo(r.RequestedInfo)
	if err != nil {
		return nil, err
	}
	scf, err := encodeAddressString(&r.GSMSCFAddress)
	if err != nil {
		return nil, fmt.Errorf("gsmSCF-Address: %w", err)
	}
	return sequence(
		tlv(asn1.ClassContextSpecific, 0, true, identity),
		tlv(asn1.ClassContextSpecific, 1, true, requested),
		tlv(asn1.ClassContextSpecific, 3, false, scf),
	), nil
}

// EncodeProvideSubscriberInfoArg encodes ProvideSubscriberInfoArg:
//
//	SEQUENCE { imsi [0] IMSI, lmsi [1] LMSI OPTIONAL,
//	           requestedInfo [2] RequestedInfo, … }
func EncodeProvideSubscriberInfoArg(r *ProvideSubscriberInfoRequest) ([]byte, error) {
	if r == nil {
		return nil, errors.New("ProvideSubscriberInfoArg: nil")
	}
	imsi, err := encodeIMSI(r.IMSI)
	if err != nil {
		return nil, err
	}
	requested, err := encodeRequestedInfo(r.RequestedInfo)
	if err != nil {
		return nil, err
	}
	fields := [][]byte{tlv(asn1.ClassContextSpecific, 0, false, imsi)}
	if len(r.LMSI) > 0 {
		if len(r.LMSI) != 4 {
			return nil, errors.New("ProvideSubscriberInfoArg: lmsi must be 4 octets")
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 1, false, r.LMSI))
	}
	fields = append(fields, tlv(asn1.ClassContextSpecific, 2, true, requested))
	return sequence(fields...), nil
}

// encodeRequestedInfo encodes the content of RequestedInfo in the order the
// ASN.1 declares its fields (imei [6] before ms-classmark [5]).
func encodeRequestedInfo(ri RequestedInfo) ([]byte, error) {
	if !ri.LocationInformation && (ri.CurrentLocation || ri.LocationInformationEPSSupported) {
		return nil, errors.New("RequestedInfo: currentLocation and locationInformationEPS-Supported require locationInformation")
	}
	null := func(tag int) []byte { return tlv(asn1.ClassContextSpecific, tag, false, nil) }
	var out []byte
	if ri.LocationInformation {
		out = append(out, null(0)...)
	}
	if ri.SubscriberState {
		out = append(out, null(1)...)
	}
	if ri.CurrentLocation {
		out = append(out, null(3)...)
	}
	if ri.RequestedDomain != nil {
		if *ri.RequestedDomain > DomainPS {
			return nil, fmt.Errorf("RequestedInfo: requestedDomain %d", *ri.RequestedDomain)
		}
		out = append(out, tlv(asn1.ClassContextSpecific, 4, false, []byte{byte(*ri.RequestedDomain)})...)
	}
	if ri.IMEI {
		out = append(out, null(6)...)
	}
	if ri.MSClassmark {
		out = append(out, null(5)...)
	}
	if ri.MNPRequestedInfo {
		out = append(out, null(7)...)
	}
	if ri.LocationInformationEPSSupported {
		out = append(out, null(11)...)
	}
	if len(out) == 0 {
		return nil, errors.New("RequestedInfo: nothing requested")
	}
	return out, nil
}

// ----- decoders (network → gateway) ----------------------------------------

// DecodeAnyTimeInterrogationRes decodes AnyTimeInterrogationRes:
// SEQUENCE { subscriberInfo SubscriberInfo, extensionContainer OPTIONAL, … }.
func DecodeAnyTimeInterrogationRes(b []byte) (*AnyTimeInterrogationResponse, error) {
	info, err := decodeSubscriberInfoResult(b)
	if err != nil {
		return nil, fmt.Errorf("AnyTimeInterrogationRes: %w", err)
	}
	return &AnyTimeInterrogationResponse{SubscriberInfo: *info}, nil
}

// DecodeProvideSubscriberInfoRes decodes ProvideSubscriberInfoRes, the same
// shape as AnyTimeInterrogationRes.
func DecodeProvideSubscriberInfoRes(b []byte) (*ProvideSubscriberInfoResponse, error) {
	info, err := decodeSubscriberInfoResult(b)
	if err != nil {
		return nil, fmt.Errorf("ProvideSubscriberInfoRes: %w", err)
	}
	return &ProvideSubscriberInfoResponse{SubscriberInfo: *info}, nil
}

func decodeSubscriberInfoResult(b []byte) (*SubscriberInfo, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 || !isUniversal(fields[0], asn1.TagSequence, true) {
		return nil, errors.New("subscriberInfo missing")
	}
	return decodeSubscriberInfo(fields[0].Bytes)
}

func decodeSubscriberInfo(b []byte) (*SubscriberInfo, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("subscriberInfo: %w", err)
	}
	si := &SubscriberInfo{}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue
		}
		var err error
		switch {
		case f.Tag == 0 && f.IsCompound:
			si.LocationInformation, err = decodeLocationInformation(f.Bytes, false)
		case f.Tag == 1 && f.IsCompound:
			si.SubscriberState, err = decodeSubscriberState(f.Bytes, false)
		case f.Tag == 3 && f.IsCompound:
			si.LocationInformationGPRS, err = decodeLocationInformation(f.Bytes, true)
		case f.Tag == 4 && f.IsCompound:
			si.PSSubscriberState, err = decodeSubscriberState(f.Bytes, true)
		case f.Tag == 5 && !f.IsCompound:
			si.IMEI, err = decodeIMEI(f.Bytes)
		case f.Tag == 6 && !f.IsCompound:
			si.MSClassmark2, err = fixedOctets("ms-Classmark2", f.Bytes, 3, 3)
		case f.Tag == 8 && f.IsCompound:
			si.MNPInfoRes, err = decodeMNPInfoRes(f.Bytes)
		case f.Tag == 12 && f.IsCompound:
			si.EPSSubscriberState, err = decodeSubscriberState(f.Bytes, true)
		case f.Tag == 13 && f.IsCompound:
			si.LocationInformationEPS, err = decodeLocationInformationEPS(f.Bytes)
		}
		if err != nil {
			return nil, err
		}
	}
	return si, nil
}

// decodeLocationInformation reads LocationInformation (gprs false) or
// LocationInformationGPRS (gprs true); the two number their fields
// differently.
func decodeLocationInformation(b []byte, gprs bool) (*LocationInformation, error) {
	name := "locationInformation"
	if gprs {
		name = "locationInformationGPRS"
	}
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	li := &LocationInformation{}
	var cell []byte
	var cellIsLAI, sai bool
	for _, f := range fields {
		var err error
		if f.Class != asn1.ClassContextSpecific {
			// ageOfLocationInformation is the one untagged field (CS only).
			if !gprs && isUniversal(f, asn1.TagInteger, false) {
				if li.AgeOfLocationInformation, err = decodeAge(f.Bytes); err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
			}
			continue
		}
		switch tag := f.Tag; {
		case !gprs && tag == 0 && !f.IsCompound, gprs && tag == 2 && !f.IsCompound:
			li.GeographicalInformation, err = fixedOctets("geographicalInformation", f.Bytes, 8, 8)
		case !gprs && tag == 1 && !f.IsCompound:
			li.VLRNumber, err = decodeAddressString(f.Bytes)
		case !gprs && tag == 2 && !f.IsCompound:
			li.LocationNumber, err = fixedOctets("locationNumber", f.Bytes, 2, 10)
		case !gprs && tag == 3 && f.IsCompound, gprs && tag == 0 && f.IsCompound:
			cell, cellIsLAI, err = decodeCellChoice(f.Bytes)
		case !gprs && tag == 6 && !f.IsCompound:
			li.MSCNumber, err = decodeAddressString(f.Bytes)
		case !gprs && tag == 7 && !f.IsCompound, gprs && tag == 7 && !f.IsCompound:
			li.GeodeticInformation, err = fixedOctets("geodeticInformation", f.Bytes, 10, 10)
		case tag == 8 && !f.IsCompound:
			li.CurrentLocationRetrieved = true
		case !gprs && tag == 9 && !f.IsCompound, gprs && tag == 6 && !f.IsCompound:
			sai = true
		case !gprs && tag == 10 && f.IsCompound:
			li.EPS, err = decodeLocationInformationEPS(f.Bytes)
		case gprs && tag == 1 && !f.IsCompound:
			li.RouteingAreaIdentity, err = fixedOctets("routeingAreaIdentity", f.Bytes, 6, 6)
		case gprs && tag == 3 && !f.IsCompound:
			li.SGSNNumber, err = decodeAddressString(f.Bytes)
		case gprs && tag == 9 && !f.IsCompound:
			li.AgeOfLocationInformation, err = decodeAge(f.Bytes)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if cell != nil {
		mcc, mnc, err := decodePLMN(cell[:3])
		if err != nil {
			return nil, fmt.Errorf("%s: cell identity: %w", name, err)
		}
		lac := uint16(cell[3])<<8 | uint16(cell[4])
		switch {
		case cellIsLAI:
			li.LAI = &LAI{MCC: mcc, MNC: mnc, LAC: lac}
		case sai:
			li.ServiceAreaID = &ServiceAreaID{MCC: mcc, MNC: mnc, LAC: lac, SAC: uint16(cell[5])<<8 | uint16(cell[6])}
		default:
			li.CellGlobalID = &CellGlobalID{MCC: mcc, MNC: mnc, LAC: lac, CI: uint16(cell[5])<<8 | uint16(cell[6])}
		}
	}
	return li, nil
}

// decodeCellChoice reads the content of CellGlobalIdOrServiceAreaIdOrLAI:
// cellGlobalIdOrServiceAreaIdFixedLength [0] (7 octets) or laiFixedLength [1]
// (5 octets).
func decodeCellChoice(b []byte) (cell []byte, isLAI bool, err error) {
	alt, err := onlyChild(b, "cellGlobalIdOrServiceAreaIdOrLAI")
	if err != nil {
		return nil, false, err
	}
	switch {
	case alt.Class == asn1.ClassContextSpecific && alt.Tag == 0 && !alt.IsCompound:
		cell, err = fixedOctets("cellGlobalIdOrServiceAreaIdFixedLength", alt.Bytes, 7, 7)
		return cell, false, err
	case alt.Class == asn1.ClassContextSpecific && alt.Tag == 1 && !alt.IsCompound:
		cell, err = fixedOctets("laiFixedLength", alt.Bytes, 5, 5)
		return cell, true, err
	}
	return nil, false, fmt.Errorf("cellGlobalIdOrServiceAreaIdOrLAI: unknown alternative [%d]", alt.Tag)
}

// decodePLMN reads the MCC and MNC from the first three octets of a CGI or
// LAI (TS 29.002 CellGlobalIdOrServiceAreaIdFixedLength): MCC digits 1, 2 in
// octet 1, MCC digit 3 and MNC digit 3 (or filler 1111) in octet 2, MNC
// digits 1, 2 in octet 3 — low nibble first.
func decodePLMN(b []byte) (mcc, mnc string, err error) {
	digit := func(n byte) (byte, error) {
		if n > 9 {
			return 0, fmt.Errorf("PLMN digit 0x%X", n)
		}
		return '0' + n, nil
	}
	nibbles := []byte{b[0] & 0x0F, b[0] >> 4, b[1] & 0x0F, b[2] & 0x0F, b[2] >> 4}
	out := make([]byte, 0, 6)
	for _, n := range nibbles {
		d, err := digit(n)
		if err != nil {
			return "", "", err
		}
		out = append(out, d)
	}
	mcc, mnc = string(out[:3]), string(out[3:])
	if mnc3 := b[1] >> 4; mnc3 != 0x0F {
		d, err := digit(mnc3)
		if err != nil {
			return "", "", err
		}
		mnc += string(d)
	}
	return mcc, mnc, nil
}

func decodeLocationInformationEPS(b []byte) (*LocationInformationEPS, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("locationInformationEPS: %w", err)
	}
	eps := &LocationInformationEPS{}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific || f.IsCompound {
			continue
		}
		var err error
		switch f.Tag {
		case 0:
			eps.EUTRANCGI, err = fixedOctets("e-utranCellGlobalIdentity", f.Bytes, 7, 7)
		case 1:
			eps.TrackingAreaIdentity, err = fixedOctets("trackingAreaIdentity", f.Bytes, 5, 5)
		case 3:
			eps.GeographicalInformation, err = fixedOctets("geographicalInformation", f.Bytes, 8, 8)
		case 4:
			eps.GeodeticInformation, err = fixedOctets("geodeticInformation", f.Bytes, 10, 10)
		case 5:
			eps.CurrentLocationRetrieved = true
		case 6:
			eps.AgeOfLocationInformation, err = decodeAge(f.Bytes)
		case 7:
			var name []byte
			name, err = fixedOctets("mme-Name", f.Bytes, 9, 255)
			eps.MMEName = string(name)
		}
		if err != nil {
			return nil, fmt.Errorf("locationInformationEPS: %w", err)
		}
	}
	return eps, nil
}

// decodeSubscriberState reads the content of SubscriberState (ps false) or
// PS-SubscriberState (ps true), each a CHOICE.
func decodeSubscriberState(b []byte, ps bool) (*SubscriberState, error) {
	alt, err := onlyChild(b, "subscriberState")
	if err != nil {
		return nil, err
	}
	if isUniversal(alt, asn1.TagEnum, false) {
		v, err := berInt(alt.Bytes)
		if err != nil {
			return nil, fmt.Errorf("notReachableReason: %w", err)
		}
		// Unknown values pass through by number rather than failing the answer.
		reason, ok := notReachableReasonNames[v]
		if !ok {
			reason = fmt.Sprintf("notReachableReason(%d)", v)
		}
		return &SubscriberState{State: netDetNotReachable, NotReachableReason: reason}, nil
	}
	if alt.Class != asn1.ClassContextSpecific {
		return nil, fmt.Errorf("subscriberState: unexpected class %d", alt.Class)
	}
	names := subscriberStateNames
	if ps {
		names = psSubscriberStateNames
	}
	name, ok := names[alt.Tag]
	if !ok {
		// An alternative added after this codec passes through by number,
		// like an unknown notReachableReason, rather than failing the answer.
		name = fmt.Sprintf("subscriberState[%d]", alt.Tag)
	}
	// The PDP-active alternatives carry a PDP-ContextInfoList, not decoded.
	return &SubscriberState{State: name}, nil
}

func decodeMNPInfoRes(b []byte) (*MNPInfoRes, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("mnpInfoRes: %w", err)
	}
	m := &MNPInfoRes{}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific || f.IsCompound {
			continue
		}
		var err error
		switch f.Tag {
		case 0:
			if len(f.Bytes) < 1 || len(f.Bytes) > 5 {
				err = errors.New("routeingNumber length out of range")
			} else {
				m.RouteingNumber, err = decodeTBCD(f.Bytes)
			}
		case 1:
			m.IMSI, err = decodeIMSI(f.Bytes)
		case 2:
			m.MSISDN, err = decodeAddressString(f.Bytes)
		case 3:
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				// "reception of other values … the receiver shall ignore the
				// whole NumberPortabilityStatus" (TS 29.002 MNPInfoRes).
				m.NumberPortabilityStatus = numberPortabilityStatusNames[v]
			}
		}
		if err != nil {
			return nil, fmt.Errorf("mnpInfoRes: %w", err)
		}
	}
	return m, nil
}

// decodeIMEI reads an IMEI or IMEISV: TBCD, 8 octets.
func decodeIMEI(b []byte) (string, error) {
	if len(b) != 8 {
		return "", fmt.Errorf("imei: %d octets, want 8", len(b))
	}
	s, err := decodeTBCD(b)
	if err != nil {
		return "", fmt.Errorf("imei: %w", err)
	}
	return s, nil
}

// decodeAge reads AgeOfLocationInformation, INTEGER (0..32767).
func decodeAge(b []byte) (*int, error) {
	v, err := berInt(b)
	if err != nil {
		return nil, fmt.Errorf("ageOfLocationInformation: %w", err)
	}
	if v < 0 || v > 32767 {
		return nil, fmt.Errorf("ageOfLocationInformation %d out of range 0..32767", v)
	}
	return &v, nil
}

// ----- BER helpers -----------------------------------------------------------

// berChildren splits the content octets of a constructed value into its
// elements.
func berChildren(b []byte) ([]asn1.RawValue, error) {
	var out []asn1.RawValue
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
		b = rest
	}
	return out, nil
}

// sequenceContent reads one SEQUENCE with nothing after it and returns its
// elements.
func sequenceContent(b []byte) ([]asn1.RawValue, error) {
	var seq asn1.RawValue
	rest, err := asn1.Unmarshal(b, &seq)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, errors.New("trailing data")
	}
	if !isUniversal(seq, asn1.TagSequence, true) {
		return nil, errors.New("not a SEQUENCE")
	}
	return berChildren(seq.Bytes)
}

// onlyChild reads the single element an explicitly tagged CHOICE wraps.
func onlyChild(b []byte, name string) (asn1.RawValue, error) {
	children, err := berChildren(b)
	if err != nil {
		return asn1.RawValue{}, fmt.Errorf("%s: %w", name, err)
	}
	if len(children) != 1 {
		return asn1.RawValue{}, fmt.Errorf("%s: %d alternatives, want 1", name, len(children))
	}
	return children[0], nil
}

func isUniversal(rv asn1.RawValue, tag int, compound bool) bool {
	return rv.Class == asn1.ClassUniversal && rv.Tag == tag && rv.IsCompound == compound
}

// berInt reads a two's-complement INTEGER or ENUMERATED of at most 4 octets.
func berInt(b []byte) (int, error) {
	if len(b) == 0 || len(b) > 4 {
		return 0, fmt.Errorf("integer of %d octets", len(b))
	}
	v := int(int8(b[0]))
	for _, o := range b[1:] {
		v = v<<8 | int(o)
	}
	return v, nil
}

func fixedOctets(name string, b []byte, min, max int) ([]byte, error) {
	if len(b) < min || len(b) > max {
		return nil, fmt.Errorf("%s: %d octets, want %d..%d", name, len(b), min, max)
	}
	return append([]byte(nil), b...), nil
}

// tlv encodes one BER element with a definite length.
func tlv(class, tag int, compound bool, content []byte) []byte {
	b, err := asn1.Marshal(asn1.RawValue{Class: class, Tag: tag, IsCompound: compound, Bytes: content})
	if err != nil {
		panic(fmt.Sprintf("tlv: %v", err)) // only for tags ≥ 31 or negative, never used here
	}
	return b
}

func sequence(fields ...[]byte) []byte {
	var content []byte
	for _, f := range fields {
		content = append(content, f...)
	}
	return tlv(asn1.ClassUniversal, asn1.TagSequence, true, content)
}
