// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// UserError is an operation's error as the gateway sends it: its code and,
// for the two errors whose parameter qualifies them here, the cause (TS
// 29.002 §7.6.1, MAP-ER-DataTypes).
type UserError struct {
	Code  ErrorCode
	Cause ErrorCause
}

// ErrorCause qualifies an error. unknownSubscriber's diagnostic is
// optional (its parameter is); roamingNotAllowed's cause is not — its
// parameter is mandatory, and without a cause it is plmnRoamingNotAllowed,
// the default TS 29.002 §8.1.2.3 names for an unqualified one.
type ErrorCause uint8

const (
	CauseNone ErrorCause = iota
	// unknownSubscriber, UnknownSubscriberDiagnostic.
	CauseIMSIUnknown                // imsiUnknown (0)
	CauseGPRSEPSSubscriptionUnknown // gprs-eps-SubscriptionUnknown (1)
	// roamingNotAllowed, RoamingNotAllowedCause and
	// AdditionalRoamingNotAllowedCause.
	CausePLMNRoamingNotAllowed       // plmnRoamingNotAllowed (0)
	CauseOperatorDeterminedBarring   // operatorDeterminedBarring (3)
	CauseSupportedRATTypesNotAllowed // additional: supportedRAT-TypesNotAllowed (0)
)

var errorCauseNames = map[ErrorCause]string{
	CauseNone:        "none",
	CauseIMSIUnknown: "imsiUnknown", CauseGPRSEPSSubscriptionUnknown: "gprs-eps-SubscriptionUnknown",
	CausePLMNRoamingNotAllowed: "plmnRoamingNotAllowed", CauseOperatorDeterminedBarring: "operatorDeterminedBarring",
	CauseSupportedRATTypesNotAllowed: "supportedRAT-TypesNotAllowed",
}

func (c ErrorCause) String() string {
	if n, ok := errorCauseNames[c]; ok {
		return n
	}
	return fmt.Sprintf("cause(%d)", uint8(c))
}

// Parameter returns the error's parameter, nil when it goes without one:
//
//	UnknownSubscriberParam ::= SEQUENCE {
//	    extensionContainer ExtensionContainer OPTIONAL, ...,
//	    unknownSubscriberDiagnostic UnknownSubscriberDiagnostic OPTIONAL }
//	RoamingNotAllowedParam ::= SEQUENCE {
//	    roamingNotAllowedCause RoamingNotAllowedCause,
//	    extensionContainer ExtensionContainer OPTIONAL, ...,
//	    additionalRoamingNotAllowedCause [0] AdditionalRoamingNotAllowedCause OPTIONAL }
//
// A receiver that gets the additional cause discards the other, which goes
// as plmnRoamingNotAllowed.
func (e UserError) Parameter() ([]byte, error) {
	enum := func(v byte) []byte { return tlv(asn1.ClassUniversal, asn1.TagEnum, false, []byte{v}) }
	switch {
	case e.Cause == CauseNone && e.Code != ErrRoamingNotAllowed:
		return nil, nil
	case e.Code == ErrUnknownSubscriber && e.Cause == CauseIMSIUnknown:
		return sequence(enum(0)), nil
	case e.Code == ErrUnknownSubscriber && e.Cause == CauseGPRSEPSSubscriptionUnknown:
		return sequence(enum(1)), nil
	case e.Code == ErrRoamingNotAllowed && (e.Cause == CauseNone || e.Cause == CausePLMNRoamingNotAllowed):
		return sequence(enum(0)), nil
	case e.Code == ErrRoamingNotAllowed && e.Cause == CauseOperatorDeterminedBarring:
		return sequence(enum(3)), nil
	case e.Code == ErrRoamingNotAllowed && e.Cause == CauseSupportedRATTypesNotAllowed:
		return sequence(enum(0), tlv(asn1.ClassContextSpecific, 0, false, []byte{0})), nil
	}
	return nil, fmt.Errorf("%s does not qualify %s", e.Cause, e.Code)
}

// DecodeUserErrorCause reads the cause in the parameter of a ReturnError
// of code: what a VLR makes of the gateway's error. roamingNotAllowed
// without its parameter is an error; a diagnostic or additional cause of a
// value this release does not know is discarded, as the types' extension
// markers require.
func DecodeUserErrorCause(code ErrorCode, param []byte) (ErrorCause, error) {
	switch code {
	case ErrUnknownSubscriber:
		if len(param) == 0 {
			return CauseNone, nil
		}
		fields, err := sequenceContent(param)
		if err != nil {
			return 0, fmt.Errorf("UnknownSubscriberParam: %w", err)
		}
		// extensionContainer, then the diagnostic — an extension addition,
		// as anything after it would be: ignored.
		if len(fields) > 0 && isUniversal(fields[0], asn1.TagSequence, true) {
			fields = fields[1:]
		}
		cause := CauseNone
		if len(fields) > 0 && isUniversal(fields[0], asn1.TagEnum, false) {
			v, err := enumValue(fields[0].Bytes)
			if err != nil {
				return 0, fmt.Errorf("UnknownSubscriberParam: %w", err)
			}
			switch v {
			case 0:
				cause = CauseIMSIUnknown
			case 1:
				cause = CauseGPRSEPSSubscriptionUnknown
			} // other values are discarded, as the type says
		}
		return cause, nil
	case ErrRoamingNotAllowed:
		if len(param) == 0 {
			return 0, errors.New("RoamingNotAllowedParam missing, and it is mandatory")
		}
		fields, err := sequenceContent(param)
		if err != nil {
			return 0, fmt.Errorf("RoamingNotAllowedParam (mandatory): %w", err)
		}
		if len(fields) == 0 || !isUniversal(fields[0], asn1.TagEnum, false) {
			return 0, errors.New("RoamingNotAllowedParam: no roamingNotAllowedCause")
		}
		v, err := enumValue(fields[0].Bytes)
		if err != nil {
			return 0, fmt.Errorf("RoamingNotAllowedParam: %w", err)
		}
		cause := CausePLMNRoamingNotAllowed
		switch v {
		case 0:
		case 3:
			cause = CauseOperatorDeterminedBarring
		default: // RoamingNotAllowedCause has no extension marker
			return 0, fmt.Errorf("RoamingNotAllowedParam: roamingNotAllowedCause %d", v)
		}
		// extensionContainer, then the additional cause [0] — an extension
		// addition, as anything after it would be: ignored.
		rest := fields[1:]
		if len(rest) > 0 && isUniversal(rest[0], asn1.TagSequence, true) {
			rest = rest[1:]
		}
		if len(rest) > 0 && isContext(rest[0], 0, false) {
			v, err := enumValue(rest[0].Bytes)
			if err != nil {
				return 0, fmt.Errorf("RoamingNotAllowedParam: additional cause: %w", err)
			}
			if v == 0 {
				cause = CauseSupportedRATTypesNotAllowed
			}
		}
		return cause, nil
	}
	return CauseNone, nil // another error's parameter: nothing here reads it
}

// enumValue reads a non-negative ENUMERATED of at most four octets.
func enumValue(b []byte) (int, error) {
	if len(b) == 0 || len(b) > 4 || b[0]&0x80 != 0 {
		return 0, fmt.Errorf("ENUMERATED of %d octets or negative", len(b))
	}
	v := 0
	for _, o := range b {
		v = v<<8 | int(o)
	}
	return v, nil
}
