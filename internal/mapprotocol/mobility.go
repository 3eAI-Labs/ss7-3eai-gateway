// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// Location management with the gateway in the HLR's role, 3GPP TS 29.002
// §8.1 and MAP-MS-DataTypes (§17.7.1, IMPLICIT TAGS):
//   - updateLocation (op 2): a VLR registers a subscriber; the gateway
//     decodes the argument and encodes the result;
//   - cancelLocation (op 3) and provideRoamingNumber (op 4): the gateway
//     sends them to a VLR; it encodes the argument and decodes the result.
//
// The inverse functions, for test peers playing the VLR, are in
// mobility_peer.go.

// UpdateLocationArg is TS 29.002 UpdateLocationArg, the fields the gateway
// reads. v-gmlc-Address, pagingArea, eplmn-List and mme-DiameterAddress
// are skipped.
type UpdateLocationArg struct {
	IMSI      IMSI
	MSCNumber AddressString // [1]
	VLRNumber AddressString
	LMSI      []byte // [10], 4 octets
	// The extension fields.
	VLRCapability               *VLRCapability // [6]
	InformPreviousNetworkEntity bool           // [11]
	CSLCSNotSupportedByUE       bool           // [12]
	// IMEISV is ADD-Info's imeisv [13]/[0], TBCD digits.
	IMEISV string
	// SkipSubscriberDataUpdate is [15], or ADD-Info's [1]: they carry the
	// same meaning (the ASN.1 says so).
	SkipSubscriberDataUpdate bool
	RestorationIndicator     bool // [16]
}

// VLRCapability is TS 29.002 VLR-Capability. Bit strings are given as the
// numbers of the bits set.
type VLRCapability struct {
	SupportedCAMELPhases []int // [0]: 0 = phase 1 … 3 = phase 4
	SOLSASupport         bool  // [2]
	// ISTSupport is istSupportIndicator [1]: nil absent, 0 basic, 1 command
	// (a higher value received is mapped to 1, as the ASN.1 says).
	ISTSupport *int
	// SuperCharger is superChargerSupportedInServingNetworkEntity [3]:
	// nil absent; else SendSubscriberData, or the AgeIndicator of the data
	// stored.
	SuperCharger                 *SuperChargerInfo
	LongFTNSupported             bool  // [4]
	SupportedLCSCapabilitySets   []int // [5]
	OfferedCAMEL4CSIs            []int // [6]
	SupportedRATTypes            []int // [7]
	LongGroupIDSupported         bool  // [8]
	MTRoamingForwardingSupported bool  // [9]
	MSISDNLessOperationSupported bool  // [10]
	ResetIDsSupported            bool  // [11]
}

// SuperChargerInfo is the CHOICE SuperChargerInfo.
type SuperChargerInfo struct {
	SendSubscriberData bool   // [0]
	SubscriberDataAge  []byte // [1] AgeIndicator, 1..6 octets
}

// UpdateLocationRes is TS 29.002 UpdateLocationRes: the HLR's number.
type UpdateLocationRes struct {
	HLRNumber AddressString
}

// CancellationType is TS 29.002 CancellationType.
type CancellationType int

const (
	CancellationUpdateProcedure        CancellationType = 0
	CancellationSubscriptionWithdraw   CancellationType = 1
	CancellationInitialAttachProcedure CancellationType = 2
)

// CancelLocationArg is TS 29.002 CancelLocationArg, the fields an HLR sends
// a VLR. typeOfUpdate and the MT roaming forwarding fields are for an SGSN
// or for MT roaming retry and are not sent.
type CancelLocationArg struct {
	IMSI IMSI
	// LMSI makes the identity imsi-WithLMSI; 4 octets.
	LMSI             []byte
	CancellationType *CancellationType
}

// CancelLocationRes is a successful cancelLocation: CancelLocationRes
// carries nothing the gateway reads.
type CancelLocationRes struct{}

// ProvideRoamingNumberArg is TS 29.002 ProvideRoamingNumberArg, the fields
// the gateway sends.
type ProvideRoamingNumberArg struct {
	IMSI      IMSI          // [0]
	MSCNumber AddressString // [1]
	MSISDN    *AddressString
	LMSI      []byte // [4], 4 octets
	// GSMBearerCapability is gsm-BearerCapability [5], an
	// ExternalSignalInfo: the protocol and the signal octets.
	GSMBearerCapability       *ExternalSignalInfo
	SuppressionOfAnnouncement bool           // [7]
	GMSCAddress               *AddressString // [8]
	CallReferenceNumber       []byte         // [9], 1..8 octets
	ORInterrogation           bool           // [10]
}

