// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// Authentication and MS purging with the gateway as HLR (TS 29.002 §8.5.2,
// §8.1.6): a VLR or SGSN asks for authentication vectors with
// sendAuthenticationInfo (56) under infoRetrievalContext-v3, and tells the
// HLR it deleted a subscriber's record with purgeMS (67) under
// msPurgingContext-v3. The vectors come from the application (the AuC);
// the gateway carries them.

// OpSendAuthenticationInfo is sendAuthenticationInfo (TS 29.002 §17.6.4).
const OpSendAuthenticationInfo OperationCode = 56

// OpPurgeMS is purgeMS (TS 29.002 §17.6.1); the same code as
// OpPurgeMSRequest.
const OpPurgeMS = OpPurgeMSRequest

// The errors sendAuthenticationInfo and purgeMS allow (TS 29.002 §17.6.1,
// §17.6.4). purgeMS has no systemFailure.
var (
	SendAuthenticationInfoErrors = map[ErrorCode]bool{
		ErrSystemFailure: true, ErrDataMissing: true, ErrUnexpectedDataValue: true, ErrUnknownSubscriber: true,
	}
	PurgeMSErrors = map[ErrorCode]bool{
		ErrDataMissing: true, ErrUnexpectedDataValue: true, ErrUnknownSubscriber: true,
	}
)

// Octet lengths of the authentication types (TS 29.002 MAP-MS-DataTypes).
const (
	RANDLength     = 16
	SRESLength     = 4
	KcLength       = 8
	XRESMinLength  = 4
	XRESMaxLength  = 16
	CKLength       = 16
	IKLength       = 16
	AUTNLength     = 16
	AUTSLength     = 14
	KASMELength    = 32
	PLMNIDLength   = 3
	UEUsageTypeLen = 4
	// MaxVectors is the SIZE bound of each vector list and the range of
	// NumberOfRequestedVectors.
	MaxVectors = 5
)

// RequestingNodeType (TS 29.002): the node asking for vectors.
type RequestingNodeType int

const (
	NodeVLR           RequestingNodeType = 0
	NodeSGSN          RequestingNodeType = 1
	NodeSCSCF         RequestingNodeType = 2
	NodeBSF           RequestingNodeType = 3
	NodeGANAAAServer  RequestingNodeType = 4
	NodeWLANAAAServer RequestingNodeType = 5
	NodeMME           RequestingNodeType = 16
	NodeMMESGSN       RequestingNodeType = 17
)

var requestingNodeTypeNames = map[RequestingNodeType]string{
	NodeVLR: "vlr", NodeSGSN: "sgsn", NodeSCSCF: "s-cscf", NodeBSF: "bsf",
	NodeGANAAAServer: "gan-aaa-server", NodeWLANAAAServer: "wlan-aaa-server",
	NodeMME: "mme", NodeMMESGSN: "mme-sgsn",
}

func (t RequestingNodeType) String() string {
	if s, ok := requestingNodeTypeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("requestingNodeType(%d)", int(t))
}

// requestingNodeType applies the type's exception handling: values 6..15
// are taken as vlr, values above 17 as sgsn.
func requestingNodeType(v int) RequestingNodeType {
	switch {
	case v >= 6 && v <= 15:
		return NodeVLR
	case v > 17:
		return NodeSGSN
	}
	return RequestingNodeType(v)
}

// SendAuthenticationInfoArg is what a VLR or SGSN sends (TS 29.002 §8.5.2).
type SendAuthenticationInfoArg struct {
	IMSI                       IMSI
	NumberOfRequestedVectors   int
	SegmentationProhibited     bool
	ImmediateResponsePreferred bool
	// ResyncRAND and ResyncAUTS: re-synchronisationInfo (TS 33.102
	// §6.3.5), both or neither.
	ResyncRAND, ResyncAUTS []byte
	RequestingNodeType     *RequestingNodeType
	RequestingPLMNID       []byte
	// AdditionalVectors: numberOfRequestedAdditional-Vectors (0 absent).
	AdditionalVectors          int
	AdditionalVectorsAreForEPS bool
	UEUsageTypeRequested       bool
}

// AuthenticationTriplet is a GSM vector.
type AuthenticationTriplet struct{ RAND, SRES, Kc []byte }

// AuthenticationQuintuplet is a UMTS vector.
type AuthenticationQuintuplet struct{ RAND, XRES, CK, IK, AUTN []byte }

// EPCAV is an EPS vector.
type EPCAV struct{ RAND, XRES, AUTN, KASME []byte }

// SendAuthenticationInfoRes is the HLR's answer. Triplets and Quintuplets
// are the two alternatives of authenticationSetList: at most one is set.
// All empty is the empty response (TS 29.002 §8.5.2.1: the HLR has no
// vectors to give).
type SendAuthenticationInfoRes struct {
	Triplets    []AuthenticationTriplet
	Quintuplets []AuthenticationQuintuplet
	EPSVectors  []EPCAV
	UEUsageType []byte
}

