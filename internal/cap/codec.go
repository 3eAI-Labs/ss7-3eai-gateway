// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap

import (
	"encoding/asn1"
	"errors"
	"fmt"
)

// ASN.1 BER codec for CAP operations (3GPP TS 29.078, CAP-DataTypes).
//
// Scope:
//   - Mandatory IEs of in-scope ops decoded into typed *Request structs.
//   - CAP responses are intentionally not encoded here: in CAMEL phase 3/4
//     the gsmSCF→gsmSSF response path is a sequence of separate Invokes
//     (Connect, Continue, ReleaseCall, RequestReportBCSMEvent) rather than
//     a ReturnResult on the InitialDP. Those send paths are stubbed in
//     handler.go and land with the SCP integration slice.
//
// Wire-format anchors:
//   - 3GPP TS 29.078 §11 (CAP-DataTypes / CAP-classes)
//   - ITU-T Q.713 Annex E — TBCD digit packing for AddressString
//   - ITU-T X.690 — BER/DER tag/length/value rules
//
// CAP-DataTypes is declared `IMPLICIT TAGS ::=` so context-tagged fields
// `[N] X` appear as primitive `[CONTEXT N]` octets — matching
// `asn1:"tag:N,implicit"` in encoding/asn1.

var (
	errCodecShortBuffer = errors.New("cap: ASN.1 buffer too short")
	errCodecBadTBCD     = errors.New("cap: invalid TBCD digit")
	errCodecBadAddrLen  = errors.New("cap: AddressString length out of range")
)

// ----- TBCD / AddressString (Q.713 Annex E) --------------------------------

func decodeTBCD(b []byte) (string, error) {
	out := make([]byte, 0, len(b)*2)
	for i, octet := range b {
		lo, hi := octet&0x0F, (octet>>4)&0x0F
		d, err := tbcdDigit(lo)
		if err != nil {
			return "", err
		}
		out = append(out, d)
		if hi == 0x0F {
			if i != len(b)-1 {
				return "", errCodecBadTBCD
			}
			break
		}
		d, err = tbcdDigit(hi)
		if err != nil {
			return "", err
		}
		out = append(out, d)
	}
	return string(out), nil
}

func tbcdDigit(n byte) (byte, error) {
	switch {
	case n <= 9:
		return '0' + n, nil
	case n == 10:
		return '*', nil
	case n == 11:
		return '#', nil
	case n == 12:
		return 'a', nil
	case n == 13:
		return 'b', nil
	case n == 14:
		return 'c', nil
	}
	return 0, errCodecBadTBCD
}

func decodeAddressString(b []byte) (*AddressString, error) {
	if len(b) < 1 || len(b) > 20 {
		return nil, errCodecBadAddrLen
	}
	digits, err := decodeTBCD(b[1:])
	if err != nil {
		return nil, fmt.Errorf("AddressString: %w", err)
	}
	return &AddressString{
		Nature:     (b[0] >> 4) & 0x07,
		NumberPlan: b[0] & 0x0F,
		Digits:     digits,
	}, nil
}

// ----- InitialDPArg --------------------------------------------------------
//
// InitialDPArg ::= SEQUENCE {
//   serviceKey                 [0]  ServiceKey,
//   calledPartyNumber          [2]  CalledPartyNumber           OPTIONAL,
//   callingPartyNumber         [3]  CallingPartyNumber          OPTIONAL,
//   callingPartysCategory      [5]  CallingPartysCategory       OPTIONAL,
//   ...   -- many other optional fields, all skipped here
// }
//
// We extract the mandatory serviceKey and the two address strings most
// commonly present in real InitialDP traffic (called/calling). The rest is
// forward-compat — captured into the Rest extension slice and ignored.

type wireInitialDPArg struct {
	ServiceKey         int             `asn1:"tag:0,implicit"`
	CalledPartyNumber  []byte          `asn1:"tag:2,optional,implicit"`
	CallingPartyNumber []byte          `asn1:"tag:3,optional,implicit"`
	Rest               []asn1.RawValue `asn1:"optional"`
}

func decodeInitialDPArg(b []byte) (*InitialDPRequest, error) {
	var w wireInitialDPArg
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("InitialDPArg: %w", err)
	}
	req := &InitialDPRequest{ServiceKey: uint32(w.ServiceKey)}
	if len(w.CalledPartyNumber) > 0 {
		// CalledPartyNumber per TS 29.078 wraps the ISUP "Called Party Number"
		// IE which itself begins with NoA/NPI byte then TBCD digits — same
		// shape as MAP AddressString for our purposes.
		addr, err := decodeAddressString(w.CalledPartyNumber)
		if err != nil {
			return nil, fmt.Errorf("calledPartyNumber: %w", err)
		}
		req.CalledPartyNumber = addr
	}
	if len(w.CallingPartyNumber) > 0 {
		addr, err := decodeAddressString(w.CallingPartyNumber)
		if err != nil {
			return nil, fmt.Errorf("callingPartyNumber: %w", err)
		}
		req.CallingPartyNumber = addr
	}
	return req, nil
}

// ----- EventReportBCSMArg --------------------------------------------------
//
// EventReportBCSMArg ::= SEQUENCE {
//   eventTypeBCSM              [0]  EventTypeBCSM,
//   eventSpecificInformationBCSM [2] EventSpecificInformationBCSM OPTIONAL,
//   legID                       [3] ReceivingSideID OPTIONAL,
//   miscCallInfo                [4] MiscCallInfo OPTIONAL,
//   extensions                  [5] Extensions OPTIONAL
// }

type wireEventReportBCSMArg struct {
	EventTypeBCSM int             `asn1:"tag:0,implicit"`
	Rest          []asn1.RawValue `asn1:"optional"`
}

func decodeEventReportBCSMArg(b []byte) (*EventReportBCSMRequest, error) {
	var w wireEventReportBCSMArg
	if _, err := asn1.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("EventReportBCSMArg: %w", err)
	}
	return &EventReportBCSMRequest{EventTypeBCSM: EventTypeBCSM(w.EventTypeBCSM)}, nil
}

// ----- ApplyChargingReportArg ----------------------------------------------
//
// ApplyChargingReportArg ::= CallResult
// CallResult ::= OCTET STRING (SIZE (...))
//
// The arg is a bare primitive OCTET STRING (no SEQUENCE wrapper). Carrier
// platforms parse the call-result internals; here we just preserve the
// raw bytes for downstream consumers.

func decodeApplyChargingReportArg(b []byte) (*ApplyChargingReportRequest, error) {
	var raw []byte
	if _, err := asn1.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("ApplyChargingReportArg: %w", err)
	}
	return &ApplyChargingReportRequest{CallResult: raw}, nil
}
