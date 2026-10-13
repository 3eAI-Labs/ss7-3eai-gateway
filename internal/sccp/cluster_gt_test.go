// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// Cluster GT model (ADR-006) contract:
//   - LocalGlobalTitle = per-node sticky CgPA
//   - ClusterGlobalTitle = cluster-shared inbound GT
//   - isLocalAddress accepts inbound CdPA on either GT identity OR the
//     classic PC+SSN path. SG-side delivery may use any of the three.
//   - matchesOurGT distinguishes the two roles for log/metric attribution.
//   - validateConfig rejects same digits in both roles (would defeat
//     sticky routing).

func newMatcherHandler(t *testing.T, local, cluster string) *Handler {
	t.Helper()
	in, out := make(chan []byte, 4), make(chan *struct{}, 4)
	_ = in
	_ = out
	h := &Handler{
		config: &Config{
			LocalPointCode:     0x010101,
			LocalSubsystems:    []SubsystemNumber{SSNHLR}, // SSNMAP is an alias (both 0x06)
			LocalGlobalTitle:   GTConfig{Address: local},
			ClusterGlobalTitle: GTConfig{Address: cluster},
		},
		subsystems: map[SubsystemNumber]*SubsystemStatus{
			SSNHLR: {Available: true},
		},
		logger: zaptest.NewLogger(t),
	}
	return h
}

func addrPC(pc uint32, ssn SubsystemNumber) *SccpAddress {
	return &SccpAddress{
		AddressIndicator: AISSNPresent | AIPCPresent,
		PointCode:        pc,
		SubsystemNumber:  ssn,
	}
}

func addrGT(gtDigits string, ssn SubsystemNumber) *SccpAddress {
	return &SccpAddress{
		AddressIndicator: AISSNPresent | AIGTPresent,
		SubsystemNumber:  ssn,
		GlobalTitle: &GlobalTitle{
			GTI:           GTI0100,
			NumberingPlan: NPISDN,
			NAI:           NAIInternational,
			Address:       gtDigits,
		},
	}
}

func TestMatchesOurGT_NoneWhenNoGT(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	assert.Equal(t, gtRoleNone, h.matchesOurGT(nil))
	assert.Equal(t, gtRoleNone, h.matchesOurGT(&GlobalTitle{Address: ""}))
}

func TestMatchesOurGT_LocalRole(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	gt := &GlobalTitle{Address: "905900000001"}
	assert.Equal(t, gtRoleLocal, h.matchesOurGT(gt),
		"digits matching local_global_title resolve as local role")
}

func TestMatchesOurGT_ClusterRole(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	gt := &GlobalTitle{Address: "905900000000"}
	assert.Equal(t, gtRoleCluster, h.matchesOurGT(gt),
		"digits matching cluster_global_title resolve as cluster role")
}

func TestMatchesOurGT_NoneWhenForeignDigits(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	gt := &GlobalTitle{Address: "447700000000"} // foreign HLR GT
	assert.Equal(t, gtRoleNone, h.matchesOurGT(gt))
}

func TestMatchesOurGT_LocalOnlyWhenClusterEmpty(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "")
	assert.Equal(t, gtRoleLocal, h.matchesOurGT(&GlobalTitle{Address: "905900000001"}))
	assert.Equal(t, gtRoleNone, h.matchesOurGT(&GlobalTitle{Address: "905900000000"}),
		"empty cluster_gt config means cluster role is never matched")
}

// isLocalAddress contract: PC+SSN match OR GT match (either role) +
// available SSN qualifies the message for local delivery.

func TestIsLocalAddress_ClassicPCSSNMatch(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	addr := addrPC(0x010101, SSNHLR)
	assert.True(t, h.isLocalAddress(addr),
		"PC+SSN match still wins (SG-side GTT-translated UDT path)")
}

