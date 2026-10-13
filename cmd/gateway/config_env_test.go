// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigEnvOverride locks the Arinna-packaging contract: secrets and
// per-pod identity injected via SS7_* env vars must override the mounted
// config.yaml (Viper precedence env > file). The Helm chart relies on this to
// keep secrets out of the ConfigMap (secretRefOnly) and to give each
// StatefulSet ordinal its own sticky-CgPA GT (ADR-006).
func TestLoadConfigEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	base := "" +
		"node_id: node-1\n" +
		"node_id_numeric: 1\n" +
		"cluster_name: ss7-cluster\n" +
		"tcap:\n  node_id: 1\n" +
		"sccp:\n  local_global_title:\n    address: \"905300000001\"\n" +
		"api:\n  jwt_secret: \"file-value\"\n" +
		"tdr:\n  hmac_secret: \"file-hmac\"\n" +
		"nats:\n  url: \"nats://file:4222\"\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("SS7_NODE_ID", "node-3")
	t.Setenv("SS7_NODE_ID_NUMERIC", "3")
	t.Setenv("SS7_TCAP_NODE_ID", "3")
	t.Setenv("SS7_SCCP_LOCAL_GLOBAL_TITLE_ADDRESS", "905300000003")
	t.Setenv("SS7_API_JWT_SECRET", "env-injected-secret-value-over-32chars!!")
	t.Setenv("SS7_TDR_HMAC_SECRET", "env-injected-hmac-value-over-32chars!!!!")
	t.Setenv("SS7_NATS_URL", "nats://env:4222")

	v, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	strChecks := map[string]string{
		"node_id":                         "node-3",
		"sccp.local_global_title.address": "905300000003",
		"api.jwt_secret":                  "env-injected-secret-value-over-32chars!!",
		"tdr.hmac_secret":                 "env-injected-hmac-value-over-32chars!!!!",
		"nats.url":                        "nats://env:4222",
	}
	for k, want := range strChecks {
		if got := v.GetString(k); got != want {
			t.Errorf("key %q = %q, want %q", k, got, want)
		}
	}
	if got := v.GetInt("node_id_numeric"); got != 3 {
		t.Errorf("node_id_numeric = %d, want 3", got)
	}
	if got := v.GetInt("tcap.node_id"); got != 3 {
		t.Errorf("tcap.node_id = %d, want 3", got)
	}
}

// TestLoadConfigFileWhenNoEnv verifies the file values survive untouched when no
// SS7_* env override is present (the smoke / single-node default path).
func TestLoadConfigFileWhenNoEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	base := "node_id: node-7\napi:\n  jwt_secret: \"only-from-file\"\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	v, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := v.GetString("node_id"); got != "node-7" {
		t.Errorf("node_id = %q, want node-7", got)
	}
	if got := v.GetString("api.jwt_secret"); got != "only-from-file" {
		t.Errorf("api.jwt_secret = %q, want only-from-file", got)
	}
}
