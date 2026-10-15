// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// USSD-Arg and USSD-Res (TS 29.002 MAP-SS-DataTypes, IMPLICIT TAGS), the argument
// of processUnstructuredSS-Request (59), unstructuredSS-Request (60) and
// unstructuredSS-Notify (61), and the result of 59 and 60:
//
//	USSD-Arg ::= SEQUENCE { ussd-DataCodingScheme OCTET STRING (SIZE (1)),
//	                        ussd-String OCTET STRING (SIZE (1..160)), …,
//	                        alertingPattern AlertingPattern OPTIONAL,
//	                        msisdn [0] ISDN-AddressString OPTIONAL }
//	USSD-Res ::= SEQUENCE { ussd-DataCodingScheme, ussd-String, … }
//
// The first three are untagged OCTET STRINGs, told apart by position.

// USSDArg is USSD-Arg; it also holds a USSD-Res (no alerting pattern, no
// MSISDN).
type USSDArg struct {
	DCS    byte
	String []byte
	// AlertingPattern, when non-nil, is one octet (TS 29.002 AlertingPattern).
	AlertingPattern *byte
	// MSISDN, in a processUnstructuredSS-Request from the HLR, is the
	// subscriber's.
	MSISDN *AddressString
}

// EncodeUSSDArg encodes USSD-Arg.
func EncodeUSSDArg(a *USSDArg) ([]byte, error) {
	if err := checkUSSDString(a.String); err != nil {
		return nil, err
	}
	fields := [][]byte{
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{a.DCS}),
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, a.String),
	}
	if a.AlertingPattern != nil {
		fields = append(fields, tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{*a.AlertingPattern}))
	}
	if a.MSISDN != nil {
		b, err := encodeISDNAddressString(a.MSISDN)
		if err != nil {
			return nil, fmt.Errorf("USSD-Arg msisdn: %w", err)
		}
		fields = append(fields, tlv(asn1.ClassContextSpecific, 0, false, b))
	}
	return sequence(fields...), nil
}

// EncodeUSSDRes encodes USSD-Res.
func EncodeUSSDRes(dcs byte, str []byte) ([]byte, error) {
	if err := checkUSSDString(str); err != nil {
		return nil, err
	}
	return sequence(
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, []byte{dcs}),
		tlv(asn1.ClassUniversal, asn1.TagOctetString, false, str),
	), nil
}

// DecodeUSSDArg decodes USSD-Arg, or USSD-Res (which is its first two
// fields).
func DecodeUSSDArg(b []byte) (*USSDArg, error) {
	fields, err := sequenceContent(b)
	if err != nil {
		return nil, fmt.Errorf("USSD-Arg: %w", err)
	}
	a := &USSDArg{}
	octets := 0
	for _, f := range fields {
		switch {
		case isUniversal(f, asn1.TagOctetString, false):
			switch octets {
			case 0:
				if len(f.Bytes) != 1 {
					return nil, fmt.Errorf("USSD-Arg: ussd-DataCodingScheme of %d octets", len(f.Bytes))
				}
				a.DCS = f.Bytes[0]
			case 1:
				if err := checkUSSDString(f.Bytes); err != nil {
					return nil, err
				}
				a.String = append([]byte(nil), f.Bytes...)
			case 2:
				if len(f.Bytes) != 1 {
					return nil, fmt.Errorf("USSD-Arg: alertingPattern of %d octets", len(f.Bytes))
				}
				p := f.Bytes[0]
				a.AlertingPattern = &p
			}
			octets++
		case f.Class == asn1.ClassContextSpecific && f.Tag == 0 && !f.IsCompound:
			if a.MSISDN, err = decodeISDNAddressString("msisdn", f.Bytes); err != nil {
				return nil, fmt.Errorf("USSD-Arg: %w", err)
			}
		}
	}
	if octets < 2 {
		return nil, errors.New("USSD-Arg: ussd-DataCodingScheme or ussd-String missing")
	}
	return a, nil
}

func checkUSSDString(s []byte) error {
	if len(s) < 1 || len(s) > MaxUSSDStringLength {
		return fmt.Errorf("ussd-String of %d octets, want 1..%d", len(s), MaxUSSDStringLength)
	}
	return nil
}
