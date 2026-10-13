// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package m3ua

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func labelTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := NewHandler(&Config{
		SGWAddress:       "127.0.0.1",
		SGWPort:          2905,
		TrafficMode:      2,
		NetworkIndicator: 2,
		ASPUPACKTimeout:  time.Second,
	}, zap.NewNop())
	require.NoError(t, err)
	h.setState(ASPStateActive)
	return h
}

// A DATA without a DPC cannot be routed by any STP; it must be refused
// before it reaches the association rather than sent with DPC 0.
func TestSendMTP3_RefusesIncompleteRoutingLabel(t *testing.T) {
	h := labelTestHandler(t)
	for _, msg := range []*MTP3Message{
		{OPC: 0, DPC: 200, SI: 3, PayloadData: []byte{0x09}},
		{OPC: 100, DPC: 0, SI: 3, PayloadData: []byte{0x09}},
	} {
		assert.ErrorIs(t, h.sendMTP3(msg), ErrInvalidRoutingLabel)
	}
	assert.ErrorIs(t, h.sendMTP3(&MTP3Message{OPC: 100, DPC: 200, SI: 3, PayloadData: []byte{0x09}}),
		ErrNotConnected, "a complete label gets as far as the missing association")
}

func TestStreamForSLS(t *testing.T) {
	assert.Equal(t, uint16(1), streamForSLS(7, 0), "nothing negotiated: stream 1")
	assert.Equal(t, uint16(1), streamForSLS(0, 9))
	assert.Equal(t, uint16(8), streamForSLS(7, 9))
	assert.Equal(t, uint16(2), streamForSLS(10, 9), "wraps over the data streams, never 0")
	assert.Equal(t, streamForSLS(5, 9), streamForSLS(5, 9), "same SLS, same stream")
}

func TestValidateConfig_NetworkIndicator(t *testing.T) {
	cfg := &Config{SGWAddress: "127.0.0.1", SGWPort: 2905, TrafficMode: 2, ASPUPACKTimeout: time.Second}
	for ni := uint8(0); ni <= 3; ni++ {
		cfg.NetworkIndicator = ni
		assert.NoError(t, validateConfig(cfg), "NI %d", ni)
	}
	cfg.NetworkIndicator = 4
	assert.Error(t, validateConfig(cfg))
}
