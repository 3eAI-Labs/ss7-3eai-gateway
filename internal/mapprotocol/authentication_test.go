// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden frames for sendAuthenticationInfo and purgeMS, built by hand from
// the TS 29.002 ASN.1 (MAP-MS-DataTypes, IMPLICIT TAGS) like those of
// mobility_test.go. Octet strings of one repeated value are written with
// rep, to keep the frames readable.

// rep is n octets of value b, as frame text.
func rep(b byte, n int) string {
	return strings.TrimSpace(strings.Repeat(fmt.Sprintf("%02X ", b), n))
}

var (
	resyncRAND = []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	resyncAUTS = []byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD}
)

// A VLR asks for three vectors, re-synchronising.
const goldenSAIArgVLR = `30 3B
	80 08 82 06 01 00 00 00 21 F3           imsi [0] 286010000000123
	02 01 03                                numberOfRequestedVectors 3
	81 00                                   immediateResponsePreferred [1]
	30 22                                   re-synchronisationInfo
	   04 10 00 11 22 33 44 55 66 77 88 99 AA BB CC DD EE FF    rand
	   04 0E A0 A1 A2 A3 A4 A5 A6 A7 A8 A9 AA AB AC AD          auts
	83 01 00                                requestingNodeType [3] vlr
	84 03 62 F2 10                          requestingPLMN-Id [4] 262-01`

// A combined MME/SGSN asks for five vectors and two more for EPS, without
// segmentation, and for the UE usage type.
const goldenSAIArgMMESGSN = `30 19
	80 08 82 06 01 00 00 00 21 F3           imsi [0]
	02 01 05                                numberOfRequestedVectors 5
	05 00                                   segmentationProhibited
	83 01 11                                requestingNodeType [3] mme-sgsn (17)
	85 01 02                                numberOfRequestedAdditional-Vectors [5] 2
	86 00                                   additionalVectorsAreForEPS [6]
	87 00                                   ueUsageTypeRequestIndication [7]`

func TestSendAuthenticationInfoArg_Golden(t *testing.T) {
	vlr, mmeSGSN := NodeVLR, NodeMMESGSN
	for name, tc := range map[string]struct {
		frame string
		want  *SendAuthenticationInfoArg
	}{
		"vlr": {goldenSAIArgVLR, &SendAuthenticationInfoArg{IMSI: goldenIMSI, NumberOfRequestedVectors: 3,
			ImmediateResponsePreferred: true, ResyncRAND: resyncRAND, ResyncAUTS: resyncAUTS,
			RequestingNodeType: &vlr, RequestingPLMNID: []byte{0x62, 0xF2, 0x10}}},
		"mme-sgsn": {goldenSAIArgMMESGSN, &SendAuthenticationInfoArg{IMSI: goldenIMSI, NumberOfRequestedVectors: 5,
			SegmentationProhibited: true, RequestingNodeType: &mmeSGSN, AdditionalVectors: 2,
			AdditionalVectorsAreForEPS: true, UEUsageTypeRequested: true}},
	} {
		t.Run(name, func(t *testing.T) {
			b := goldenFrame(t, tc.frame)
			require.Equal(t, int(b[1])+2, len(b), "the frame's length")
			arg, err := DecodeSendAuthenticationInfoArg(b)
			require.NoError(t, err)
			assert.Equal(t, tc.want, arg)
			enc, err := EncodeSendAuthenticationInfoArg(tc.want)
			require.NoError(t, err)
			assert.Equal(t, b, enc, "the peer's encoder agrees with the hand-built frame")
		})
	}
}

func TestSendAuthenticationInfoArg_Rules(t *testing.T) {
	imsi := "80 08 82 06 01 00 00 00 21 F3"
	for name, tc := range map[string]struct {
		frame   string
		node    *RequestingNodeType
		wantErr string
	}{
		"node type 9 is taken as vlr":     {frame: "30 0D " + imsi + " 02 01 01 83 01 09", node: ptr(NodeVLR)},
		"node type 32 is taken as sgsn":   {frame: "30 0D " + imsi + " 02 01 01 83 01 20", node: ptr(NodeSGSN)},
		"an unknown extension is skipped": {frame: "30 0D " + imsi + " 02 01 01 9F 30 00"},
		"no vectors":                      {frame: "30 0D " + imsi + " 02 01 00", wantErr: "outside 1..5"},
		"six vectors":                     {frame: "30 0D " + imsi + " 02 01 06", wantErr: "outside 1..5"},
		"imsi second":                     {frame: "30 0D 02 01 01 " + imsi, wantErr: "in that order"},
		"short rand":                      {frame: "30 1E " + imsi + " 02 01 01 30 11 04 0F " + rep(0, 15) + " 04 00", wantErr: "rand"},
	} {
		t.Run(name, func(t *testing.T) {
			b := hexFrame(t, tc.frame)
			b[1] = byte(len(b) - 2)
			arg, err := DecodeSendAuthenticationInfoArg(b)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.node, arg.RequestingNodeType)
		})
	}
}

