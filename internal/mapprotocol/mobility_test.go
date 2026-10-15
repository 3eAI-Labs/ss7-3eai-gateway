// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden frames for the mobility operations, built by hand from the
// TS 29.002 ASN.1 (MAP-MS-DataTypes, IMPLICIT TAGS) — not from the codec —
// so that encoder and decoder cannot agree on a wrong tag. Each line is one
// element; lengths are counted by hand.

func hexFrame(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	require.NoError(t, err)
	return b
}

var (
	goldenIMSI = IMSI("286010000000123") // 82 06 01 00 00 00 21 F3
	intl       = func(d string) AddressString {
		return AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: d}
	}
)

// UpdateLocationArg as a VLR sends it.
const goldenUpdateLocationArg = `30 3C
	04 08 82 06 01 00 00 00 21 F3           imsi 286010000000123
	81 07 91 09 35 12 00 00 10              msc-Number [1] 905321000001
	04 07 91 09 35 12 00 00 20              vlr-Number 905321000002
	8A 04 01 02 03 04                       lmsi [10]
	A6 0A                                   vlr-Capability [6]
	   80 02 05 E0                          supportedCamelPhases [0]: phases 1-3
	   87 02 06 C0                          supportedRAT-TypesIndicator [7]: utran, geran
	   8A 00                                msisdn-lessOperation-Supported [10]
	AD 0A 80 08 53 43 65 10 21 43 65 10     add-info [13] { imeisv [0] 3534560112345601 }
	90 00                                   restorationIndicator [16]`

func goldenFrame(t *testing.T, annotated string) []byte {
	t.Helper()
	var hexes []string
	for _, line := range strings.Split(annotated, "\n") {
		for _, w := range strings.Fields(line) {
			if len(w) != 2 || strings.ToUpper(w) != w {
				break // the annotation
			}
			if _, err := hex.DecodeString(w); err != nil {
				break
			}
			hexes = append(hexes, w)
		}
	}
	return hexFrame(t, strings.Join(hexes, " "))
}

func TestUpdateLocationArg_Golden(t *testing.T) {
	b := goldenFrame(t, goldenUpdateLocationArg)
	require.Len(t, b, 2+0x3C)
	arg, err := DecodeUpdateLocationArg(b)
	require.NoError(t, err)
	want := &UpdateLocationArg{
		IMSI: goldenIMSI, MSCNumber: intl("905321000001"), VLRNumber: intl("905321000002"),
		LMSI: []byte{1, 2, 3, 4},
		VLRCapability: &VLRCapability{SupportedCAMELPhases: []int{0, 1, 2}, SupportedRATTypes: []int{0, 1},
			MSISDNLessOperationSupported: true},
		IMEISV: "3534560112345601", RestorationIndicator: true,
	}
	assert.Equal(t, want, arg)

	enc, err := EncodeUpdateLocationArg(want)
	require.NoError(t, err)
	assert.Equal(t, b, enc, "the peer's encoder agrees with the hand-built frame")
}

// Fields after the extension marker the gateway does not read, and an
// extension container, are skipped; skipSubscriberDataUpdate is read from
// either place.
func TestUpdateLocationArg_SkipsAndAlternatives(t *testing.T) {
	b := goldenFrame(t, `30 23
		04 08 82 06 01 00 00 00 21 F3   imsi
		81 07 91 09 35 12 00 00 10      msc-Number
		04 07 91 09 35 12 00 00 20      vlr-Number
		30 00                           extensionContainer
		82 01 00                        v-gmlc-Address [2]
		8F 00                           skipSubscriberDataUpdate [15]`)
	arg, err := DecodeUpdateLocationArg(b)
	require.NoError(t, err)
	assert.True(t, arg.SkipSubscriberDataUpdate)
	assert.Nil(t, arg.VLRCapability)

	for name, frame := range map[string]string{
		"vlr-Number before msc-Number": `30 15 04 08 82 06 01 00 00 00 21 F3 04 07 91 09 35 12 00 00 20 81 00`,
		"no vlr-Number":                `30 13 04 08 82 06 01 00 00 00 21 F3 81 07 91 09 35 12 00 00 10`,
		"lmsi of 3 octets": `30 21 04 08 82 06 01 00 00 00 21 F3 81 07 91 09 35 12 00 00 10
			04 07 91 09 35 12 00 00 20 8A 03 01 02 03`,
		"add-info without imeisv": `30 20 04 08 82 06 01 00 00 00 21 F3 81 07 91 09 35 12 00 00 10
			04 07 91 09 35 12 00 00 20 AD 02 81 00`,
	} {
		_, err := DecodeUpdateLocationArg(hexFrame(t, frame))
		assert.Error(t, err, name)
	}
}

