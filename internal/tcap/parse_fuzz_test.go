// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"encoding/hex"
	"testing"

	"go.uber.org/zap"
)

// FuzzParseTCAPMessage: parsing what a peer sends must never panic — a panic
// in a TCAP worker takes the whole gateway down.
func FuzzParseTCAPMessage(f *testing.F) {
	for _, s := range []string{
		"673249040a0b0c0d6b2a2828060700118605010101a01d611b80020780a109060704000001001503a203020101a305a103020102", // U-ABORT + AARE
		"670949040a0b0c0d4a0104", // P-ABORT
		"6703490101",             // short DTID
		"67026b00",               // ABORT, empty dialogue
		"6b00",                   // bare dialogue tag
	} {
		b, _ := hex.DecodeString(s)
		f.Add(b)
	}
	h := &Handler{logger: zap.NewNop()}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = h.parseTCAPMessage(data)
		_ = transactionShard(data, 4)
	})
}
