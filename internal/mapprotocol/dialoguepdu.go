// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"fmt"
)

// The MAP dialogue PDU (TS 29.002 §17.4, module MAP-DialogueInformation,
// IMPLICIT TAGS), carried in a TCAP dialogue portion's user-information as
// an EXTERNAL under map-DialogueAS. Only MAP-OpenInfo is used: its
// destination reference names the subscriber of a USSD dialogue between
// HLR and gsmSCF (TS 29.002 table 7.3/2, notes 1 and 2), its origination
// reference the gsmSCF.

// mapDialogueAS is {gsm-NetworkId as-Id map-DialoguePDU (1) version1 (1)}:
// gsm-NetworkId 0.4.0.0.1, as-Id 1 (ETSI ETR 091, MobileDomainDefinitions).
var mapDialogueAS = asn1.ObjectIdentifier{0, 4, 0, 0, 1, 1, 1, 1}

// MAPOpenInfo is MAP-OpenInfo: either reference may be absent.
type MAPOpenInfo struct {
	DestinationReference *AddressString
	OriginationReference *AddressString
}

// EncodeMAPOpenInfo encodes the content of a TCAP user-information [30]:
// one EXTERNAL { map-DialogueAS, single-ASN1-type [0] MAP-DialoguePDU
// map-open [0] { destinationReference [0], originationReference [1] } }.
func EncodeMAPOpenInfo(o *MAPOpenInfo) ([]byte, error) {
	var fields []byte
	for i, a := range []*AddressString{o.DestinationReference, o.OriginationReference} {
		if a == nil {
			continue
		}
		v, err := encodeAddressString(a)
		if err != nil {
			return nil, fmt.Errorf("MAP-OpenInfo reference [%d]: %w", i, err)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, i, false, v)...)
	}
	open := tlv(asn1.ClassContextSpecific, 0, true, fields) // map-open [0], implicitly a SEQUENCE
	as, _ := asn1.Marshal(mapDialogueAS)
	external := append(as, tlv(asn1.ClassContextSpecific, 0, true, open)...) // single-ASN1-type [0]
	return tlv(asn1.ClassUniversal, 8, true, external), nil                  // EXTERNAL
}

// DecodeMAPOpenInfo finds MAP-OpenInfo in a TCAP user-information's content
// (a sequence of EXTERNALs). It reports false when none is a MAP dialogue
// PDU's map-open.
func DecodeMAPOpenInfo(userInfo []byte) (*MAPOpenInfo, bool, error) {
	externals, err := berChildren(userInfo)
	if err != nil {
		return nil, false, fmt.Errorf("user-information: %w", err)
	}
	for _, ext := range externals {
		if ext.Class != asn1.ClassUniversal || ext.Tag != 8 || !ext.IsCompound {
			continue
		}
		parts, err := berChildren(ext.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("EXTERNAL: %w", err)
		}
		var as asn1.ObjectIdentifier
		var pdu *asn1.RawValue
		for i := range parts {
			p := parts[i]
			switch {
			case isUniversal(p, asn1.TagOID, false):
				if _, err := asn1.Unmarshal(p.FullBytes, &as); err != nil {
					return nil, false, fmt.Errorf("EXTERNAL direct-reference: %w", err)
				}
			case p.Class == asn1.ClassContextSpecific && p.Tag == 0 && p.IsCompound:
				inner, err := onlyChild(p.Bytes, "single-ASN1-type")
				if err != nil {
					return nil, false, err
				}
				pdu = &inner
			}
		}
		if !as.Equal(mapDialogueAS) || pdu == nil {
			continue
		}
		if pdu.Class != asn1.ClassContextSpecific || pdu.Tag != 0 || !pdu.IsCompound {
			continue // another MAP dialogue PDU (accept, close, refuse, aborts)
		}
		fields, err := berChildren(pdu.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("MAP-OpenInfo: %w", err)
		}
		o := &MAPOpenInfo{}
		for _, f := range fields {
			if f.Class != asn1.ClassContextSpecific || f.IsCompound {
				continue // extension container
			}
			a, err := decodeAddressString(f.Bytes)
			if err != nil {
				return nil, false, fmt.Errorf("MAP-OpenInfo reference [%d]: %w", f.Tag, err)
			}
			switch f.Tag {
			case 0:
				o.DestinationReference = a
			case 1:
				o.OriginationReference = a
			}
		}
		return o, true, nil
	}
	return nil, false, nil
}