func TestUpdateLocationRes_Golden(t *testing.T) {
	want := hexFrame(t, `30 09 04 07 91 09 35 90 99 99 99`) // hlr-Number 905309999999
	b, err := EncodeUpdateLocationRes(&UpdateLocationRes{HLRNumber: intl("905309999999")})
	require.NoError(t, err)
	assert.Equal(t, want, b)
	res, err := DecodeUpdateLocationRes(want)
	require.NoError(t, err)
	assert.Equal(t, intl("905309999999"), res.HLRNumber)
}

func TestCancelLocationArg_Golden(t *testing.T) {
	withdraw := CancellationSubscriptionWithdraw
	for _, tc := range []struct {
		arg   CancelLocationArg
		frame string
	}{
		{CancelLocationArg{IMSI: goldenIMSI, LMSI: []byte{1, 2, 3, 4}, CancellationType: &withdraw}, `A3 15
			30 10 04 08 82 06 01 00 00 00 21 F3 04 04 01 02 03 04   identity: imsi-WithLMSI
			0A 01 01                                                cancellationType subscriptionWithdraw`},
		{CancelLocationArg{IMSI: goldenIMSI}, `A3 0A 04 08 82 06 01 00 00 00 21 F3   identity: imsi`},
	} {
		want := goldenFrame(t, tc.frame)
		b, err := EncodeCancelLocationArg(&tc.arg)
		require.NoError(t, err)
		assert.Equal(t, want, b)
		got, err := DecodeCancelLocationArg(want)
		require.NoError(t, err)
		assert.Equal(t, &tc.arg, got)
	}
	_, err := EncodeCancelLocationArg(&CancelLocationArg{IMSI: goldenIMSI, LMSI: []byte{1}})
	assert.Error(t, err, "an LMSI is 4 octets")
}

func TestCancelLocationRes(t *testing.T) {
	assert.NoError(t, DecodeCancelLocationRes(nil), "the result is optional")
	assert.NoError(t, DecodeCancelLocationRes(hexFrame(t, `30 00`)))
	assert.NoError(t, DecodeCancelLocationRes(hexFrame(t, `30 02 30 00`)), "with an extension container")
	assert.Error(t, DecodeCancelLocationRes(hexFrame(t, `04 00`)))
	assert.Equal(t, hexFrame(t, `30 00`), EncodeCancelLocationRes())
}

const goldenProvideRoamingNumberArg = `30 3B
	80 08 82 06 01 00 00 00 21 F3     imsi [0]
	81 07 91 09 35 12 00 00 10        msc-Number [1] 905321000001
	82 07 91 09 35 21 43 65 87        msisdn [2] 905312345678
	84 04 01 02 03 04                 lmsi [4]
	A5 08 0A 01 01 04 03 A0 04 01     gsm-BearerCapability [5] { protocolId gsm-0408, signalInfo }
	88 07 91 09 35 90 99 99 99        gmsc-Address [8] 905309999999
	89 02 AB CD                       callReferenceNumber [9]
	8A 00                             or-Interrogation [10]`

func TestProvideRoamingNumberArg_Golden(t *testing.T) {
	want := goldenFrame(t, goldenProvideRoamingNumberArg)
	require.Len(t, want, 2+0x3B)
	msisdn, gmsc := intl("905312345678"), intl("905309999999")
	arg := &ProvideRoamingNumberArg{
		IMSI: goldenIMSI, MSCNumber: intl("905321000001"), MSISDN: &msisdn, LMSI: []byte{1, 2, 3, 4},
		GSMBearerCapability: &ExternalSignalInfo{ProtocolID: 1, SignalInfo: []byte{0xA0, 0x04, 0x01}},
		GMSCAddress:         &gmsc, CallReferenceNumber: []byte{0xAB, 0xCD}, ORInterrogation: true,
	}
	b, err := EncodeProvideRoamingNumberArg(arg)
	require.NoError(t, err)
	assert.Equal(t, want, b)
	got, err := DecodeProvideRoamingNumberArg(want)
	require.NoError(t, err)
	assert.Equal(t, arg, got)
}