func TestIsLocalAddress_LocalGTMatch_NoPCSet(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	addr := addrGT("905900000001", SSNHLR)
	addr.PointCode = 0 // SG delivered without setting PC, only GT
	assert.True(t, h.isLocalAddress(addr),
		"local_gt match qualifies even when CdPA carries no PC")
}

func TestIsLocalAddress_ClusterGTMatch_NoPCSet(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	addr := addrGT("905900000000", SSNHLR)
	addr.PointCode = 0
	assert.True(t, h.isLocalAddress(addr),
		"cluster_gt match qualifies — new dialogue from foreign peer loadshared to us")
}

func TestIsLocalAddress_ForeignGT_NotLocal(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	addr := addrGT("447700000000", SSNHLR)
	addr.PointCode = 0
	assert.False(t, h.isLocalAddress(addr),
		"foreign GT with no PC match — must route via GTT, not deliver local")
}

func TestIsLocalAddress_GTMatchButSSNUnavailable(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	h.subsystems[SSNHLR].Available = false
	addr := addrGT("905900000001", SSNHLR)
	assert.False(t, h.isLocalAddress(addr),
		"GT match must still respect SSN availability — outage signals propagate")
}

func TestIsLocalAddress_NilAddr(t *testing.T) {
	h := newMatcherHandler(t, "905900000001", "905900000000")
	assert.False(t, h.isLocalAddress(nil), "nil CdPA never qualifies")
}

// validateConfig contract for the cluster_gt path.

func TestValidateConfig_RejectsSameDigitsInBothRoles(t *testing.T) {
	cfg := &Config{
		LocalPointCode:     1,
		LocalSubsystems:    []SubsystemNumber{SSNHLR},
		LocalGlobalTitle:   GTConfig{Address: "905900000000"},
		ClusterGlobalTitle: GTConfig{Address: "905900000000"},
	}
	err := validateConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ")
}

func TestValidateConfig_AcceptsDistinctRoles(t *testing.T) {
	cfg := &Config{
		LocalPointCode:     1,
		LocalSubsystems:    []SubsystemNumber{SSNHLR},
		LocalGlobalTitle:   GTConfig{Address: "905900000001"},
		ClusterGlobalTitle: GTConfig{Address: "905900000000"},
	}
	require.NoError(t, validateConfig(cfg))
}

func TestValidateConfig_AcceptsLocalOnly(t *testing.T) {
	// Single-node deployment — only sticky GT, no cluster GT.
	cfg := &Config{
		LocalPointCode:   1,
		LocalSubsystems:  []SubsystemNumber{SSNHLR},
		LocalGlobalTitle: GTConfig{Address: "905900000001"},
	}
	require.NoError(t, validateConfig(cfg))
}

func TestValidateConfig_RejectsBadClusterGTDigits(t *testing.T) {
	cfg := &Config{
		LocalPointCode:     1,
		LocalSubsystems:    []SubsystemNumber{SSNHLR},
		ClusterGlobalTitle: GTConfig{Address: "9059-bad"},
	}
	err := validateConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster_global_title.address")
	assert.Contains(t, err.Error(), "E.164 digits")
}

// BuildClusterGT mirrors BuildLocalGT — same defaults, same parity logic.

func TestBuildClusterGT_DisabledWhenAddressEmpty(t *testing.T) {
	cfg := &Config{ClusterGlobalTitle: GTConfig{Address: ""}}
	assert.Nil(t, cfg.BuildClusterGT())
}

func TestBuildClusterGT_DefaultsApplied(t *testing.T) {
	cfg := &Config{ClusterGlobalTitle: GTConfig{Address: "905900000000"}}
	gt := cfg.BuildClusterGT()
	require.NotNil(t, gt)
	assert.Equal(t, GTI0100, gt.GTI)
	assert.Equal(t, NPISDN, gt.NumberingPlan)
	assert.Equal(t, NAIInternational, gt.NAI)
	assert.Equal(t, "905900000000", gt.Address)
}
