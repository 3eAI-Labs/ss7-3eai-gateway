// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// Built by hand from the ASN.1 of TS 29.002 MAP-CH-DataTypes (IMPLICIT
// TAGS; the tagged CHOICE basicService explicit), as in location_test.go.
const (
	// SendRoutingInfoArg: msisdn 905321112233, basicCall, GMSC 905009990000.
	sriArgHex = "30 15" +
		" 80 07 91 09 35 12 11 22 33" + // msisdn [0]
		" 83 01 00" + // interrogationType [3] basicCall
		" 86 07 91 09 05 90 99 00 00" // gmsc-OrGsmSCF-Address [6]

	// SendRoutingInfoRes with a roaming number, the fields the gateway reads.
	sriResMSRNFields = " 89 08 82 06 01 00 00 00 21 F3" + // imsi [9] 286010000000123
		" 04 07 91 09 35 92 99 00 10" + // roamingNumber: untagged ISDN-AddressString 905329990001
		" A7 04 A1 02 80 00" + // subscriberInfo [7] { subscriberState [1] { assumedIdle [0] } }
		" A1 06 04 01 21 04 01 29" + // ss-List [1] { cfu 21, cfb 29 }
		" A5 03 83 01 11" + // basicService [5] { ext-Teleservice [3] telephony 11 }
		" 82 07 91 09 35 02 00 00 10" + // vmsc-Address [2] 905320000001
		" 8D 01 00" // numberPortabilityStatus [13] notKnownToBePorted
	sriResMSRNHex = "A3 32" + sriResMSRNFields

	// The same with fields the gateway skips, where the ASN.1 puts them.
	sriResMSRNSkippedHex = "A3 38" +
		" 89 08 82 06 01 00 00 00 21 F3 04 07 91 09 35 92 99 00 10 A7 04 A1 02 80 00" +
		" A1 06 04 01 21 04 01 29 A5 03 83 01 11 82 07 91 09 35 02 00 00 10" +
		" A0 00" + // extensionContainer [0]
		" 8D 01 00" +
		" 94 02 06 C0" // allowedServices [20] BIT STRING

	// SendRoutingInfoRes forwarding the call: forwardedToNumber 905551112233,
	// options notification to calling party + busy, basic MSISDN.
	sriResForwardHex = "A3 21" +
		" 89 08 82 06 01 00 00 00 21 F3" +
		" 30 0C" + // forwardingData: untagged SEQUENCE
		" 85 07 91 09 55 15 11 22 33" + // forwardedToNumber [5]
		" 86 01 24" + // forwardingOptions [6]: bit 6 notification to calling party, bits 43 = 01 busy
		" 8C 07 91 09 35 12 11 22 33" // msisdn [12]

	// SendRoutingInfoRes with CAMEL routing information.
	sriResCamelHex = "A3 11" +
		" 89 08 82 06 01 00 00 00 21 F3" +
		" A8 05 A0 03 80 01 05" // camelRoutingInfo [8] { gmscCamelSubscriptionInfo [0] { … } }
)

func routingSeeds() [][]byte {
	var out [][]byte
	for _, h := range []string{sriArgHex, sriResMSRNHex, sriResMSRNSkippedHex, sriResForwardHex, sriResCamelHex} {
		b, _ := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
		out = append(out, b)
	}
	return out
}

func isdn(digits string) *AddressString {
	return &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: digits}
}

func TestEncodeSendRoutingInfoArg(t *testing.T) {
	req := &SendRoutingInfoRequest{
		MSISDN:            "905321112233",
		InterrogationType: InterrogationBasicCall,
		GMSCAddress:       *isdn("905009990000"),
	}
	got, err := EncodeSendRoutingInfoArg(req)
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, sriArgHex); !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got % X\nwant % X", got, want)
	}
	back, err := DecodeSendRoutingInfoArg(got)
	if err != nil || !reflect.DeepEqual(back, req) {
		t.Fatalf("peer decode = %+v, %v", back, err)
	}
}

func msrnResponse() *SendRoutingInfoResponse {
	return &SendRoutingInfoResponse{
		IMSI:                    "286010000000123",
		RoamingNumber:           isdn("905329990001"),
		SubscriberInfo:          &SubscriberInfo{SubscriberState: &SubscriberState{State: "assumedIdle"}},
		SSList:                  []byte{0x21, 0x29},
		BasicService:            &BasicServiceCode{Code: []byte{0x11}},
		VMSCAddress:             isdn("905320000001"),
		NumberPortabilityStatus: "notKnownToBePorted",
	}
}