func TestProvideRoamingNumberRes_Golden(t *testing.T) {
	b := goldenFrame(t, `30 14
		04 07 91 09 35 00 90 99 99   roamingNumber 905300099999
		05 00                        releaseResourcesSupported
		04 07 91 09 35 12 00 00 10   vmsc-Address 905321000001`)
	res, err := DecodeProvideRoamingNumberRes(b)
	require.NoError(t, err)
	vmsc := intl("905321000001")
	want := &ProvideRoamingNumberRes{RoamingNumber: intl("905300099999"), ReleaseResourcesSupported: true, VMSCAddress: &vmsc}
	assert.Equal(t, want, res)
	enc, err := EncodeProvideRoamingNumberRes(want)
	require.NoError(t, err)
	assert.Equal(t, b, enc)

	// An extension container between them; the roaming number alone.
	res, err = DecodeProvideRoamingNumberRes(hexFrame(t, `30 0D 04 07 91 09 35 00 90 99 99 30 00 04 00`))
	assert.Error(t, err, "an empty vmsc-Address is not an AddressString")
	res, err = DecodeProvideRoamingNumberRes(hexFrame(t, `30 0B 04 07 91 09 35 00 90 99 99 30 00`))
	require.NoError(t, err)
	assert.Nil(t, res.VMSCAddress)
	_, err = DecodeProvideRoamingNumberRes(hexFrame(t, `30 02 05 00`))
	assert.Error(t, err, "roamingNumber missing")
}

// InsertSubscriberDataArg within a location update (no IMSI): the
// circuit-switched profile.
const goldenInsertSubscriberDataArg = `30 67
	81 07 91 09 35 21 43 65 87        msisdn [1] 905312345678
	82 01 0A                          category [2] ordinary subscriber
	83 01 00                          subscriberStatus [3] serviceGranted
	A4 03 04 01 16                    bearerServiceList [4]
	A6 06 04 01 11 04 01 21           teleserviceList [6] telephony, shortMessageMT-PP
	A7 35                             provisionedSS [7]
	   A0 1C                          forwardingInfo [0]
	      04 01 2A                    ss-Code cfnry
	      30 17                       forwardingFeatureList
	         30 15                    Ext-ForwFeature
	            83 01 10              basicService ext-Teleservice allSpeechTransmissionServices
	            84 01 07              ss-Status [4] P R A
	            85 07 91 09 35 21 43 65 99   forwardedToNumber [5] 905312345699
	            86 01 88              forwardingOptions [6] notify forwarding party, no reply
	            87 01 14              noReplyConditionTime [7] 20 s
	   A1 0A                          callBarringInfo [1]
	      04 01 92                    ss-Code baoc
	      30 05 30 03 84 01 04        callBarringFeatureList { { ss-Status P } }
	   A3 09                          ss-Data [3]
	      04 01 12                    ss-Code clir
	      84 01 05                    ss-Status P A
	      82 01 01                    cliRestrictionOption [2] temporaryDefaultRestricted
	A8 05 03 03 01 80 00              odb-Data [8] { odb-GeneralData: allOG-CallsBarred, 15 bits }
	AA 04 04 02 00 01                 regionalSubscriptionData [10]
	98 01 01                          networkAccessMode [24] onlyCircuit
	93 02 06 80                       accessRestrictionData [19] utranNotAllowed`

func goldenSubscriberData() SubscriberData {
	msisdn, ftn := intl("905312345678"), intl("905312345699")
	granted, circuit := ServiceGranted, OnlyCircuit
	noReply, clir := 20, 1
	return SubscriberData{
		MSISDN: &msisdn, Category: []byte{0x0A}, SubscriberStatus: &granted,
		BearerServices: [][]byte{{0x16}}, Teleservices: [][]byte{{0x11}, {0x21}},
		ProvisionedSS: []SSInfo{
			{Forwarding: &ForwardingInfo{SSCode: 0x2A, Features: []ForwardingFeature{{
				BasicService: &BasicServiceCode{Code: []byte{0x10}}, SSStatus: 0x07, ForwardedToNumber: &ftn,
				ForwardingOptions: []byte{0x88}, NoReplyConditionTime: &noReply,
			}}}},
			{CallBarring: &CallBarringInfo{SSCode: 0x92, Features: []CallBarringFeature{{SSStatus: 0x04}}}},
			{SSData: &SSData{SSCode: 0x12, SSStatus: 0x05, CLIRestrictionOption: &clir}},
		},
		ODB:                   &ODBData{General: NamedBits{0}},
		RegionalSubscription:  [][]byte{{0x00, 0x01}},
		NetworkAccessMode:     &circuit,
		AccessRestrictionData: NamedBits{0},
	}
}

