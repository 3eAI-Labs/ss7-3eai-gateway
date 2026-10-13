// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// warnHAConfigDrift contract (ADR-006 H2): cross-layer SCCP↔M3UA config
// misalignments emit boot-time warnings without blocking startup.
// Operators occasionally have intentional reasons (migration windows,
// single-node deployment that publishes cluster_gt for future expansion),
// so this is a WARN, not an ERROR. The tests assert the right warning
// fires for the right config combination — and silence for the right
// ones — so a future refactor can't silently weaken the operator
// guidance without breaking these tests.

func observedLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, recorded := observer.New(zapcore.WarnLevel)
	return zap.New(core), recorded
}

func TestWarnHAConfigDrift_ClusterGTWithoutLoadshare(t *testing.T) {
	logger, recorded := observedLogger(t)
	sccpCfg := &sccp.Config{
		ClusterGlobalTitle: sccp.GTConfig{Address: "905900000000"},
	}
	m3uaCfg := &m3ua.Config{TrafficMode: 1} // Override — wrong for cluster

	warnHAConfigDrift(sccpCfg, m3uaCfg, logger)

	logs := recorded.All()
	assert.Len(t, logs, 1, "exactly one drift warning expected")
	assert.Contains(t, logs[0].Message, "traffic_mode is 1 (Override)")
	assert.Contains(t, logs[0].Message, "cluster_global_title set")
}

func TestWarnHAConfigDrift_LoadshareWithoutClusterGT(t *testing.T) {
	logger, recorded := observedLogger(t)
	sccpCfg := &sccp.Config{
		// ClusterGlobalTitle empty — but loadshare is on
		LocalGlobalTitle: sccp.GTConfig{Address: "905900000001"},
	}
	m3uaCfg := &m3ua.Config{TrafficMode: 2}

	warnHAConfigDrift(sccpCfg, m3uaCfg, logger)

	logs := recorded.All()
	assert.Len(t, logs, 1, "loadshare-without-cluster-gt drift warning expected")
	assert.Contains(t, logs[0].Message, "traffic_mode is 2 (Loadshare)")
	assert.Contains(t, logs[0].Message, "cluster_global_title is empty")
}

func TestWarnHAConfigDrift_AlignedClusterMode(t *testing.T) {
	// cluster_gt set + traffic_mode=2 → ADR-006 happy path. Silence.
	logger, recorded := observedLogger(t)
	sccpCfg := &sccp.Config{
		LocalGlobalTitle:   sccp.GTConfig{Address: "905900000001"},
		ClusterGlobalTitle: sccp.GTConfig{Address: "905900000000"},
	}
	m3uaCfg := &m3ua.Config{TrafficMode: 2}

	warnHAConfigDrift(sccpCfg, m3uaCfg, logger)

	assert.Empty(t, recorded.All(),
		"aligned cluster config must not emit drift warnings")
}

func TestWarnHAConfigDrift_AlignedSingleNodeMode(t *testing.T) {
	// Single-node deployment: no cluster_gt, traffic_mode=1. Silence.
	logger, recorded := observedLogger(t)
	sccpCfg := &sccp.Config{
		LocalGlobalTitle: sccp.GTConfig{Address: "905900000001"},
	}
	m3uaCfg := &m3ua.Config{TrafficMode: 1}

	warnHAConfigDrift(sccpCfg, m3uaCfg, logger)

	assert.Empty(t, recorded.All(),
		"aligned single-node config must not emit drift warnings")
}
