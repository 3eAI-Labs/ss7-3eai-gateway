// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GTConfig contract: BuildLocalGT returns nil when Address is empty
// and a fully-defaulted *GlobalTitle when Address is set. Defaults align
// with carrier-grade common case (GTI0100 / TT=0 / NPISDN / NAIIntl).
// EncodingScheme auto-flips on address parity per Q.713 §3.4.2.3.4.

func TestBuildLocalGT_DisabledWhenAddressEmpty(t *testing.T) {
	cfg := &Config{LocalGlobalTitle: GTConfig{Address: ""}}
	assert.Nil(t, cfg.BuildLocalGT(), "empty address must disable own-GT path")
}

func TestBuildLocalGT_DefaultsApplied(t *testing.T) {
	cfg := &Config{LocalGlobalTitle: GTConfig{Address: "905900000001"}}
	gt := cfg.BuildLocalGT()
	require.NotNil(t, gt)

	assert.Equal(t, GTI0100, gt.GTI, "default GTI is 0100 (TT+NP+ES+NAI)")
	assert.Equal(t, uint8(0), gt.TranslationType)
	assert.Equal(t, NPISDN, gt.NumberingPlan, "default NumberingPlan is ISDN/E.164")
	assert.Equal(t, NAIInternational, gt.NAI, "default NAI is International")
	assert.Equal(t, "905900000001", gt.Address)
	// Address has 12 digits (even) → ESBCDEven per Q.713 §3.4.2.3.4.
	assert.Equal(t, ESBCDEven, gt.EncodingScheme, "even-length address → ESBCDEven")
}

func TestBuildLocalGT_OddAddressUsesESBCD(t *testing.T) {
	cfg := &Config{LocalGlobalTitle: GTConfig{Address: "9059000001"}} // 10 digits → even
	gt := cfg.BuildLocalGT()
	require.NotNil(t, gt)
	assert.Equal(t, ESBCDEven, gt.EncodingScheme)

	cfg.LocalGlobalTitle.Address = "905900001" // 9 digits → odd
	gt = cfg.BuildLocalGT()
	require.NotNil(t, gt)
	assert.Equal(t, ESBCD, gt.EncodingScheme, "odd-length address → ESBCD")
}

func TestBuildLocalGT_OverridesHonored(t *testing.T) {
	cfg := &Config{LocalGlobalTitle: GTConfig{
		Address:         "905900000001",
		GTI:             uint8(GTI0010),
		TranslationType: 7,
		NumberingPlan:   2,
		NatureOfAddress: 3,
	}}
	gt := cfg.BuildLocalGT()
	require.NotNil(t, gt)
	assert.Equal(t, GTI0010, gt.GTI)
	assert.Equal(t, uint8(7), gt.TranslationType)
	assert.Equal(t, NumberingPlan(2), gt.NumberingPlan)
	assert.Equal(t, NatureOfAddressIndicator(3), gt.NAI)
}

// validateConfig must reject non-numeric GT addresses — catches operator
// fat-finger before the wire path tries to BCD-encode garbage.
func TestValidateConfig_RejectsNonNumericGT(t *testing.T) {
	cfg := &Config{
		LocalPointCode:   1,
		LocalSubsystems:  []SubsystemNumber{SSNHLR},
		LocalGlobalTitle: GTConfig{Address: "9059-bad"},
	}
	err := validateConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "E.164 digits")
}

func TestValidateConfig_RejectsBadGTI(t *testing.T) {
	cfg := &Config{
		LocalPointCode:   1,
		LocalSubsystems:  []SubsystemNumber{SSNHLR},
		LocalGlobalTitle: GTConfig{Address: "905900000001", GTI: 9},
	}
	err := validateConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gti")
}
