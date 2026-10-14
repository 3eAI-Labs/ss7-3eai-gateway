// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"testing"
)

// Round-trip tests for the MAP-SM-DataTypes codec (3GPP TS 29.002 §17.7).
// The encoder must be the inverse of the decoder for the field set we
// exercise — operationally this means a SRI-for-SM Arg encoded by the
// gateway and round-tripped through asn1.Unmarshal yields the same MSISDN,
// SCA digits, and SM-RP-PRI flag the operator put in. Catches regressions
// in tag/length emission and CHOICE alternative selection.

func TestSRI_for_SM_RoundTrip(t *testing.T) {
	in := &SendRoutingInfoForSMRequest{
		MSISDN:               "905321234567",
		SMRPPRI:              true,
		ServiceCentreAddress: AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905001112233"},
	}
	b, err := encodeSendRoutingInfoForSMArg(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeSendRoutingInfoForSMArg(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.MSISDN != in.MSISDN {
		t.Errorf("msisdn: got %q want %q", out.MSISDN, in.MSISDN)
	}
	if out.SMRPPRI != in.SMRPPRI {
		t.Errorf("sm-RP-PRI: got %v want %v", out.SMRPPRI, in.SMRPPRI)
	}
	if out.ServiceCentreAddress.Digits != in.ServiceCentreAddress.Digits {
		t.Errorf("sca: got %q want %q",
			out.ServiceCentreAddress.Digits, in.ServiceCentreAddress.Digits)
	}
}

func TestSRI_for_SM_Response_RoundTrip(t *testing.T) {
	in := &SendRoutingInfoForSMResponse{
		IMSI: "286010123456789",
		LocationInfoWithLMSI: LocationInfoWithLMSI{
			NetworkNodeNumber: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905304567890"},
			LMSI:              []byte{0x01, 0x02, 0x03, 0x04},
		},
	}
	b, err := encodeSendRoutingInfoForSMRes(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeSendRoutingInfoForSMRes(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.IMSI != in.IMSI {
		t.Errorf("imsi: got %q want %q", out.IMSI, in.IMSI)
	}
	if out.LocationInfoWithLMSI.NetworkNodeNumber == nil ||
		out.LocationInfoWithLMSI.NetworkNodeNumber.Digits != "905304567890" {
		t.Errorf("network-node-number not preserved: %+v",
			out.LocationInfoWithLMSI.NetworkNodeNumber)
	}
	if !bytes.Equal(out.LocationInfoWithLMSI.LMSI, in.LocationInfoWithLMSI.LMSI) {
		t.Errorf("lmsi: got %x want %x",
			out.LocationInfoWithLMSI.LMSI, in.LocationInfoWithLMSI.LMSI)
	}
}

func TestMtForwardSM_RoundTrip_IMSI_DA(t *testing.T) {
	imsi := IMSI("286010123456789")
	tpdu := []byte{0x04, 0x0B, 0x91, 0x09, 0x53, 0x21, 0x43, 0x65, 0x87, 0x00, 0x00}

	in := &MtForwardSMRequest{
		SMRPDA:             SMRPDA{IMSI: &imsi},
		SMRPOA:             SMRPOA{ServiceCentreAddress: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905001112233"}},
		SMRPUI:             tpdu,
		MoreMessagesToSend: true,
	}
	b, err := encodeMtForwardSMArg(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeMtForwardSMArg(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SMRPDA.IMSI == nil || *out.SMRPDA.IMSI != imsi {
		t.Errorf("sm-RP-DA imsi: got %+v want %q", out.SMRPDA.IMSI, imsi)
	}
	if out.SMRPOA.ServiceCentreAddress == nil ||
		out.SMRPOA.ServiceCentreAddress.Digits != "905001112233" {
		t.Errorf("sm-RP-OA sca not preserved: %+v", out.SMRPOA.ServiceCentreAddress)
	}
	if !bytes.Equal(out.SMRPUI, tpdu) {
		t.Errorf("tpdu: got %x want %x", out.SMRPUI, tpdu)
	}
	if !out.MoreMessagesToSend {
		t.Error("moreMessagesToSend dropped")
	}
}

func TestMtForwardSM_RoundTrip_LMSI_DA(t *testing.T) {
	msisdn := MSISDN("905321234567")
	in := &MtForwardSMRequest{
		SMRPDA: SMRPDA{LMSI: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
		SMRPOA: SMRPOA{MSISDN: &msisdn},
		SMRPUI: []byte{0x01, 0x02, 0x03},
	}
	b, err := encodeMtForwardSMArg(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeMtForwardSMArg(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(out.SMRPDA.LMSI, in.SMRPDA.LMSI) {
		t.Errorf("sm-RP-DA lmsi: got %x want %x", out.SMRPDA.LMSI, in.SMRPDA.LMSI)
	}
	if out.SMRPOA.MSISDN == nil || *out.SMRPOA.MSISDN != msisdn {
		t.Errorf("sm-RP-OA msisdn not preserved: %+v", out.SMRPOA.MSISDN)
	}
}

func TestMtForwardSM_Response_EmptyOK(t *testing.T) {
	// MT-Fwd-SM-Res may be empty (no sm-RP-UI). Encoder must not panic;
	// decoder must accept the bare SEQUENCE.
	b, err := encodeMtForwardSMRes(&MtForwardSMResponse{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeMtForwardSMRes(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.SMRPUI) != 0 {
		t.Errorf("expected empty sm-RP-UI, got %x", out.SMRPUI)
	}
}

func TestMoForwardSM_RoundTrip(t *testing.T) {
	imsi := IMSI("286010987654321")
	in := &MoForwardSMRequest{
		SMRPDA: SMRPDA{ServiceCentreAddress: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905001112233"}},
		SMRPOA: SMRPOA{ServiceCentreAddress: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905004445566"}},
		SMRPUI: []byte{0x11, 0x22, 0x33},
		IMSI:   &imsi,
	}
	b, err := encodeMoForwardSMArg(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeMoForwardSMArg(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.IMSI == nil || *out.IMSI != imsi {
		t.Errorf("post-rel-99 imsi: got %+v want %q", out.IMSI, imsi)
	}
	if !bytes.Equal(out.SMRPUI, in.SMRPUI) {
		t.Errorf("tpdu: got %x want %x", out.SMRPUI, in.SMRPUI)
	}
}

func TestSMRPDA_NoVariant_Errors(t *testing.T) {
	// Encoder must reject an SM-RP-DA with no alternative chosen — the
	// CHOICE is mandatory in the wire grammar.
	_, err := encodeSMRPDA(SMRPDA{})
	if err == nil {
		t.Fatal("expected error on empty SM-RP-DA")
	}
}

func TestSMRPDA_NoSMRPDA_RoundTrip(t *testing.T) {
	// Explicit "noSM-RP-DA" alternative — primitive [5] NULL.
	rv, err := encodeSMRPDA(SMRPDA{NoSMRPDA: true})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeSMRPDA(rv)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.NoSMRPDA {
		t.Errorf("noSM-RP-DA dropped: %+v", out)
	}
}
