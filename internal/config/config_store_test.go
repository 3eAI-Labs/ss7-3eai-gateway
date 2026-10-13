// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// startTestNATSServer starts an embedded NATS server for testing
func startTestNATSServer(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()

	opts := &server.Options{
		JetStream: true,
		Port:      -1, // Random port
		StoreDir:  t.TempDir(),
	}

	ns, err := server.NewServer(opts)
	require.NoError(t, err, "Failed to create NATS server")

	go ns.Start()

	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err, "Failed to connect to NATS")

	t.Cleanup(func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	return ns, nc
}

func TestNewConfigStore(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)

	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)
	require.NotNil(t, cs)

	assert.NotNil(t, cs.nc)
	assert.NotNil(t, cs.js)
	assert.NotNil(t, cs.kv)
	assert.NotNil(t, cs.logger)
}

func TestRoutingRules_AddGetDelete(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)
	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Test: Get empty rules
	rules, err := cs.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)

	// Test: Add rule
	rule1 := &RoutingRule{
		ID:          "route-1",
		Pattern:     "^1234.*",
		Destination: "10.5.0.100",
		Priority:    100,
		Enabled:     true,
	}
	err = cs.AddRoutingRule(ctx, rule1)
	require.NoError(t, err)

	// Test: Get rules
	rules, err = cs.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "route-1", rules[0].ID)
	assert.Equal(t, "^1234.*", rules[0].Pattern)
	assert.Equal(t, "10.5.0.100", rules[0].Destination)
	assert.Equal(t, 100, rules[0].Priority)
	assert.True(t, rules[0].Enabled)

	// Test: Add another rule
	rule2 := &RoutingRule{
		ID:          "route-2",
		Pattern:     "^5678.*",
		Destination: "10.5.0.200",
		Priority:    200,
		Enabled:     false,
	}
	err = cs.AddRoutingRule(ctx, rule2)
	require.NoError(t, err)

	// Test: Get all rules
	rules, err = cs.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 2)

	// Test: Add duplicate rule (should fail)
	err = cs.AddRoutingRule(ctx, rule1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Test: Delete rule
	err = cs.DeleteRoutingRule(ctx, "route-1")
	require.NoError(t, err)

	rules, err = cs.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "route-2", rules[0].ID)

	// Test: Delete non-existent rule (should fail)
	err = cs.DeleteRoutingRule(ctx, "route-999")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestFirewallRules_AddGetDelete(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)
	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Test: Get empty rules
	rules, err := cs.GetFirewallRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)

	// Test: Add rule
	rule1 := &FirewallRule{
		ID:       "fw-1",
		Type:     "blacklist",
		Pattern:  "^666.*",
		Action:   "block",
		Priority: 100,
		Enabled:  true,
	}
	err = cs.AddFirewallRule(ctx, rule1)
	require.NoError(t, err)

	// Test: Get rules
	rules, err = cs.GetFirewallRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "fw-1", rules[0].ID)
	assert.Equal(t, "blacklist", rules[0].Type)
	assert.Equal(t, "block", rules[0].Action)

	// Test: Add duplicate (should fail)
	err = cs.AddFirewallRule(ctx, rule1)
	assert.Error(t, err)

	// Test: Delete rule
	err = cs.DeleteFirewallRule(ctx, "fw-1")
	require.NoError(t, err)

	rules, err = cs.GetFirewallRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestGTTRules_AddGetDelete(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)
	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Test: Get empty rules
	rules, err := cs.GetGTTRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)

	// Test: Add rule
	rule1 := &GTTRule{
		ID:          "gtt-1",
		Input:       "1234567890",
		Output:      "0987654321",
		Translation: "prefix-swap",
		Priority:    100,
		Enabled:     true,
	}
	err = cs.AddGTTRule(ctx, rule1)
	require.NoError(t, err)

	// Test: Get rules
	rules, err = cs.GetGTTRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "gtt-1", rules[0].ID)
	assert.Equal(t, "1234567890", rules[0].Input)
	assert.Equal(t, "0987654321", rules[0].Output)

	// Test: Delete rule
	err = cs.DeleteGTTRule(ctx, "gtt-1")
	require.NoError(t, err)

	rules, err = cs.GetGTTRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestConfigStore_Watch(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)
	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Channel to receive watch notifications
	changes := make(chan string, 10)

	// Start watching
	err = cs.Watch(ctx, func(key string) {
		changes <- key
	})
	require.NoError(t, err)

	// Give watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Add a routing rule
	rule := &RoutingRule{
		ID:          "route-watch-1",
		Pattern:     "^9999.*",
		Destination: "10.5.0.99",
		Priority:    50,
		Enabled:     true,
	}
	err = cs.AddRoutingRule(ctx, rule)
	require.NoError(t, err)

	// Wait for notification
	select {
	case key := <-changes:
		assert.Equal(t, RoutingRulesKey, key)
	case <-time.After(2 * time.Second):
		t.Fatal("Did not receive watch notification")
	}
}

func TestConfigStore_Concurrency(t *testing.T) {
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)
	cs, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Concurrent writes
	done := make(chan bool)
	errors := make(chan error, 10)

	for i := 0; i < 10; i++ {
		go func(id int) {
			rule := &RoutingRule{
				ID:          formatRouteID(id),
				Pattern:     formatPattern(id),
				Destination: "10.5.0.1",
				Priority:    id,
				Enabled:     true,
			}
			if err := cs.AddRoutingRule(ctx, rule); err != nil {
				errors <- err
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Check for errors
	select {
	case err := <-errors:
		t.Fatalf("Concurrent write error: %v", err)
	default:
		// No errors
	}

	// Verify all rules were added
	rules, err := cs.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 10)
}

// Helper functions
func formatRouteID(id int) string {
	return fmt.Sprintf("route-%d", id)
}

func formatPattern(id int) string {
	return fmt.Sprintf("^%d.*", id)
}

func TestConfigStore_MultipleInstances(t *testing.T) {
	// This tests that multiple ConfigStore instances share the same KV bucket
	_, nc := startTestNATSServer(t)
	logger := zaptest.NewLogger(t)

	// Create first instance
	cs1, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	// Create second instance (simulating another gateway node)
	cs2, err := NewConfigStore(nc, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Add rule via cs1
	rule := &RoutingRule{
		ID:          "shared-route",
		Pattern:     "^8888.*",
		Destination: "10.5.0.88",
		Priority:    88,
		Enabled:     true,
	}
	err = cs1.AddRoutingRule(ctx, rule)
	require.NoError(t, err)

	// Verify via cs2 (should see the same rule)
	rules, err := cs2.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)
	assert.Equal(t, "shared-route", rules[0].ID)

	// Delete via cs2
	err = cs2.DeleteRoutingRule(ctx, "shared-route")
	require.NoError(t, err)

	// Verify deletion via cs1
	rules, err = cs1.GetRoutingRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, rules)
}
