// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
)

// One OPC and SLS — one stream MTP keeps in sequence — always maps to the
// same worker; different SLS values of one OPC spread.
func TestInboundShard(t *testing.T) {
	const workers = 4
	seen := map[int]bool{}
	for sls := uint8(0); sls < 16; sls++ {
		m := &m3ua.MTP3Message{OPC: 0x0102, SLS: sls}
		first := inboundShard(m, workers)
		for i := 0; i < 3; i++ {
			if got := inboundShard(&m3ua.MTP3Message{OPC: 0x0102, SLS: sls, PayloadData: []byte{byte(i)}}, workers); got != first {
				t.Fatalf("SLS %d: worker %d then %d", sls, first, got)
			}
		}
		seen[first] = true
	}
	if len(seen) != workers {
		t.Fatalf("16 SLS values used %d of %d workers", len(seen), workers)
	}
	if inboundShard(nil, workers) != 0 || inboundShard(&m3ua.MTP3Message{SLS: 3}, 1) != 0 {
		t.Fatal("degenerate cases")
	}
}
