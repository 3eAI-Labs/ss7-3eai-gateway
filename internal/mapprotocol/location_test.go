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

// The expected octets below are built by hand from the ASN.1 of TS 29.002
// (MAP-MS-DataTypes, IMPLICIT TAGS; tagged CHOICEs explicit), not produced by
// the encoder, so the encoder and decoder are each checked against the
// specification rather than against each other.

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	// AnyTimeInterrogationArg: msisdn 905321112233, locationInformation +
	// subscriberState, gsmSCF-Address 905009990000.
	atiArgHex = "30 1A" +
		" A0 09 81 07 91 09 35 12 11 22 33" + // subscriberIdentity [0] { msisdn [1] }
		" A1 04 80 00 81 00" + // requestedInfo [1] { locationInformation [0], subscriberState [1] }
		" 83 07 91 09 05 90 99 00 00" // gsmSCF-Address [3]

	// ProvideSubscriberInfoArg: imsi 286010000000123, locationInformation,
	// currentLocation, requestedDomain ps, imei.
	psiArgHex = "30 15" +
		" 80 08 82 06 01 00 00 00 21 F3" + // imsi [0]
		" A2 09 80 00 83 00 84 01 01 86 00" // requestedInfo [2] { [0], [3], [4] ps, imei [6] }

	// AnyTimeInterrogationRes, CS domain.
	atiResHex = "30 31 30 2F" + // AnyTimeInterrogationRes { subscriberInfo SEQUENCE
		" A0 19" + // locationInformation [0]
		" 02 01 05" + // ageOfLocationInformation 5
		" 81 07 91 09 35 02 00 00 10" + // vlr-number [1] 905320000001
		" A3 09 80 07 82 F6 10 12 34 56 78" + // [3] { cellGlobalId…FixedLength [0] 286/01 LAC 1234 CI 5678 }
		" 88 00" + // currentLocationRetrieved [8]
		" A1 03 0A 01 01" + // subscriberState [1] { netDetNotReachable imsiDetached }
		" 85 08 94 10 45 02 23 73 15 F8" + // imei [5] 490154203237518
		" A8 03 83 01 05" // mnpInfoRes [8] { numberPortabilityStatus foreignNumberPortedIn }

	// ProvideSubscriberInfoRes, PS domain, with fields the gateway skips.
	psiResHex = "30 22 30 20" +
		" A2 00" + // extensionContainer [2] (empty, skipped)
		" A3 14" + // locationInformationGPRS [3]
		" A0 07 81 05 13 00 14 00 2A" + // [0] { laiFixedLength [1] 310/410 LAC 002A }
		" 81 06 13 00 14 00 2A 07" + // routeingAreaIdentity [1]
		" 89 01 00" + // ageOfLocationInformation [9] 0
		" A4 02 81 00" + // ps-SubscriberState [4] { ps-Detached [1] }
		" 8E 02 40 00" // timeZone [14] (skipped)
)

func locationSeeds() [][]byte {
	var out [][]byte
	for _, h := range []string{atiArgHex, psiArgHex, atiResHex, psiResHex} {
		b, _ := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
		out = append(out, b)
	}
	return out
}

func TestEncodeAnyTimeInterrogationArg(t *testing.T) {
	msisdn := MSISDN("905321112233")
	got, err := EncodeAnyTimeInterrogationArg(&AnyTimeInterrogationRequest{
		SubscriberIdentity: SubscriberIdentity{MSISDN: &msisdn},
		RequestedInfo:      RequestedInfo{LocationInformation: true, SubscriberState: true},
		GSMSCFAddress:      AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905009990000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, atiArgHex); !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got % X\nwant % X", got, want)
	}
	back, err := DecodeAnyTimeInterrogationArg(got)
	if err != nil || *back.SubscriberIdentity.MSISDN != msisdn || back.GSMSCFAddress.Digits != "905009990000" ||
		!back.RequestedInfo.LocationInformation || !back.RequestedInfo.SubscriberState {
		t.Fatalf("decoded %+v, %v", back, err)
	}
}