// DecodeSendAuthenticationInfoArg decodes
//
//	SendAuthenticationInfoArg ::= SEQUENCE {
//	  imsi [0] IMSI, numberOfRequestedVectors NumberOfRequestedVectors,
//	  segmentationProhibited NULL OPTIONAL,
//	  immediateResponsePreferred [1] NULL OPTIONAL,
//	  re-synchronisationInfo Re-synchronisationInfo OPTIONAL,
//	  extensionContainer [2] OPTIONAL, …,
//	  requestingNodeType [3] OPTIONAL, requestingPLMN-Id [4] OPTIONAL,
//	  numberOfRequestedAdditional-Vectors [5] OPTIONAL,
//	  additionalVectorsAreForEPS [6] NULL OPTIONAL,
//	  ueUsageTypeRequestIndication [7] NULL OPTIONAL }
//
// Elements after the extension marker that it does not know are skipped.
func DecodeSendAuthenticationInfoArg(b []byte) (*SendAuthenticationInfoArg, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
	}
	if len(fields) < 2 || !isContext(fields[0], 0, false) || !isUniversal(fields[1], asn1.TagInteger, false) {
		return nil, errors.New("SendAuthenticationInfoArg: imsi [0] and numberOfRequestedVectors required, in that order")
	}
	arg := &SendAuthenticationInfoArg{}
	if arg.IMSI, err = decodeIMSI(fields[0].Bytes); err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
	}
	if arg.NumberOfRequestedVectors, err = vectorCount("numberOfRequestedVectors", fields[1].Bytes); err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
	}
	for _, f := range fields[2:] {
		switch {
		case isUniversal(f, asn1.TagNull, false):
			arg.SegmentationProhibited = true
		case isUniversal(f, asn1.TagSequence, true):
			if arg.ResyncRAND, arg.ResyncAUTS, err = decodeResync(f.Bytes); err != nil {
				return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
			}
		case isContext(f, 1, false):
			arg.ImmediateResponsePreferred = true
		case isContext(f, 3, false):
			v, err := berInt(f.Bytes)
			if err != nil {
				return nil, fmt.Errorf("SendAuthenticationInfoArg: requestingNodeType: %w", err)
			}
			if v < 0 {
				return nil, fmt.Errorf("SendAuthenticationInfoArg: requestingNodeType %d", v)
			}
			t := requestingNodeType(v)
			arg.RequestingNodeType = &t
		case isContext(f, 4, false):
			if arg.RequestingPLMNID, err = fixedOctets("requestingPLMN-Id", f.Bytes, PLMNIDLength, PLMNIDLength); err != nil {
				return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
			}
		case isContext(f, 5, false):
			if arg.AdditionalVectors, err = vectorCount("numberOfRequestedAdditional-Vectors", f.Bytes); err != nil {
				return nil, fmt.Errorf("SendAuthenticationInfoArg: %w", err)
			}
		case isContext(f, 6, false):
			arg.AdditionalVectorsAreForEPS = true
		case isContext(f, 7, false):
			arg.UEUsageTypeRequested = true
		}
	}
	return arg, nil
}

// VectorLimits are how many vectors of each kind an answer to a may
// carry (TS 29.002 §8.5.2.1, §8.5.2.3): an MME gets EPS vectors only, the
// number it asked for; a combined MME/SGSN gets the number it asked for of
// the kind for immediate use and the additional number of the other kind
// — EPS when additionalVectorsAreForEPS, else triplets or quintuplets; any
// other node, or a request without a node type, gets triplets or
// quintuplets only.
func (a *SendAuthenticationInfoArg) VectorLimits() (set, eps int) {
	n := a.NumberOfRequestedVectors
	switch t := a.RequestingNodeType; {
	case t != nil && *t == NodeMME:
		return 0, n
	case t != nil && *t == NodeMMESGSN:
		if a.AdditionalVectorsAreForEPS {
			return n, a.AdditionalVectors
		}
		return a.AdditionalVectors, n
	}
	return n, 0
}

func vectorCount(name string, b []byte) (int, error) {
	n, err := berInt(b)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if n < 1 || n > MaxVectors {
		return 0, fmt.Errorf("%s %d outside 1..%d", name, n, MaxVectors)
	}
	return n, nil
}