func ptr[T any](v T) *T { return &v }

// Two triplets.
var goldenSAIResTriplets = `A3 4A
	A0 48                                   tripletList [0]
	   30 22
	      04 10 ` + rep(0x11, 16) + `        rand
	      04 04 21 21 21 21                 sres
	      04 08 ` + rep(0x31, 8) + `         kc
	   30 22
	      04 10 ` + rep(0x12, 16) + `        rand
	      04 04 22 22 22 22                 sres
	      04 08 ` + rep(0x32, 8) + `         kc`

// A quintuplet, an EPS vector and the UE usage type. The content is 176
// octets: the length takes the long form.
var goldenSAIResQuintupletEPS = `A3 81 B0
	A1 54                                   quintupletList [1]
	   30 52
	      04 10 ` + rep(0x41, 16) + `        rand
	      04 08 ` + rep(0x42, 8) + `         xres
	      04 10 ` + rep(0x43, 16) + `        ck
	      04 10 ` + rep(0x44, 16) + `        ik
	      04 10 ` + rep(0x45, 16) + `        autn
	A2 52                                   eps-AuthenticationSetList [2]
	   30 50
	      04 10 ` + rep(0x51, 16) + `        rand
	      04 08 ` + rep(0x52, 8) + `         xres
	      04 10 ` + rep(0x53, 16) + `        autn
	      04 20 ` + rep(0x54, 32) + `        kasme
	83 04 00 00 00 80                       ueUsageType [3]`

func TestSendAuthenticationInfoRes_Golden(t *testing.T) {
	o := bytes.Repeat
	for name, tc := range map[string]struct {
		frame string
		res   *SendAuthenticationInfoRes
	}{
		"triplets": {goldenSAIResTriplets, &SendAuthenticationInfoRes{Triplets: []AuthenticationTriplet{
			{RAND: o([]byte{0x11}, 16), SRES: o([]byte{0x21}, 4), Kc: o([]byte{0x31}, 8)},
			{RAND: o([]byte{0x12}, 16), SRES: o([]byte{0x22}, 4), Kc: o([]byte{0x32}, 8)},
		}}},
		"quintuplet, EPS vector, UE usage type": {goldenSAIResQuintupletEPS, &SendAuthenticationInfoRes{
			Quintuplets: []AuthenticationQuintuplet{{RAND: o([]byte{0x41}, 16), XRES: o([]byte{0x42}, 8),
				CK: o([]byte{0x43}, 16), IK: o([]byte{0x44}, 16), AUTN: o([]byte{0x45}, 16)}},
			EPSVectors: []EPCAV{{RAND: o([]byte{0x51}, 16), XRES: o([]byte{0x52}, 8), AUTN: o([]byte{0x53}, 16),
				KASME: o([]byte{0x54}, 32)}},
			UEUsageType: []byte{0, 0, 0, 0x80},
		}},
		"empty": {"A3 00", &SendAuthenticationInfoRes{}},
	} {
		t.Run(name, func(t *testing.T) {
			b := goldenFrame(t, tc.frame)
			enc, err := EncodeSendAuthenticationInfoRes(tc.res)
			require.NoError(t, err)
			assert.Equal(t, b, enc)
			dec, err := DecodeSendAuthenticationInfoRes(b)
			require.NoError(t, err)
			assert.Equal(t, tc.res, dec, "the peer's decoder agrees with the hand-built frame")
		})
	}
}

