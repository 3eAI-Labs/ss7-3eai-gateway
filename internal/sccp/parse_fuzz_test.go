// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"
)

// FuzzParseSCCP: what arrives from the SG must never panic the parser.
func FuzzParseSCCP(f *testing.F) {
	msg := &SccpMessage{
		MessageType:    MsgTypeUDT,
		CalledAddress:  &SccpAddress{AddressIndicator: AISSNPresent, SubsystemNumber: SSNMSC, GlobalTitle: &GlobalTitle{GTI: GTI0100, NumberingPlan: NPISDN, EncodingScheme: ESBCD, NAI: NAIInternational, Address: "905"}},
		CallingAddress: &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: 2, SubsystemNumber: SSNHLR},
		Data:           []byte{0x62, 0x00},
	}
	for _, v := range []NetworkVariant{VariantITU, VariantANSI} {
		for _, mt := range []MessageType{MsgTypeUDT, MsgTypeXUDT, MsgTypeUDTS} {
			msg.MessageType = mt
			if b, err := EncodeSCCP(msg, v); err == nil {
				f.Add(b, uint8(v))
			}
		}
	}
	msg.MessageType = MsgTypeXUDT
	msg.Segmentation = &Segmentation{First: true, Remaining: 2, LocalRef: 7}
	if b, err := EncodeSCCP(msg, VariantITU); err == nil {
		f.Add(b, uint8(VariantITU))
	}
	f.Fuzz(func(t *testing.T, data []byte, v uint8) {
		_, _ = ParseSCCP(data, NetworkVariant(v%2))
	})
}