// decodeResync reads Re-synchronisationInfo ::= SEQUENCE { rand RAND,
// auts AUTS, … }.
func decodeResync(b []byte) (rand, auts []byte, err error) {
	fields, err := berChildren(b)
	if err != nil {
		return nil, nil, fmt.Errorf("re-synchronisationInfo: %w", err)
	}
	if len(fields) < 2 || !isUniversal(fields[0], asn1.TagOctetString, false) ||
		!isUniversal(fields[1], asn1.TagOctetString, false) {
		return nil, nil, errors.New("re-synchronisationInfo: rand and auts required")
	}
	if rand, err = fixedOctets("re-synchronisationInfo rand", fields[0].Bytes, RANDLength, RANDLength); err != nil {
		return nil, nil, err
	}
	if auts, err = fixedOctets("re-synchronisationInfo auts", fields[1].Bytes, AUTSLength, AUTSLength); err != nil {
		return nil, nil, err
	}
	return rand, auts, nil
}

// Validate checks r against the type and against arg, the request it
// answers: one kind of authenticationSetList, each list of at most five,
// no more vectors than asked for (TS 29.002 §8.5.2.3), and a UE usage type
// only when asked for.
func (r *SendAuthenticationInfoRes) Validate(arg *SendAuthenticationInfoArg) error {
	if len(r.Triplets) > 0 && len(r.Quintuplets) > 0 {
		return errors.New("triplets and quintuplets: authenticationSetList is one or the other")
	}
	for name, n := range map[string]int{"triplets": len(r.Triplets), "quintuplets": len(r.Quintuplets), "EPS vectors": len(r.EPSVectors)} {
		if n > MaxVectors {
			return fmt.Errorf("%d %s, a list holds at most %d", n, name, MaxVectors)
		}
	}
	if arg != nil {
		set, eps := arg.VectorLimits()
		if n := len(r.Triplets) + len(r.Quintuplets); n > set {
			return fmt.Errorf("%d triplets or quintuplets, the request allows %d", n, set)
		}
		if n := len(r.EPSVectors); n > eps {
			return fmt.Errorf("%d EPS vectors, the request allows %d", n, eps)
		}
		if r.UEUsageType != nil && !arg.UEUsageTypeRequested {
			return errors.New("a UE usage type the request did not ask for")
		}
	}
	for i, v := range r.Triplets {
		if err := octetLengths(fmt.Sprintf("triplet %d", i), []octets{{"rand", v.RAND, RANDLength, RANDLength},
			{"sres", v.SRES, SRESLength, SRESLength}, {"kc", v.Kc, KcLength, KcLength}}); err != nil {
			return err
		}
	}
	for i, v := range r.Quintuplets {
		if err := octetLengths(fmt.Sprintf("quintuplet %d", i), []octets{{"rand", v.RAND, RANDLength, RANDLength},
			{"xres", v.XRES, XRESMinLength, XRESMaxLength}, {"ck", v.CK, CKLength, CKLength},
			{"ik", v.IK, IKLength, IKLength}, {"autn", v.AUTN, AUTNLength, AUTNLength}}); err != nil {
			return err
		}
	}
	for i, v := range r.EPSVectors {
		if err := octetLengths(fmt.Sprintf("EPS vector %d", i), []octets{{"rand", v.RAND, RANDLength, RANDLength},
			{"xres", v.XRES, XRESMinLength, XRESMaxLength}, {"autn", v.AUTN, AUTNLength, AUTNLength},
			{"kasme", v.KASME, KASMELength, KASMELength}}); err != nil {
			return err
		}
	}
	if r.UEUsageType != nil && len(r.UEUsageType) != UEUsageTypeLen {
		return fmt.Errorf("ue usage type of %d octets, want %d", len(r.UEUsageType), UEUsageTypeLen)
	}
	return nil
}

type octets struct {
	name     string
	b        []byte
	min, max int
}

func octetLengths(what string, fields []octets) error {
	for _, f := range fields {
		if len(f.b) < f.min || len(f.b) > f.max {
			if f.min == f.max {
				return fmt.Errorf("%s: %s of %d octets, want %d", what, f.name, len(f.b), f.min)
			}
			return fmt.Errorf("%s: %s of %d octets, want %d..%d", what, f.name, len(f.b), f.min, f.max)
		}
	}
	return nil
}

