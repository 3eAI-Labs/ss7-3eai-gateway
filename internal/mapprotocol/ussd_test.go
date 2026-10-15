// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"reflect"
	"testing"
)

// Built by hand from TS 29.002 USSD-Arg / USSD-Res (MAP-SS-DataTypes).
const (
	// processUnstructuredSS-Request from the HLR: "*123#" in GSM 7-bit
	// (DCS 0F), the subscriber's MSISDN.
	ussdArgHex = "30 13" +
		" 04 01 0F" + // ussd-DataCodingScheme
		" 04 05 AA 98 6C 36 02" + // ussd-String "*123#"
		" 80 07 91 09 35 12 11 22 33" // msisdn [0]
	// The same with an alerting pattern (the third OCTET STRING).
	ussdArgAlertHex = "30 16 04 01 0F 04 05 AA 98 6C 36 02 04 01 05 80 07 91 09 35 12 11 22 33"
	// USSD-Res: "1" in GSM 7-bit.
	ussdResHex = "30 06 04 01 0F 04 01 31"
)

func TestUSSDArg(t *testing.T) {
	alert := byte(5)
	for name, c := range map[string]struct {
		hex string
		arg *USSDArg
	}{
		"msisdn":           {ussdArgHex, &USSDArg{DCS: 0x0F, String: unhex(t, "AA 98 6C 36 02"), MSISDN: isdn("905321112233")}},
		"alerting pattern": {ussdArgAlertHex, &USSDArg{DCS: 0x0F, String: unhex(t, "AA 98 6C 36 02"), AlertingPattern: &alert, MSISDN: isdn("905321112233")}},
	} {
		got, err := EncodeUSSDArg(c.arg)
		if err != nil {
			t.Fatal(err)
		}
		if want := unhex(t, c.hex); !bytes.Equal(got, want) {
			t.Errorf("%s: encoded\n got % X\nwant % X", name, got, want)
		}
		back, err := DecodeUSSDArg(got)
		if err != nil || !reflect.DeepEqual(back, c.arg) {
			t.Errorf("%s: decoded %+v %v", name, back, err)
		}
	}
	res, err := EncodeUSSDRes(0x0F, []byte{0x31})
	if err != nil || !bytes.Equal(res, unhex(t, ussdResHex)) {
		t.Fatalf("USSD-Res % X %v", res, err)
	}
	if back, err := DecodeUSSDArg(res); err != nil || back.DCS != 0x0F || !bytes.Equal(back.String, []byte{0x31}) {
		t.Fatalf("USSD-Res decoded %+v %v", back, err)
	}
}

func TestUSSDArgRejects(t *testing.T) {
	for name, h := range map[string]string{
		"no string":         "30 03 04 01 0F",
		"DCS of 2 octets":   "30 07 04 02 0F 00 04 01 31",
		"empty string":      "30 05 04 01 0F 04 00",
		"alerting 2 octets": "30 0A 04 01 0F 04 01 31 04 02 05 05",
	} {
		if a, err := DecodeUSSDArg(unhex(t, h)); err == nil {
			t.Errorf("%s: decoded %+v", name, a)
		}
	}
	if _, err := EncodeUSSDArg(&USSDArg{DCS: 0x0F, String: make([]byte, 161)}); err == nil {
		t.Error("161-octet ussd-String encoded")
	}
}