func TestEncodeProvideSubscriberInfoArg(t *testing.T) {
	ps := DomainPS
	got, err := EncodeProvideSubscriberInfoArg(&ProvideSubscriberInfoRequest{
		IMSI: "286010000000123",
		RequestedInfo: RequestedInfo{LocationInformation: true, CurrentLocation: true,
			RequestedDomain: &ps, IMEI: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, psiArgHex); !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got % X\nwant % X", got, want)
	}
	back, err := DecodeProvideSubscriberInfoArg(got)
	if err != nil || back.IMSI != "286010000000123" || *back.RequestedInfo.RequestedDomain != DomainPS ||
		!back.RequestedInfo.CurrentLocation || !back.RequestedInfo.IMEI {
		t.Fatalf("decoded %+v, %v", back, err)
	}
}

func TestRequestedInfoRules(t *testing.T) {
	imsi := IMSI("286010000000123")
	for name, ri := range map[string]RequestedInfo{
		"nothing":                         {},
		"current location alone":          {SubscriberState: true, CurrentLocation: true},
		"EPS without locationInformation": {SubscriberState: true, LocationInformationEPSSupported: true},
	} {
		if _, err := EncodeAnyTimeInterrogationArg(&AnyTimeInterrogationRequest{
			SubscriberIdentity: SubscriberIdentity{IMSI: &imsi}, RequestedInfo: ri,
			GSMSCFAddress: AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "1"},
		}); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
	msisdn := MSISDN("1")
	if _, err := EncodeAnyTimeInterrogationArg(&AnyTimeInterrogationRequest{
		SubscriberIdentity: SubscriberIdentity{IMSI: &imsi, MSISDN: &msisdn},
		RequestedInfo:      RequestedInfo{SubscriberState: true},
		GSMSCFAddress:      AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "1"},
	}); err == nil {
		t.Error("both identities encoded")
	}
}

func TestDecodeAnyTimeInterrogationRes(t *testing.T) {
	res, err := DecodeAnyTimeInterrogationRes(unhex(t, atiResHex))
	if err != nil {
		t.Fatal(err)
	}
	five := 5
	want := SubscriberInfo{
		LocationInformation: &LocationInformation{
			AgeOfLocationInformation: &five,
			VLRNumber:                &AddressString{Nature: 1, NumberPlan: 1, Digits: "905320000001"},
			CellGlobalID:             &CellGlobalID{MCC: "286", MNC: "01", LAC: 0x1234, CI: 0x5678},
			CurrentLocationRetrieved: true,
		},
		SubscriberState: &SubscriberState{State: "netDetNotReachable", NotReachableReason: "imsiDetached"},
		IMEI:            "490154203237518",
		MNPInfoRes:      &MNPInfoRes{NumberPortabilityStatus: "foreignNumberPortedIn"},
	}
	if !reflect.DeepEqual(res.SubscriberInfo, want) {
		t.Fatalf("decoded\n got %+v\nwant %+v", res.SubscriberInfo, want)
	}
	again, err := EncodeAnyTimeInterrogationRes(res)
	if err != nil || !bytes.Equal(again, unhex(t, atiResHex)) {
		t.Fatalf("re-encoded % X, %v", again, err)
	}
}

func TestDecodeProvideSubscriberInfoRes(t *testing.T) {
	res, err := DecodeProvideSubscriberInfoRes(unhex(t, psiResHex))
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	want := SubscriberInfo{
		LocationInformationGPRS: &LocationInformation{
			LAI:                      &LAI{MCC: "310", MNC: "410", LAC: 0x002A},
			RouteingAreaIdentity:     unhex(t, "13 00 14 00 2A 07"),
			AgeOfLocationInformation: &zero,
		},
		PSSubscriberState: &SubscriberState{State: "ps-Detached"},
	}
	if !reflect.DeepEqual(res.SubscriberInfo, want) {
		t.Fatalf("decoded\n got %+v\nwant %+v", res.SubscriberInfo, want)
	}
}