func TestDecodeSendRoutingInfoRes(t *testing.T) {
	opts := byte(0x24)
	cases := map[string]struct {
		hex  string
		want *SendRoutingInfoResponse
	}{
		"roaming number":      {sriResMSRNHex, msrnResponse()},
		"skipped fields":      {sriResMSRNSkippedHex, msrnResponse()},
		"forwarding":          {sriResForwardHex, &SendRoutingInfoResponse{IMSI: "286010000000123", MSISDN: isdn("905321112233"), ForwardingData: &ForwardingData{ForwardedToNumber: isdn("905551112233"), ForwardingOptions: &opts}}},
		"camel routing":       {sriResCamelHex, &SendRoutingInfoResponse{IMSI: "286010000000123", CamelRoutingInfo: &CamelRoutingInfo{GMSCCamelSubscriptionInfo: unhex(t, "A0 03 80 01 05")}}},
		"no routing, no IMSI": {"A3 02 84 00", &SendRoutingInfoResponse{ForwardingInterrogationRequired: true}},
		"10-octet roaming number": {"A3 0C 04 0A 91 09 35 92 99 00 10 11 11 11",
			&SendRoutingInfoResponse{RoamingNumber: isdn("905329990001111111")}},
		"unlisted NP status":   {"A3 03 8D 01 07", &SendRoutingInfoResponse{}},
		"bearer basic service": {"A3 05 A5 03 82 01 10", &SendRoutingInfoResponse{BasicService: &BasicServiceCode{Bearer: true, Code: []byte{0x10}}}},
	}
	for name, c := range cases {
		got, err := DecodeSendRoutingInfoRes(unhex(t, c.hex))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, c.want)
		}
	}
}

// The peer's encoder against the same hand-built octets.
func TestEncodeSendRoutingInfoRes(t *testing.T) {
	opts := byte(0x24)
	for name, c := range map[string]struct {
		r   *SendRoutingInfoResponse
		hex string
	}{
		"roaming number": {msrnResponse(), sriResMSRNHex},
		"forwarding":     {&SendRoutingInfoResponse{IMSI: "286010000000123", MSISDN: isdn("905321112233"), ForwardingData: &ForwardingData{ForwardedToNumber: isdn("905551112233"), ForwardingOptions: &opts}}, sriResForwardHex},
		"camel routing":  {&SendRoutingInfoResponse{IMSI: "286010000000123", CamelRoutingInfo: &CamelRoutingInfo{GMSCCamelSubscriptionInfo: unhex(t, "A0 03 80 01 05")}}, sriResCamelHex},
	} {
		got, err := EncodeSendRoutingInfoRes(c.r)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if want := unhex(t, c.hex); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got % X\nwant % X", name, got, want)
		}
	}
}

func TestDecodeSendRoutingInfoResRejects(t *testing.T) {
	for name, h := range map[string]string{
		"universal SEQUENCE, not [3]": "30 03 84 01 00",
		"trailing data":               "A3 00 00",
		"two routing alternatives":    "A3 12 04 07 91 09 35 92 99 00 10 30 05 86 01 24 86 00",
		"address of 21 octets":        "A3 17 82 15 91 09 35 02 00 00 10 11 11 11 11 11 11 11 11 11 11 11 11 11 11",
		"NULL with content":           "A3 03 84 01 00",
		"empty ss-List":               "A3 02 A1 00",
		"two-octet SS-Code":           "A3 06 A1 04 04 02 21 29",
		"basicService [4]":            "A3 05 A5 03 84 01 11",
		"forwardingOptions 2 octets":  "A3 06 30 04 86 02 24 00",
		"camel without subscription":  "A3 02 A8 00",
		"bad IMSI TBCD":               "A3 04 89 02 F1 21",
	} {
		if r, err := DecodeSendRoutingInfoRes(unhex(t, h)); err == nil {
			t.Errorf("%s: decoded %+v, want an error", name, r)
		}
	}
}

func TestMergeSendRoutingInfoRes(t *testing.T) {
	first, err := DecodeSendRoutingInfoRes(unhex(t, "A3 0F 89 08 82 06 01 00 00 00 21 F3 A1 03 04 01 21"))
	if err != nil {
		t.Fatal(err)
	}
	last, err := DecodeSendRoutingInfoRes(unhex(t, "A3 0E 04 07 91 09 35 92 99 00 10 A1 03 04 01 29"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MergeSendRoutingInfoRes([]*SendRoutingInfoResponse{first, last})
	if err != nil {
		t.Fatal(err)
	}
	want := &SendRoutingInfoResponse{IMSI: "286010000000123", RoamingNumber: isdn("905329990001"), SSList: []byte{0x21, 0x29}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged\n got %+v\nwant %+v", got, want)
	}
	if _, err := MergeSendRoutingInfoRes([]*SendRoutingInfoResponse{last, last}); err == nil {
		t.Error("routing information in two segments merged")
	}
}

// The result as the dispatcher reads it: one or more segments, complete only
// with the IMSI.
func TestDecodeSendRoutingInfoResult(t *testing.T) {
	imsi := unhex(t, "A3 0A 89 08 82 06 01 00 00 00 21 F3")
	msrn := unhex(t, "A3 09 04 07 91 09 35 92 99 00 10")
	r, err := DecodeSendRoutingInfoResult([][]byte{imsi, msrn})
	if err != nil || r.IMSI != "286010000000123" || r.RoamingNumber.Digits != "905329990001" {
		t.Fatalf("segments: %+v %v", r, err)
	}
	if _, err := DecodeSendRoutingInfoResult([][]byte{msrn}); err == nil {
		t.Error("a result without the IMSI accepted")
	}
	if _, err := DecodeSendRoutingInfoResult([][]byte{msrn, imsi[:3]}); err == nil {
		t.Error("an undecodable segment accepted")
	}
}