// ExternalSignalInfo is TS 29.002 ExternalSignalInfo.
type ExternalSignalInfo struct {
	// ProtocolID: 1 gsm-0408, 2 gsm-0806, 3 gsm-BSSMAP, 4 ets-300102-1.
	ProtocolID int
	SignalInfo []byte // 1..200 octets
}

// ProvideRoamingNumberRes is TS 29.002 ProvideRoamingNumberRes.
type ProvideRoamingNumberRes struct {
	RoamingNumber             AddressString
	ReleaseResourcesSupported bool
	VMSCAddress               *AddressString
}

// Errors of the mobility operations not named before.
const ErrNoRoamingNumberAvailable ErrorCode = 39

func init() { errorCodeNames[ErrNoRoamingNumberAvailable] = "noRoamingNumberAvailable" }

// UpdateLocationErrors are the errors updateLocation allows (TS 29.002
// §17.6.1).
var UpdateLocationErrors = map[ErrorCode]bool{
	ErrSystemFailure: true, ErrDataMissing: true, ErrUnexpectedDataValue: true,
	ErrUnknownSubscriber: true, ErrRoamingNotAllowed: true,
}

const (
	lmsiLength             = 4
	maxCallReferenceLength = 8
	maxSignalInfoLength    = 200
	maxAgeIndicatorLength  = 6
	maxProtocolID          = 4
	istCommandSupported    = 1
)

// ----- updateLocation (VLR → gateway) --------------------------------------

// DecodeUpdateLocationArg decodes UpdateLocationArg:
//
//	SEQUENCE { imsi IMSI, msc-Number [1] ISDN-AddressString,
//	           vlr-Number ISDN-AddressString, lmsi [10] LMSI OPTIONAL,
//	           extensionContainer OPTIONAL, …, vlr-Capability [6] …,
//	           informPreviousNetworkEntity [11], cs-LCS-NotSupportedByUE [12],
//	           v-gmlc-Address [2], add-info [13], pagingArea [14],
//	           skipSubscriberDataUpdate [15], restorationIndicator [16],
//	           eplmn-List [3], mme-DiameterAddress [4] }
func DecodeUpdateLocationArg(b []byte) (*UpdateLocationArg, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationArg: %w", err)
	}
	if len(fields) < 3 || !isUniversal(fields[0], asn1.TagOctetString, false) ||
		!isContext(fields[1], 1, false) || !isUniversal(fields[2], asn1.TagOctetString, false) {
		return nil, errors.New("UpdateLocationArg: imsi, msc-Number [1] and vlr-Number required, in that order")
	}
	arg := &UpdateLocationArg{}
	if arg.IMSI, err = decodeIMSI(fields[0].Bytes); err != nil {
		return nil, fmt.Errorf("UpdateLocationArg: %w", err)
	}
	msc, err := decodeISDNAddressString("msc-Number", fields[1].Bytes)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationArg: %w", err)
	}
	vlr, err := decodeISDNAddressString("vlr-Number", fields[2].Bytes)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationArg: %w", err)
	}
	arg.MSCNumber, arg.VLRNumber = *msc, *vlr
	for _, f := range fields[3:] {
		if f.Class != asn1.ClassContextSpecific {
			continue // extensionContainer
		}
		switch {
		case f.Tag == 10 && !f.IsCompound:
			if arg.LMSI, err = fixedOctets("lmsi", f.Bytes, lmsiLength, lmsiLength); err != nil {
				return nil, fmt.Errorf("UpdateLocationArg: %w", err)
			}
		case f.Tag == 6 && f.IsCompound:
			if arg.VLRCapability, err = decodeVLRCapability(f.Bytes); err != nil {
				return nil, fmt.Errorf("UpdateLocationArg: %w", err)
			}
		case f.Tag == 11 && !f.IsCompound:
			arg.InformPreviousNetworkEntity = true
		case f.Tag == 12 && !f.IsCompound:
			arg.CSLCSNotSupportedByUE = true
		case f.Tag == 13 && f.IsCompound:
			if err := decodeADDInfo(f.Bytes, arg); err != nil {
				return nil, fmt.Errorf("UpdateLocationArg: %w", err)
			}
		case f.Tag == 15 && !f.IsCompound:
			arg.SkipSubscriberDataUpdate = true
		case f.Tag == 16 && !f.IsCompound:
			arg.RestorationIndicator = true
		}
	}
	return arg, nil
}

