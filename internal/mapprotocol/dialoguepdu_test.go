// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"reflect"
	"testing"
)

// Built by hand from TS 29.002 MAP-DialogueInformation (IMPLICIT TAGS) inside
// a TCAP EXTERNAL (Q.773 §3.2): the user-information content of a BEGIN that
// opens a USSD dialogue towards the HLR, destination reference an IMSI
// (numbering plan E.212), origination reference the gsmSCF (E.164).
const mapOpenInfoHex = "28 21" + // EXTERNAL
	" 06 07 04 00 00 01 01 01 01" + // map-DialogueAS 0.4.0.0.1.1.1.1
	" A0 16 A0 14" + // single-ASN1-type [0], map-open [0]
	" 80 09 96 82 06 01 00 00 00 21 F3" + // destinationReference [0]: international, E.212, 286010000000123
	" 81 07 91 09 05 90 99 00 00" // originationReference [1]: international, E.164, 905009990000

func TestMAPOpenInfo(t *testing.T) {
	o := &MAPOpenInfo{
		DestinationReference: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanLandMobile, Digits: "286010000000123"},
		OriginationReference: &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905009990000"},
	}
	got, err := EncodeMAPOpenInfo(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, mapOpenInfoHex); !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got % X\nwant % X", got, want)
	}
	back, ok, err := DecodeMAPOpenInfo(got)
	if err != nil || !ok || !reflect.DeepEqual(back, o) {
		t.Fatalf("decoded %+v %v %v", back, ok, err)
	}
}

func TestDecodeMAPOpenInfoOthers(t *testing.T) {
	// An EXTERNAL in another abstract syntax, then MAP-AcceptInfo [1]: no open.
	other := unhex(t, "28 0D 06 07 04 00 00 01 01 01 02 A0 02 A1 00")
	accept := unhex(t, "28 0D 06 07 04 00 00 01 01 01 01 A0 02 A1 00")
	if o, ok, err := DecodeMAPOpenInfo(append(other, accept...)); ok || err != nil {
		t.Fatalf("%+v %v %v", o, ok, err)
	}
	if _, _, err := DecodeMAPOpenInfo(unhex(t, "28 05 06 03")); err == nil {
		t.Fatal("truncated EXTERNAL accepted")
	}
}