// EncodeSendAuthenticationInfoRes encodes
//
//	SendAuthenticationInfoRes ::= [3] SEQUENCE {
//	  authenticationSetList AuthenticationSetList OPTIONAL,
//	  extensionContainer OPTIONAL, …,
//	  eps-AuthenticationSetList [2] EPS-AuthenticationSetList OPTIONAL,
//	  ueUsageType [3] UE-UsageType OPTIONAL }
//	AuthenticationSetList ::= CHOICE { tripletList [0] TripletList,
//	                                   quintupletList [1] QuintupletList }
//
// under IMPLICIT TAGS: [3] replaces the SEQUENCE tag, [0], [1] and [2]
// the SEQUENCE OF tags. The empty response is A3 00. It checks the type,
// not the request: Validate does that.
func EncodeSendAuthenticationInfoRes(r *SendAuthenticationInfoRes) ([]byte, error) {
	if r == nil {
		return nil, errors.New("SendAuthenticationInfoRes: nil")
	}
	if err := r.Validate(nil); err != nil {
		return nil, fmt.Errorf("SendAuthenticationInfoRes: %w", err)
	}
	octet := func(b []byte) []byte { return tlv(asn1.ClassUniversal, asn1.TagOctetString, false, b) }
	var content []byte
	switch {
	case len(r.Triplets) > 0:
		var list []byte
		for _, v := range r.Triplets {
			list = append(list, sequence(octet(v.RAND), octet(v.SRES), octet(v.Kc))...)
		}
		content = tlv(asn1.ClassContextSpecific, 0, true, list)
	case len(r.Quintuplets) > 0:
		var list []byte
		for _, v := range r.Quintuplets {
			list = append(list, sequence(octet(v.RAND), octet(v.XRES), octet(v.CK), octet(v.IK), octet(v.AUTN))...)
		}
		content = tlv(asn1.ClassContextSpecific, 1, true, list)
	}
	if len(r.EPSVectors) > 0 {
		var list []byte
		for _, v := range r.EPSVectors {
			list = append(list, sequence(octet(v.RAND), octet(v.XRES), octet(v.AUTN), octet(v.KASME))...)
		}
		content = append(content, tlv(asn1.ClassContextSpecific, 2, true, list)...)
	}
	if r.UEUsageType != nil {
		content = append(content, tlv(asn1.ClassContextSpecific, 3, false, r.UEUsageType)...)
	}
	return tlv(asn1.ClassContextSpecific, 3, true, content), nil
}

// ----- purgeMS (VLR/SGSN → gateway as HLR) ---------------------------------

// PurgeMSArg is what a VLR or SGSN sends (TS 29.002 §8.1.6). The last
// known location (locationInformation [2], [3], [4]) is not decoded.
type PurgeMSArg struct {
	IMSI       IMSI
	VLRNumber  *AddressString
	SGSNNumber *AddressString
}

// PurgeMSRes is the HLR's answer: which temporary identities the VLR,
// SGSN or IWF is to freeze.
type PurgeMSRes struct {
	FreezeTMSI, FreezePTMSI, FreezeMTMSI bool
}

// DecodePurgeMSArg decodes
//
//	PurgeMS-Arg ::= [3] SEQUENCE { imsi IMSI,
//	  vlr-Number [0] ISDN-AddressString OPTIONAL,
//	  sgsn-Number [1] ISDN-AddressString OPTIONAL,
//	  extensionContainer OPTIONAL, …, locationInformation [2] OPTIONAL,
//	  locationInformationGPRS [3] OPTIONAL, locationInformationEPS [4] OPTIONAL }
func DecodePurgeMSArg(b []byte) (*PurgeMSArg, error) {
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(b, &outer)
	if err != nil {
		return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
	}
	if len(rest) > 0 || !isContext(outer, 3, true) {
		return nil, errors.New("PurgeMS-Arg: not one [3] SEQUENCE")
	}
	fields, err := berChildren(outer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
	}
	if len(fields) == 0 || !isUniversal(fields[0], asn1.TagOctetString, false) {
		return nil, errors.New("PurgeMS-Arg: imsi required first")
	}
	arg := &PurgeMSArg{}
	if arg.IMSI, err = decodeIMSI(fields[0].Bytes); err != nil {
		return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
	}
	for _, f := range fields[1:] {
		switch {
		case isContext(f, 0, false):
			if arg.VLRNumber, err = decodeISDNAddressString("vlr-Number", f.Bytes); err != nil {
				return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
			}
		case isContext(f, 1, false):
			if arg.SGSNNumber, err = decodeISDNAddressString("sgsn-Number", f.Bytes); err != nil {
				return nil, fmt.Errorf("PurgeMS-Arg: %w", err)
			}
		}
	}
	return arg, nil
}

// EncodePurgeMSRes encodes PurgeMS-Res ::= SEQUENCE { freezeTMSI [0] NULL
// OPTIONAL, freezeP-TMSI [1] NULL OPTIONAL, extensionContainer OPTIONAL,
// …, freezeM-TMSI [2] NULL OPTIONAL }. Nothing to freeze is 30 00.
func EncodePurgeMSRes(r *PurgeMSRes) ([]byte, error) {
	if r == nil {
		return nil, errors.New("PurgeMS-Res: nil")
	}
	var content []byte
	for i, set := range []bool{r.FreezeTMSI, r.FreezePTMSI, r.FreezeMTMSI} {
		if set {
			content = append(content, tlv(asn1.ClassContextSpecific, i, false, nil)...)
		}
	}
	return tlv(asn1.ClassUniversal, asn1.TagSequence, true, content), nil
}
