// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"bytes"
	"encoding/hex"
	"strings"
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
		"62804804000000056b80288006070011860501010100000000000000", // indefinite BEGIN
		"64811749820004000000056c810ca3830000070201ff02810122",     // non-minimal END
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

// FuzzDefiniteLengths: the rewrite never panics, and what it returns needs
// no second one — every length it leaves is definite and minimal.
func FuzzDefiniteLengths(f *testing.F) {
	for _, s := range []string{
		"30800201050000", "3081030201 05", "300301 0203", "308030030102030000", "bf1f800201050000",
	} {
		b, _ := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := definiteLengths(data)
		if err != nil {
			return
		}
		again, err := definiteLengths(out)
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("% X rewrote to % X, then to % X (%v)", data, out, again, err)
		}
	})
}