func TestInsertSubscriberDataArg_Golden(t *testing.T) {
	want := goldenFrame(t, goldenInsertSubscriberDataArg)
	require.Len(t, want, 2+0x67)
	data := goldenSubscriberData()
	b, err := EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{Data: data})
	require.NoError(t, err)
	assert.Equal(t, want, b)
	got, err := DecodeInsertSubscriberDataArg(want)
	require.NoError(t, err)
	assert.Equal(t, &InsertSubscriberDataArg{Data: data}, got)

	// Stand-alone, the IMSI comes first: imsi [0].
	b, err = EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{IMSI: goldenIMSI, Data: SubscriberData{Category: []byte{0x0A}}})
	require.NoError(t, err)
	assert.Equal(t, hexFrame(t, `30 0D 80 08 82 06 01 00 00 00 21 F3 82 01 0A`), b)
}

// The VLR-side decoder holds the gateway to declaration order: the
// extension fields are not in tag order.
func TestInsertSubscriberDataArg_DeclarationOrder(t *testing.T) {
	_, err := DecodeInsertSubscriberDataArg(hexFrame(t, `30 07 93 02 06 80 98 01 01`))
	assert.ErrorContains(t, err, "out of declaration order")
	_, err = DecodeInsertSubscriberDataArg(hexFrame(t, `30 06 83 01 00 82 01 0A`))
	assert.ErrorContains(t, err, "out of declaration order")
}

func TestInsertSubscriberDataArg_Limits(t *testing.T) {
	long := intl("90531234567890123") // 17 digits: 10 octets, an FTN-AddressString
	d := SubscriberData{ProvisionedSS: []SSInfo{{Forwarding: &ForwardingInfo{SSCode: 0x29,
		Features: []ForwardingFeature{{SSStatus: 0x07, ForwardedToNumber: &long}}}}}}
	b, err := EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{Data: d})
	require.NoError(t, err)
	assert.Contains(t, hex.EncodeToString(b), "8a0a91", "a long number goes as longForwardedToNumber [10]")

	four, thirtyOne := 4, 31
	for name, bad := range map[string]SubscriberData{
		"category of 2 octets":  {Category: []byte{1, 2}},
		"no teleservice":        {Teleservices: [][]byte{}},
		"21 teleservices":       {Teleservices: make([][]byte, 21)},
		"zone code of 3":        {RegionalSubscription: [][]byte{{1, 2, 3}}},
		"two SS alternatives":   {ProvisionedSS: []SSInfo{{Forwarding: &ForwardingInfo{}, SSData: &SSData{}}}},
		"no forwarding feature": {ProvisionedSS: []SSInfo{{Forwarding: &ForwardingInfo{SSCode: 0x21}}}},
		"no reply time 4":       {ProvisionedSS: []SSInfo{{Forwarding: &ForwardingInfo{SSCode: 0x2A, Features: []ForwardingFeature{{NoReplyConditionTime: &four}}}}}},
		"no reply time 31":      {ProvisionedSS: []SSInfo{{Forwarding: &ForwardingInfo{SSCode: 0x2A, Features: []ForwardingFeature{{NoReplyConditionTime: &thirtyOne}}}}}},
		"ODB bit 32":            {ODB: &ODBData{General: NamedBits{32}}},
		"access restriction 8":  {AccessRestrictionData: NamedBits{8}},
	} {
		_, err := EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{Data: bad})
		assert.Error(t, err, name)
	}
}

func TestInsertSubscriberDataRes_Golden(t *testing.T) {
	b := goldenFrame(t, `30 16
		A1 03 04 01 21       teleserviceList [1]: shortMessageMT-PP not supported
		A3 03 04 01 12       ss-List [3]: clir
		84 03 01 80 00       odb-GeneralData [4]: allOG-CallsBarred
		85 01 02             regionalSubscriptionResponse [5] zoneCodesConflict
		86 02 05 E0          supportedCamelPhases [6] phases 1-3`)
	res, err := DecodeInsertSubscriberDataRes(b)
	require.NoError(t, err)
	conflict := 2
	want := &InsertSubscriberDataRes{
		Teleservices: [][]byte{{0x21}}, SSCodes: []byte{0x12}, ODBGeneralData: NamedBits{0},
		RegionalSubscriptionResponse: &conflict, SupportedCAMELPhases: NamedBits{0, 1, 2},
	}
	assert.Equal(t, want, res)
	enc, err := EncodeInsertSubscriberDataRes(want)
	require.NoError(t, err)
	assert.Equal(t, b, enc)

	res, err = DecodeInsertSubscriberDataRes(nil)
	require.NoError(t, err, "the result is optional")
	assert.Equal(t, &InsertSubscriberDataRes{}, res)
	_, err = DecodeInsertSubscriberDataRes(hexFrame(t, `30 03 85 01 07`))
	assert.Error(t, err, "regionalSubscriptionResponse out of range")
}