func TestSendAuthenticationInfoRes_Validate(t *testing.T) {
	triplet := AuthenticationTriplet{RAND: make([]byte, 16), SRES: make([]byte, 4), Kc: make([]byte, 8)}
	quintuplet := AuthenticationQuintuplet{RAND: make([]byte, 16), XRES: make([]byte, 16), CK: make([]byte, 16),
		IK: make([]byte, 16), AUTN: make([]byte, 16)}
	ask := &SendAuthenticationInfoArg{NumberOfRequestedVectors: 2}
	for name, tc := range map[string]struct {
		res     SendAuthenticationInfoRes
		arg     *SendAuthenticationInfoArg
		wantErr string
	}{
		"two triplets for two":            {res: SendAuthenticationInfoRes{Triplets: []AuthenticationTriplet{triplet, triplet}}, arg: ask},
		"three triplets for two":          {res: SendAuthenticationInfoRes{Triplets: []AuthenticationTriplet{triplet, triplet, triplet}}, arg: ask, wantErr: "allows 2"},
		"triplet and quintuplet":          {res: SendAuthenticationInfoRes{Triplets: []AuthenticationTriplet{triplet}, Quintuplets: []AuthenticationQuintuplet{quintuplet}}, wantErr: "one or the other"},
		"six in a list":                   {res: SendAuthenticationInfoRes{Triplets: slicesOf(triplet, 6)}, wantErr: "at most 5"},
		"short kc":                        {res: SendAuthenticationInfoRes{Triplets: []AuthenticationTriplet{{RAND: triplet.RAND, SRES: triplet.SRES, Kc: make([]byte, 7)}}}, wantErr: "kc of 7 octets"},
		"xres of 17":                      {res: SendAuthenticationInfoRes{Quintuplets: []AuthenticationQuintuplet{{RAND: quintuplet.RAND, XRES: make([]byte, 17), CK: quintuplet.CK, IK: quintuplet.IK, AUTN: quintuplet.AUTN}}}, wantErr: "want 4..16"},
		"unasked UE usage type":           {res: SendAuthenticationInfoRes{UEUsageType: []byte{0, 0, 0, 1}}, arg: ask, wantErr: "did not ask"},
		"additional vectors, not for EPS": {res: SendAuthenticationInfoRes{Triplets: slicesOf(triplet, 3)}, arg: &SendAuthenticationInfoArg{NumberOfRequestedVectors: 2, RequestingNodeType: ptr(NodeMMESGSN), AdditionalVectors: 3}},
		"additional vectors on a VLR":     {res: SendAuthenticationInfoRes{Triplets: slicesOf(triplet, 3)}, arg: &SendAuthenticationInfoArg{NumberOfRequestedVectors: 2, AdditionalVectors: 1}, wantErr: "allows 2"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.res.Validate(tc.arg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func slicesOf[T any](v T, n int) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// A VLR purges a subscriber; the last known location after the
// extension marker is skipped.
const goldenPurgeMSArg = `A3 18
	04 08 82 06 01 00 00 00 21 F3           imsi 286010000000123
	80 07 91 09 35 12 00 00 20              vlr-Number [0] 905321000002
	A2 03 02 01 05                          locationInformation [2] { ageOfLocationInformation 5 }`

func TestPurgeMSArg_Golden(t *testing.T) {
	b := goldenFrame(t, goldenPurgeMSArg)
	require.Len(t, b, 2+0x18)
	arg, err := DecodePurgeMSArg(b)
	require.NoError(t, err)
	vlr := intl("905321000002")
	assert.Equal(t, &PurgeMSArg{IMSI: goldenIMSI, VLRNumber: &vlr}, arg)

	enc, err := EncodePurgeMSArg(arg)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xA3, 0x13}, enc[:2])
	assert.Equal(t, b[2:2+0x13], enc[2:], "the peer's encoder agrees, up to the location")

	_, err = DecodePurgeMSArg(goldenFrame(t, "30 0A 04 08 82 06 01 00 00 00 21 F3"))
	require.ErrorContains(t, err, "[3] SEQUENCE", "the arg's tag is [3], not SEQUENCE")
}

func TestPurgeMSRes_Golden(t *testing.T) {
	for frame, res := range map[string]*PurgeMSRes{
		"30 04 80 00 82 00": {FreezeTMSI: true, FreezeMTMSI: true},
		"30 02 81 00":       {FreezePTMSI: true},
		"30 00":             {},
	} {
		b := hexFrame(t, frame)
		enc, err := EncodePurgeMSRes(res)
		require.NoError(t, err)
		assert.Equal(t, b, enc, frame)
		dec, err := DecodePurgeMSRes(b)
		require.NoError(t, err)
		assert.Equal(t, res, dec, frame)
	}
	_, err := DecodePurgeMSRes(hexFrame(t, "30 04 81 00 80 00"))
	require.Error(t, err, "freezeTMSI after freezeP-TMSI is out of order")
}
