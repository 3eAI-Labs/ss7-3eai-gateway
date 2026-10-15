// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/cap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// loadExampleConfig reads config.example.yaml from the repo root.
// _, file, _, ok := runtime.Caller(0) gives this file's path; repo root is
// two parents up (cmd/gateway → repo).
func loadExampleConfig(t *testing.T) *viper.Viper {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")

	v := viper.New()
	v.SetConfigFile(filepath.Join(repoRoot, "config.example.yaml"))
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadInConfig(), "read config.example.yaml")
	return v
}

// TestSlideD_SS7StackConstructionFromExampleConfig proves Slice D wiring:
// the SS7 stack (M3UA → SCCP → MAP/CAP → TCAP-with-dispatcher) instantiates
// successfully against the shipped example config, with MAP registered for
// the short-message contexts and CAP for ACCapV2/V3. Excludes NATS/Firewall/TDR/API
// because those need external systems; this is a pure construction smoke test.
func TestSliceD_SS7StackConstructionFromExampleConfig(t *testing.T) {
	cfg := loadExampleConfig(t)
	logger := zaptest.NewLogger(t)

	m3uaH, err := m3ua.NewHandler(buildM3UAConfig(cfg), logger.Named("m3ua"))
	require.NoError(t, err, "m3ua.NewHandler")
	require.NotNil(t, m3uaH)

	sccpCfg, err := buildSCCPConfig(cfg)
	require.NoError(t, err, "buildSCCPConfig")
	sccpH, err := sccp.NewHandler(
		sccpCfg,
		m3uaH.IncomingMessages(),
		m3uaH.OutgoingMessages(),
		logger.Named("sccp"),
	)
	require.NoError(t, err, "sccp.NewHandler")
	require.NotNil(t, sccpH)

	mapCfg, err := buildMAPConfig(cfg)
	require.NoError(t, err, "buildMAPConfig")
	mapH, err := mapprotocol.NewHandler(mapCfg, logger.Named("map"))
	require.NoError(t, err, "mapprotocol.NewHandler")
	require.NotNil(t, mapH)

	capH, err := cap.NewHandler(buildCAPConfig(cfg), logger.Named("cap"))
	require.NoError(t, err, "cap.NewHandler")
	require.NotNil(t, capH)

	dispatcher := tcap.NewDispatcher(logger.Named("tcap-dispatch"))
	for _, ac := range mapApplicationContexts {
		require.NoError(t, dispatcher.Register(ac.OID, mapH), "register MAP %s", ac.Name)
	}
	for _, ac := range capApplicationContexts {
		require.NoError(t, dispatcher.Register(ac.OID, capH), "register CAP %s", ac.Name)
	}

	tcapH, err := tcap.NewHandler(
		buildTCAPConfig(cfg),
		sccpH.TcapIncomingChannel(),
		sccpH.TcapOutgoingChannel(),
		dispatcher,
		logger.Named("tcap"),
	)
	require.NoError(t, err, "tcap.NewHandler")
	require.NotNil(t, tcapH)

	// Strict-mode dispatcher: registered OIDs resolve, unknown OIDs do not.
	for _, ac := range mapApplicationContexts {
		require.NotNil(t, dispatcher.LookupHandler(ac.OID), "%s should resolve", ac.Name)
	}
	require.Nil(t, dispatcher.LookupHandler(tcap.ACMapV3.OID),
		"networkLocUp is not implemented, so it must not resolve")
	require.NotNil(t, dispatcher.LookupHandler(tcap.ACCapV2.OID), "CAP v2 should resolve")
	require.Nil(t, dispatcher.LookupHandler([]int{0, 4, 0, 0, 1, 0, 99, 99}),
		"unknown AC must not resolve (carrier-grade strict mode)")
}