func decodeVLRCapability(b []byte) (*VLRCapability, error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, fmt.Errorf("vlr-Capability: %w", err)
	}
	c := &VLRCapability{}
	bits := func(name string, f asn1.RawValue) ([]int, error) {
		v, err := decodeNamedBits(f.Bytes)
		if err != nil {
			return nil, fmt.Errorf("vlr-Capability %s: %w", name, err)
		}
		return v, nil
	}
	for _, f := range fields {
		if f.Class != asn1.ClassContextSpecific {
			continue // extensionContainer
		}
		switch {
		case f.Tag == 0 && !f.IsCompound:
			c.SupportedCAMELPhases, err = bits("supportedCamelPhases", f)
		case f.Tag == 2 && !f.IsCompound:
			c.SOLSASupport = true
		case f.Tag == 1 && !f.IsCompound:
			var v int
			if v, err = berInt(f.Bytes); err == nil {
				v = min(max(v, 0), istCommandSupported)
				c.ISTSupport = &v
			}
		case f.Tag == 3 && f.IsCompound:
			c.SuperCharger, err = decodeSuperChargerInfo(f.Bytes)
		case f.Tag == 4 && !f.IsCompound:
			c.LongFTNSupported = true
		case f.Tag == 5 && !f.IsCompound:
			c.SupportedLCSCapabilitySets, err = bits("supportedLCS-CapabilitySets", f)
		case f.Tag == 6 && !f.IsCompound:
			c.OfferedCAMEL4CSIs, err = bits("offeredCamel4CSIs", f)
		case f.Tag == 7 && !f.IsCompound:
			c.SupportedRATTypes, err = bits("supportedRAT-TypesIndicator", f)
		case f.Tag == 8 && !f.IsCompound:
			c.LongGroupIDSupported = true
		case f.Tag == 9 && !f.IsCompound:
			c.MTRoamingForwardingSupported = true
		case f.Tag == 10 && !f.IsCompound:
			c.MSISDNLessOperationSupported = true
		case f.Tag == 11 && !f.IsCompound:
			c.ResetIDsSupported = true
		}
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

// decodeSuperChargerInfo reads the content of the explicitly tagged CHOICE
// SuperChargerInfo.
func decodeSuperChargerInfo(b []byte) (*SuperChargerInfo, error) {
	alt, err := onlyChild(b, "superChargerSupportedInServingNetworkEntity")
	if err != nil {
		return nil, err
	}
	switch {
	case isContext(alt, 0, false):
		return &SuperChargerInfo{SendSubscriberData: true}, nil
	case isContext(alt, 1, false):
		age, err := fixedOctets("subscriberDataStored", alt.Bytes, 1, maxAgeIndicatorLength)
		if err != nil {
			return nil, err
		}
		return &SuperChargerInfo{SubscriberDataAge: age}, nil
	}
	return nil, fmt.Errorf("superChargerSupportedInServingNetworkEntity: unknown alternative [%d]", alt.Tag)
}

// decodeADDInfo reads ADD-Info: SEQUENCE { imeisv [0] IMEI,
// skipSubscriberDataUpdate [1] NULL OPTIONAL, … }.
func decodeADDInfo(b []byte, arg *UpdateLocationArg) error {
	fields, err := berChildren(b)
	if err != nil {
		return fmt.Errorf("add-info: %w", err)
	}
	for _, f := range fields {
		switch {
		case isContext(f, 0, false):
			if arg.IMEISV, err = decodeIMEI(f.Bytes); err != nil {
				return fmt.Errorf("add-info: %w", err)
			}
		case isContext(f, 1, false):
			arg.SkipSubscriberDataUpdate = true
		}
	}
	if arg.IMEISV == "" {
		return errors.New("add-info: imeisv missing")
	}
	return nil
}

// EncodeUpdateLocationRes encodes UpdateLocationRes:
// SEQUENCE { hlr-Number ISDN-AddressString, … }.
func EncodeUpdateLocationRes(r *UpdateLocationRes) ([]byte, error) {
	if r == nil {
		return nil, errors.New("UpdateLocationRes: nil")
	}
	hlr, err := encodeISDNAddressString(&r.HLRNumber)
	if err != nil {
		return nil, fmt.Errorf("UpdateLocationRes: hlr-Number: %w", err)
	}
	return sequence(tlv(asn1.ClassUniversal, asn1.TagOctetString, false, hlr)), nil
}

// ----- cancelLocation (gateway → VLR) --------------------------------------

// EncodeCancelLocationArg encodes CancelLocationArg:
//
//	[3] SEQUENCE { identity Identity, cancellationType CancellationType
//	               OPTIONAL, extensionContainer OPTIONAL, … }
//	Identity ::= CHOICE { imsi IMSI, imsi-WithLMSI IMSI-WithLMSI }
//	IMSI-WithLMSI ::= SEQUENCE { imsi IMSI, lmsi LMSI, … }
//
// Under IMPLICIT TAGS the [3] replaces the SEQUENCE tag; the untagged
// CHOICE appears as the alternative itself.
func EncodeCancelLocationArg(r *CancelLocationArg) ([]byte, error) {
	if r == nil {
		return nil, errors.New("CancelLocationArg: nil")
	}
	imsi, err := encodeIMSI(r.IMSI)
	if err != nil {
		return nil, fmt.Errorf("CancelLocationArg: %w", err)
	}
	identity := tlv(asn1.ClassUniversal, asn1.TagOctetString, false, imsi)
	if r.LMSI != nil {
		if len(r.LMSI) != lmsiLength {
			return nil, fmt.Errorf("CancelLocationArg: lmsi of %d octets, want %d", len(r.LMSI), lmsiLength)
		}
		identity = sequence(identity, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, r.LMSI))
	}
	content := identity
	if t := r.CancellationType; t != nil {
		if *t < CancellationUpdateProcedure || *t > CancellationInitialAttachProcedure {
			return nil, fmt.Errorf("CancelLocationArg: cancellationType %d", *t)
		}
		content = append(content, tlv(asn1.ClassUniversal, asn1.TagEnum, false, []byte{byte(*t)})...)
	}
	return tlv(asn1.ClassContextSpecific, 3, true, content), nil
}

