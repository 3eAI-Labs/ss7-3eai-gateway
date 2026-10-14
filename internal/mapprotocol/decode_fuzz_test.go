// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import "testing"

// FuzzDecoders: every decoder that runs on bytes a peer sent must return an
// error, never panic, on malformed input.
func FuzzDecoders(f *testing.F) {
	if b, err := EncodeSendRoutingInfoForSMArg(&SendRoutingInfoForSMRequest{
		MSISDN: "905321112233", SMRPPRI: true,
		ServiceCentreAddress: AddressString{Nature: NatureInternational, NumberPlan: NumberingPlanISDN, Digits: "905001112233"},
	}); err == nil {
		f.Add(b)
	}
	f.Add([]byte{0x30, 0x03, 0x80, 0x01, 0x09})
	for _, seed := range append(locationSeeds(), routingSeeds()...) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeTBCD(b)
		_, _ = decodeAddressString(b)
		_, _ = decodeIMSI(b)
		_, _ = decodeUpdateLocationArg(b)
		_, _ = decodeCancelLocationArg(b)
		_, _ = DecodeSendRoutingInfoArg(b)
		_, _ = DecodeSendRoutingInfoRes(b)
		_, _ = decodeProvideRoamingNumberArg(b)
		_, _ = DecodeAnyTimeInterrogationRes(b)
		_, _ = DecodeProvideSubscriberInfoRes(b)
		_, _ = DecodeAnyTimeInterrogationArg(b)
		_, _ = DecodeProvideSubscriberInfoArg(b)
		_, _ = decodeSendRoutingInfoForSMArg(b)
		_, _ = decodeSendRoutingInfoForSMRes(b)
		_, _ = decodeMtForwardSMArg(b)
		_, _ = decodeMtForwardSMRes(b)
		_, _ = decodeMoForwardSMArg(b)
		_, _ = decodeMoForwardSMRes(b)
	})
}