func TestDecodeSubscriberInfoServiceAreaAndEPS(t *testing.T) {
	ten := 10
	in := ProvideSubscriberInfoResponse{SubscriberInfo: SubscriberInfo{
		LocationInformation: &LocationInformation{
			ServiceAreaID: &ServiceAreaID{MCC: "286", MNC: "02", LAC: 7, SAC: 9},
			MSCNumber:     &AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905320000002"},
			EPS: &LocationInformationEPS{EUTRANCGI: unhex(t, "82 F6 20 00 00 01 01"),
				TrackingAreaIdentity: unhex(t, "82 F6 20 00 07"), AgeOfLocationInformation: &ten,
				MMEName: "mme1.epc.example"},
		},
		EPSSubscriberState: &SubscriberState{State: "ps-AttachedReachableForPaging"},
		MSClassmark2:       unhex(t, "33 19 A2"),
	}}
	b, err := EncodeProvideSubscriberInfoRes(&in)
	if err != nil {
		t.Fatal(err)
	}
	// sai-Present [9] marks the fixed-length cell identity as a service area.
	if !bytes.Contains(b, []byte{0x89, 0x00}) {
		t.Fatalf("sai-Present missing: % X", b)
	}
	out, err := DecodeProvideSubscriberInfoRes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.SubscriberInfo, in.SubscriberInfo) {
		t.Fatalf("round trip\n got %+v\nwant %+v", out.SubscriberInfo, in.SubscriberInfo)
	}
}

func TestDecodeSubscriberInfoRejectsMalformed(t *testing.T) {
	for name, h := range map[string]string{
		"no subscriberInfo":   "30 00",
		"trailing data":       "30 02 30 00 00",
		"short CGI":           "30 0A 30 08 A0 06 A3 04 80 02 82 F6",
		"MCC not digits":      "30 0F 30 0D A0 0B A3 09 80 07 8A F6 10 12 34 56 78",
		"two state choices":   "30 08 30 06 A1 04 80 00 81 00",
		"IMEI of 7 octets":    "30 0B 30 09 85 07 94 10 45 02 23 73 15",
		"age out of range":    "30 0A 30 08 A0 06 02 04 00 01 00 00",
		"geographical size 7": "30 0D 30 0B A0 09 80 07 01 02 03 04 05 06 07",
	} {
		if _, err := DecodeAnyTimeInterrogationRes(unhex(t, h)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// A state alternative added after this codec passes through by number.
func TestDecodeUnknownStateAlternative(t *testing.T) {
	res, err := DecodeAnyTimeInterrogationRes(unhex(t, "30 06 30 04 A1 02 85 00"))
	if err != nil || res.SubscriberInfo.SubscriberState.State != "subscriberState[5]" {
		t.Fatalf("decoded %+v, %v", res, err)
	}
}

func TestDecodePLMN(t *testing.T) {
	for _, tc := range []struct {
		octets   string
		mcc, mnc string
	}{
		{"82 F6 10", "286", "01"},  // two-digit MNC: filler in octet 2's high nibble
		{"13 00 14", "310", "410"}, // three-digit MNC
	} {
		mcc, mnc, err := decodePLMN(unhex(t, tc.octets))
		if err != nil || mcc != tc.mcc || mnc != tc.mnc {
			t.Errorf("%s → %s/%s, %v; want %s/%s", tc.octets, mcc, mnc, err, tc.mcc, tc.mnc)
		}
		back, err := encodePLMN(tc.mcc, tc.mnc)
		if err != nil || !bytes.Equal(back, unhex(t, tc.octets)) {
			t.Errorf("encodePLMN(%s, %s) = % X, %v", tc.mcc, tc.mnc, back, err)
		}
	}
}

// AddressString's nature of address is TS 29.002's (international = 001),
// not the SCCP global title's (international = 4): the first octet of an
// international E.164 number is 0x91. Built by hand from the ASN.1.
func TestSendRoutingInfoForSMArgOctets(t *testing.T) {
	got, err := EncodeSendRoutingInfoForSMArg(&SendRoutingInfoForSMRequest{
		MSISDN: "905321112233", SMRPPRI: true,
		ServiceCentreAddress: AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905001112233"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := unhex(t, "30 15"+
		" 80 07 91 09 35 12 11 22 33"+ // msisdn [0]
		" 81 01 FF"+ // sm-RP-PRI [1] TRUE
		" 82 07 91 09 05 10 11 22 33") // serviceCentreAddress [2]
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got % X\nwant % X", got, want)
	}
}