// DecodeCancelLocationRes checks CancelLocationRes, SEQUENCE
// { extensionContainer OPTIONAL, … }. The result is optional: a
// ReturnResult without a parameter is a success too.
func DecodeCancelLocationRes(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if _, err := sequenceContent(b); err != nil {
		return fmt.Errorf("CancelLocationRes: %w", err)
	}
	return nil
}

// ----- provideRoamingNumber (gateway → VLR) --------------------------------

// EncodeProvideRoamingNumberArg encodes ProvideRoamingNumberArg in the
// order the ASN.1 declares its fields:
//
//	SEQUENCE { imsi [0] IMSI, msc-Number [1] ISDN-AddressString,
//	           msisdn [2] OPTIONAL, lmsi [4] OPTIONAL,
//	           gsm-BearerCapability [5] ExternalSignalInfo OPTIONAL,
//	           suppressionOfAnnouncement [7] NULL OPTIONAL,
//	           gmsc-Address [8] OPTIONAL, callReferenceNumber [9] OPTIONAL,
//	           or-Interrogation [10] NULL OPTIONAL, … }
func EncodeProvideRoamingNumberArg(r *ProvideRoamingNumberArg) ([]byte, error) {
	if r == nil {
		return nil, errors.New("ProvideRoamingNumberArg: nil")
	}
	wrap := func(err error) error { return fmt.Errorf("ProvideRoamingNumberArg: %w", err) }
	imsi, err := encodeIMSI(r.IMSI)
	if err != nil {
		return nil, wrap(err)
	}
	msc, err := encodeISDNAddressString(&r.MSCNumber)
	if err != nil {
		return nil, wrap(fmt.Errorf("msc-Number: %w", err))
	}
	fields := [][]byte{
		tlv(asn1.ClassContextSpecific, 0, false, imsi),
		tlv(asn1.ClassContextSpecific, 1, false, msc),
	}
	if r.MSISDN != nil {
		m, err := encodeISDNAddressString(r.MSISDN)
		if err != nil {
			return nil, wrap(fmt.Errorf("msisdn: %w", err))
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 2, false, m))
	}
	if r.LMSI != nil {
		if len(r.LMSI) != lmsiLength {
			return nil, wrap(fmt.Errorf("lmsi of %d octets, want %d", len(r.LMSI), lmsiLength))
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 4, false, r.LMSI))
	}
	if s := r.GSMBearerCapability; s != nil {
		if s.ProtocolID < 1 || s.ProtocolID > maxProtocolID {
			return nil, wrap(fmt.Errorf("gsm-BearerCapability: protocolId %d", s.ProtocolID))
		}
		if len(s.SignalInfo) < 1 || len(s.SignalInfo) > maxSignalInfoLength {
			return nil, wrap(fmt.Errorf("gsm-BearerCapability: signalInfo of %d octets, want 1..%d", len(s.SignalInfo), maxSignalInfoLength))
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 5, true, concatBytes(
			tlv(asn1.ClassUniversal, asn1.TagEnum, false, []byte{byte(s.ProtocolID)}),
			tlv(asn1.ClassUniversal, asn1.TagOctetString, false, s.SignalInfo))))
	}
	if r.SuppressionOfAnnouncement {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 7, false, nil))
	}
	if r.GMSCAddress != nil {
		g, err := encodeISDNAddressString(r.GMSCAddress)
		if err != nil {
			return nil, wrap(fmt.Errorf("gmsc-Address: %w", err))
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 8, false, g))
	}
	if r.CallReferenceNumber != nil {
		if len(r.CallReferenceNumber) < 1 || len(r.CallReferenceNumber) > maxCallReferenceLength {
			return nil, wrap(fmt.Errorf("callReferenceNumber of %d octets, want 1..%d", len(r.CallReferenceNumber), maxCallReferenceLength))
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 9, false, r.CallReferenceNumber))
	}
	if r.ORInterrogation {
		fields = append(fields, tlv(asn1.ClassContextSpecific, 10, false, nil))
	}
	return sequence(fields...), nil
}

