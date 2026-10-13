// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelectSLS(t *testing.T) {
	h := &Handler{}
	called := &SccpAddress{PointCode: 200, SubsystemNumber: SSNHLR}
	calling := &SccpAddress{PointCode: 100, SubsystemNumber: SSNMAP, GlobalTitle: &GlobalTitle{Address: "905900000001"}}

	t.Run("class 0 spreads over the links", func(t *testing.T) {
		seen := map[uint8]bool{}
		for i := 0; i < 32; i++ {
			sls := h.selectSLS(&SccpMessage{ProtocolClass: ClassBasicConnectionless, CalledAddress: called, CallingAddress: calling})
			assert.LessOrEqual(t, sls, uint8(slsMask))
			seen[sls] = true
		}
		assert.Len(t, seen, 16, "every 4-bit SLS is used")
	})

	t.Run("class 1 keeps one SLS per address pair", func(t *testing.T) {
		msg := &SccpMessage{ProtocolClass: ClassInSequenceConnectionless, CalledAddress: called, CallingAddress: calling}
		first := h.selectSLS(msg)
		for i := 0; i < 8; i++ {
			assert.Equal(t, first, h.selectSLS(msg))
		}
		assert.LessOrEqual(t, first, uint8(slsMask))
	})
}