func TestNamedBits(t *testing.T) {
	for _, tc := range []struct {
		bits     []int
		min, max int
		want     string
	}{
		{nil, 15, 32, "01 00 00"},
		{[]int{0}, 2, 8, "06 80"},
		{[]int{0, 1, 2}, 1, 16, "05 E0"},
		{[]int{28}, 15, 32, "03 00 00 00 08"},
		{[]int{7}, 2, 8, "00 01"},
	} {
		b, err := encodeNamedBits(tc.bits, tc.min, tc.max)
		require.NoError(t, err)
		assert.Equal(t, hexFrame(t, tc.want), b, "%v", tc.bits)
		got, err := decodeNamedBits(b)
		require.NoError(t, err)
		assert.Equal(t, []int(tc.bits), got)
	}
	_, err := decodeNamedBits([]byte{0x08, 0x00})
	assert.Error(t, err, "more than 7 unused bits")
	_, err = decodeNamedBits([]byte{0x01})
	assert.Error(t, err, "unused bits without data")
}

// mobilitySeeds are the golden frames, as fuzz seeds.
func mobilitySeeds() [][]byte {
	var out [][]byte
	for _, f := range []string{goldenUpdateLocationArg, goldenProvideRoamingNumberArg, goldenInsertSubscriberDataArg,
		"30 16 A1 03 04 01 21 A3 03 04 01 12 84 03 01 80 00 85 01 02 86 02 05 E0",
		"A3 15 30 10 04 08 82 06 01 00 00 00 21 F3 04 04 01 02 03 04 0A 01 01",
		"30 14 04 07 91 09 35 00 90 99 99 05 00 04 07 91 09 35 12 00 00 10",
		goldenSAIArgVLR, goldenSAIArgMMESGSN, goldenSAIResTriplets, goldenSAIResQuintupletEPS, goldenPurgeMSArg,
		"30 04 80 00 82 00"} {
		var words []string
		for _, line := range strings.Split(f, "\n") {
			for _, w := range strings.Fields(line) {
				if _, err := hex.DecodeString(w); err != nil || len(w) != 2 || strings.ToUpper(w) != w {
					break
				}
				words = append(words, w)
			}
		}
		b, _ := hex.DecodeString(strings.Join(words, ""))
		out = append(out, b)
	}
	return out
}

func TestSplitSubscriberData(t *testing.T) {
	data := goldenSubscriberData()
	whole, err := EncodeInsertSubscriberDataArg(&InsertSubscriberDataArg{Data: data})
	require.NoError(t, err)
	under := func(n int) func([]byte) (bool, error) {
		return func(b []byte) (bool, error) { return len(b) <= n, nil }
	}

	parts, err := SplitSubscriberData(&data, under(len(whole)))
	require.NoError(t, err)
	assert.Equal(t, [][]byte{whole}, parts, "what fits goes whole")

	parts, err = SplitSubscriberData(&data, under(60))
	require.NoError(t, err)
	require.Greater(t, len(parts), 1)
	var merged SubscriberData
	for i, p := range parts {
		assert.LessOrEqual(t, len(p), 60)
		arg, err := DecodeInsertSubscriberDataArg(p)
		require.NoError(t, err)
		d := arg.Data
		if i > 0 {
			assert.Nil(t, d.MSISDN, "the subscriber's own data is in the first part only")
			assert.Nil(t, d.NetworkAccessMode, "networkAccessMode is in the first part only")
		}
		if d.MSISDN != nil {
			merged = d
			merged.BearerServices, merged.Teleservices, merged.ProvisionedSS = nil, nil, nil
		}
		if d.BearerServices != nil {
			merged.BearerServices = d.BearerServices
		}
		if d.Teleservices != nil {
			merged.Teleservices = d.Teleservices
		}
		merged.ProvisionedSS = append(merged.ProvisionedSS, d.ProvisionedSS...)
	}
	assert.Equal(t, data, merged, "nothing lost, nothing repeated, in order")

	_, err = SplitSubscriberData(&data, under(30))
	assert.Error(t, err, "the forwarding information alone is over 30 octets")
	_, err = SplitSubscriberData(&data, under(10))
	assert.Error(t, err, "the subscriber's own data is over 10 octets")
}
