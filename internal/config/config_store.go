// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// ConfigStore manages configuration using NATS JetStream KV
type ConfigStore struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	kv     nats.KeyValue
	logger *zap.Logger
	mu     sync.RWMutex
}

// Config represents gateway configuration
type Config struct {
	// Routing Rules
	RoutingRules []*RoutingRule `json:"routing_rules,omitempty"`

	// Firewall Rules
	FirewallRules []*FirewallRule `json:"firewall_rules,omitempty"`

	// GTT Rules
	GTTRules []*GTTRule `json:"gtt_rules,omitempty"`
}

type RoutingRule struct {
	ID          string `json:"id"`
	Pattern     string `json:"pattern"`
	Destination string `json:"destination"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
}

type FirewallRule struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Pattern  string `json:"pattern"`
	Action   string `json:"action"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
}

type GTTRule struct {
	ID          string `json:"id"`
	Input       string `json:"input"`
	Output      string `json:"output"`
	Translation string `json:"translation"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
}

const (
	ConfigBucketName = "ss7-config"

	// Keys
	RoutingRulesKey  = "routing-rules"
	FirewallRulesKey = "firewall-rules"
	GTTRulesKey      = "gtt-rules"
)

// NewConfigStore creates a new NATS-backed configuration store
func NewConfigStore(nc *nats.Conn, logger *zap.Logger) (*ConfigStore, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	// Wait for JetStream to be ready (max 10 attempts, 1 second each)
	var jsInfo *nats.AccountInfo
	for i := 0; i < 10; i++ {
		jsInfo, err = js.AccountInfo()
		if err == nil {
			break
		}
		logger.Warn("JetStream not ready yet, retrying...",
			zap.Int("attempt", i+1),
			zap.Error(err))
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("JetStream not available after 10 attempts: %w", err)
	}

	logger.Info("JetStream is ready",
		zap.Int("streams", jsInfo.Streams),
		zap.Int("consumers", jsInfo.Consumers))

	// Create or get KV bucket for configuration
	kv, err := js.KeyValue(ConfigBucketName)
	if err != nil {
		// Create new bucket
		logger.Info("Creating NATS KV bucket for configuration",
			zap.String("bucket", ConfigBucketName))

		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:      ConfigBucketName,
			Description: "SS7 Gateway Configuration",
			TTL:         0, // No TTL for config (permanent)
			Replicas:    1, // 1 replica for standalone NATS, 3+ for clustered
			Storage:     nats.FileStorage,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create KV bucket: %w", err)
		}
	}

	cs := &ConfigStore{
		nc:     nc,
		js:     js,
		kv:     kv,
		logger: logger,
	}

	logger.Info("ConfigStore initialized with NATS JetStream KV",
		zap.String("bucket", ConfigBucketName))

	return cs, nil
}

// GetRoutingRules retrieves all routing rules
func (cs *ConfigStore) GetRoutingRules(ctx context.Context) ([]*RoutingRule, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	entry, err := cs.kv.Get(RoutingRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*RoutingRule{}, nil
		}
		return nil, fmt.Errorf("failed to get routing rules: %w", err)
	}

	var rules []*RoutingRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, fmt.Errorf("failed to unmarshal routing rules: %w", err)
	}

	return rules, nil
}

// AddRoutingRule adds a new routing rule
func (cs *ConfigStore) AddRoutingRule(ctx context.Context, rule *RoutingRule) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Get existing rules
	rules, err := cs.getRoutingRulesLocked()
	if err != nil {
		return err
	}

	// Check for duplicate ID
	for _, r := range rules {
		if r.ID == rule.ID {
			return fmt.Errorf("routing rule with ID %s already exists", rule.ID)
		}
	}

	// Add new rule
	rules = append(rules, rule)

	// Save back
	return cs.saveRoutingRulesLocked(rules)
}

// DeleteRoutingRule deletes a routing rule by ID
func (cs *ConfigStore) DeleteRoutingRule(ctx context.Context, id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	rules, err := cs.getRoutingRulesLocked()
	if err != nil {
		return err
	}

	// Find and remove
	newRules := make([]*RoutingRule, 0, len(rules))
	found := false
	for _, r := range rules {
		if r.ID != id {
			newRules = append(newRules, r)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("routing rule with ID %s not found", id)
	}

	return cs.saveRoutingRulesLocked(newRules)
}

// GetFirewallRules retrieves all firewall rules
func (cs *ConfigStore) GetFirewallRules(ctx context.Context) ([]*FirewallRule, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	entry, err := cs.kv.Get(FirewallRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*FirewallRule{}, nil
		}
		return nil, fmt.Errorf("failed to get firewall rules: %w", err)
	}

	var rules []*FirewallRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, fmt.Errorf("failed to unmarshal firewall rules: %w", err)
	}

	return rules, nil
}

// AddFirewallRule adds a new firewall rule
func (cs *ConfigStore) AddFirewallRule(ctx context.Context, rule *FirewallRule) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	rules, err := cs.getFirewallRulesLocked()
	if err != nil {
		return err
	}

	for _, r := range rules {
		if r.ID == rule.ID {
			return fmt.Errorf("firewall rule with ID %s already exists", rule.ID)
		}
	}

	rules = append(rules, rule)
	return cs.saveFirewallRulesLocked(rules)
}

// DeleteFirewallRule deletes a firewall rule by ID
func (cs *ConfigStore) DeleteFirewallRule(ctx context.Context, id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	rules, err := cs.getFirewallRulesLocked()
	if err != nil {
		return err
	}

	newRules := make([]*FirewallRule, 0, len(rules))
	found := false
	for _, r := range rules {
		if r.ID != id {
			newRules = append(newRules, r)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("firewall rule with ID %s not found", id)
	}

	return cs.saveFirewallRulesLocked(newRules)
}

// GetGTTRules retrieves all GTT rules
func (cs *ConfigStore) GetGTTRules(ctx context.Context) ([]*GTTRule, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	entry, err := cs.kv.Get(GTTRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*GTTRule{}, nil
		}
		return nil, fmt.Errorf("failed to get GTT rules: %w", err)
	}

	var rules []*GTTRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, fmt.Errorf("failed to unmarshal GTT rules: %w", err)
	}

	return rules, nil
}

// AddGTTRule adds a new GTT rule
func (cs *ConfigStore) AddGTTRule(ctx context.Context, rule *GTTRule) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	rules, err := cs.getGTTRulesLocked()
	if err != nil {
		return err
	}

	for _, r := range rules {
		if r.ID == rule.ID {
			return fmt.Errorf("GTT rule with ID %s already exists", rule.ID)
		}
	}

	rules = append(rules, rule)
	return cs.saveGTTRulesLocked(rules)
}

// DeleteGTTRule deletes a GTT rule by ID
func (cs *ConfigStore) DeleteGTTRule(ctx context.Context, id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	rules, err := cs.getGTTRulesLocked()
	if err != nil {
		return err
	}

	newRules := make([]*GTTRule, 0, len(rules))
	found := false
	for _, r := range rules {
		if r.ID != id {
			newRules = append(newRules, r)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("GTT rule with ID %s not found", id)
	}

	return cs.saveGTTRulesLocked(newRules)
}

// Watch watches for configuration changes
func (cs *ConfigStore) Watch(ctx context.Context, callback func(key string)) error {
	watcher, err := cs.kv.WatchAll()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}

	go func() {
		defer watcher.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case entry := <-watcher.Updates():
				if entry != nil {
					cs.logger.Info("Configuration changed",
						zap.String("key", entry.Key()),
						zap.Uint64("revision", entry.Revision()))
					callback(entry.Key())
				}
			}
		}
	}()

	return nil
}

// Helper methods (must be called with lock held)

func (cs *ConfigStore) getRoutingRulesLocked() ([]*RoutingRule, error) {
	entry, err := cs.kv.Get(RoutingRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*RoutingRule{}, nil
		}
		return nil, err
	}

	var rules []*RoutingRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func (cs *ConfigStore) saveRoutingRulesLocked(rules []*RoutingRule) error {
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("failed to marshal routing rules: %w", err)
	}

	_, err = cs.kv.Put(RoutingRulesKey, data)
	if err != nil {
		return fmt.Errorf("failed to save routing rules: %w", err)
	}

	cs.logger.Info("Routing rules updated",
		zap.Int("count", len(rules)))
	return nil
}

func (cs *ConfigStore) getFirewallRulesLocked() ([]*FirewallRule, error) {
	entry, err := cs.kv.Get(FirewallRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*FirewallRule{}, nil
		}
		return nil, err
	}

	var rules []*FirewallRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func (cs *ConfigStore) saveFirewallRulesLocked(rules []*FirewallRule) error {
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("failed to marshal firewall rules: %w", err)
	}

	_, err = cs.kv.Put(FirewallRulesKey, data)
	if err != nil {
		return fmt.Errorf("failed to save firewall rules: %w", err)
	}

	cs.logger.Info("Firewall rules updated",
		zap.Int("count", len(rules)))
	return nil
}

func (cs *ConfigStore) getGTTRulesLocked() ([]*GTTRule, error) {
	entry, err := cs.kv.Get(GTTRulesKey)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return []*GTTRule{}, nil
		}
		return nil, err
	}

	var rules []*GTTRule
	if err := json.Unmarshal(entry.Value(), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func (cs *ConfigStore) saveGTTRulesLocked(rules []*GTTRule) error {
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("failed to marshal GTT rules: %w", err)
	}

	_, err = cs.kv.Put(GTTRulesKey, data)
	if err != nil {
		return fmt.Errorf("failed to save GTT rules: %w", err)
	}

	cs.logger.Info("GTT rules updated",
		zap.Int("count", len(rules)))
	return nil
}

// Close closes the config store
func (cs *ConfigStore) Close() error {
	cs.logger.Info("ConfigStore closed")
	return nil
}
