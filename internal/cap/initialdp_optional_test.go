// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap_test

import (
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/cap"
)

// InitialDP's party numbers are OPTIONAL (TS 29.078): one without them must
// be refused as invalid, not crash the gateway while logging them.
func TestProcessInitialDP_WithoutPartyNumbers(t *testing.T) {
	h := newTestHandler(t)
	h.SetInitialDPHandler(acceptAll{})
	if err := h.ProcessInitialDP(&cap.InitialDPRequest{ServiceKey: 1}); err == nil {
		t.Fatal("an InitialDP without party numbers must be refused")
	}
}

type acceptAll struct{}

func (acceptAll) HandleInitialDP(*cap.InitialDPRequest) error { return nil }
