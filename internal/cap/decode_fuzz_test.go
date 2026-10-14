// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap

import "testing"

// FuzzDecoders: CAP decoders run on bytes a peer sent and must not panic.
func FuzzDecoders(f *testing.F) {
	f.Add([]byte{0x30, 0x03, 0x80, 0x01, 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeTBCD(b)
		_, _ = decodeAddressString(b)
		_, _ = decodeInitialDPArg(b)
		_, _ = decodeEventReportBCSMArg(b)
		_, _ = decodeApplyChargingReportArg(b)
	})
}
