// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateConfig_SRIRouting(t *testing.T) {
	cfg := &Config{}
	assert.NoError(t, validateConfig(cfg), "no HLR needed with the default")
	assert.Equal(t, SRIRoutingMSISDN, cfg.SRIRouting, "default is msisdn")

	assert.Error(t, validateConfig(&Config{SRIRouting: SRIRoutingHLR}), "hlr routing needs an HLR")
	assert.NoError(t, validateConfig(&Config{SRIRouting: SRIRoutingHLR, HLRPointCode: 2}))
	assert.NoError(t, validateConfig(&Config{SRIRouting: SRIRoutingHLR, HLRGlobalTitle: "905009990001"}))
	assert.Error(t, validateConfig(&Config{SRIRouting: "gt"}))
}