// DecodeProvideRoamingNumberRes decodes ProvideRoamingNumberRes:
//
//	SEQUENCE { roamingNumber ISDN-AddressString, extensionContainer
//	           OPTIONAL, …, releaseResourcesSupported NULL OPTIONAL,
//	           vmsc-Address ISDN-AddressString OPTIONAL }
//
// Both addresses are universal OCTET STRINGs: the first is the roaming
// number, one after it the VMSC address.
func DecodeProvideRoamingNumberRes(b []byte) (*ProvideRoamingNumberRes, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("ProvideRoamingNumberRes: %w", err)
	}
	if len(fields) == 0 || !isUniversal(fields[0], asn1.TagOctetString, false) {
		return nil, errors.New("ProvideRoamingNumberRes: roamingNumber missing")
	}
	rn, err := decodeISDNAddressString("roamingNumber", fields[0].Bytes)
	if err != nil {
		return nil, fmt.Errorf("ProvideRoamingNumberRes: %w", err)
	}
	res := &ProvideRoamingNumberRes{RoamingNumber: *rn}
	for _, f := range fields[1:] {
		switch {
		case isUniversal(f, asn1.TagNull, false):
			res.ReleaseResourcesSupported = true
		case isUniversal(f, asn1.TagOctetString, false) && res.VMSCAddress == nil:
			if res.VMSCAddress, err = decodeISDNAddressString("vmsc-Address", f.Bytes); err != nil {
				return nil, fmt.Errorf("ProvideRoamingNumberRes: %w", err)
			}
		}
	}
	return res, nil
}

// ----- helpers ---------------------------------------------------------------

func isContext(rv asn1.RawValue, tag int, compound bool) bool {
	return rv.Class == asn1.ClassContextSpecific && rv.Tag == tag && rv.IsCompound == compound
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// decodeNamedBits reads the content of a BIT STRING (X.690 §8.6: an octet
// of unused bits, then the bits, the first in the high-order bit) as the
// numbers of the bits set. The bit strings here have at most 32 named bits;
// longer ones are read up to their named bits' extent.
func decodeNamedBits(b []byte) ([]int, error) {
	if len(b) == 0 {
		return nil, errors.New("BIT STRING without content")
	}
	unused := int(b[0])
	if unused > 7 || (len(b) == 1 && unused != 0) {
		return nil, fmt.Errorf("BIT STRING with %d unused bits in %d octets", unused, len(b)-1)
	}
	data := b[1:]
	n := len(data)*8 - unused
	var out []int
	for i := 0; i < n; i++ {
		if data[i/8]&(0x80>>(i%8)) != 0 {
			out = append(out, i)
		}
	}
	return out, nil
}

// encodeNamedBits encodes the content of a BIT STRING with bits set at
// the given numbers, at least minBits long (the type's SIZE lower bound)
// and no longer than the highest bit set needs.
func encodeNamedBits(bits []int, minBits, maxBits int) ([]byte, error) {
	n := minBits
	for _, b := range bits {
		if b < 0 || b >= maxBits {
			return nil, fmt.Errorf("bit %d outside 0..%d", b, maxBits-1)
		}
		n = max(n, b+1)
	}
	octets := (n + 7) / 8
	out := make([]byte, 1+octets)
	out[0] = byte(octets*8 - n)
	for _, b := range bits {
		out[1+b/8] |= 0x80 >> (b % 8)
	}
	return out, nil
}
