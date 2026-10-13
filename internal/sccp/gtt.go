// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

var (
	ErrNoMatchingRule      = errors.New("GTT: no matching rule found")
	ErrNoActiveDestination = errors.New("GTT: no active destination available")
	ErrRuleNotFound        = errors.New("GTT: rule not found")
)

// NewGTTEngine creates a new GTT engine instance
func NewGTTEngine(logger *zap.Logger) *GTTEngine {
	if logger == nil {
		logger = zap.NewNop()
	}

	return &GTTEngine{
		rules:      make([]*GTTRule, 0),
		rrCounters: make(map[uint64]*atomic.Uint64),
		metrics:    &GTTMetrics{},
		logger:     logger,
	}
}

// LoadRules loads GTT rules into the engine (replaces existing rules)
func (e *GTTEngine) LoadRules(rules []*GTTRule) error {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()

	// Validate all rules first
	for _, rule := range rules {
		if err := e.validateRule(rule); err != nil {
			return fmt.Errorf("invalid rule %d: %w", rule.ID, err)
		}
	}

	// Sort rules by priority (higher priority first)
	sortedRules := make([]*GTTRule, len(rules))
	copy(sortedRules, rules)
	sort.Slice(sortedRules, func(i, j int) bool {
		return sortedRules[i].Priority > sortedRules[j].Priority
	})

	e.rules = sortedRules

	// Initialize load balancing counters
	e.lbMu.Lock()
	e.rrCounters = make(map[uint64]*atomic.Uint64)
	for _, rule := range sortedRules {
		e.rrCounters[rule.ID] = &atomic.Uint64{}
	}
	e.lbMu.Unlock()

	e.logger.Info("Loaded GTT rules",
		zap.Int("count", len(rules)))

	return nil
}

// AddRule adds a single GTT rule
func (e *GTTEngine) AddRule(rule *GTTRule) error {
	if err := e.validateRule(rule); err != nil {
		return fmt.Errorf("invalid rule: %w", err)
	}

	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()

	// Check for duplicate ID
	for _, r := range e.rules {
		if r.ID == rule.ID {
			return fmt.Errorf("rule with ID %d already exists", rule.ID)
		}
	}

	// Add rule and re-sort by priority
	e.rules = append(e.rules, rule)
	sort.Slice(e.rules, func(i, j int) bool {
		return e.rules[i].Priority > e.rules[j].Priority
	})

	// Initialize load balancing counter
	e.lbMu.Lock()
	e.rrCounters[rule.ID] = &atomic.Uint64{}
	e.lbMu.Unlock()

	e.logger.Info("Added GTT rule",
		zap.Uint64("rule_id", rule.ID),
		zap.String("pattern", rule.Pattern),
		zap.Int("priority", rule.Priority))

	return nil
}

// RemoveRule removes a GTT rule by ID
func (e *GTTEngine) RemoveRule(ruleID uint64) error {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()

	for i, rule := range e.rules {
		if rule.ID == ruleID {
			// Remove from slice
			e.rules = append(e.rules[:i], e.rules[i+1:]...)

			// Remove load balancing counter
			e.lbMu.Lock()
			delete(e.rrCounters, ruleID)
			e.lbMu.Unlock()

			e.logger.Info("Removed GTT rule", zap.Uint64("rule_id", ruleID))
			return nil
		}
	}

	return ErrRuleNotFound
}

// GetRules returns all GTT rules (sorted by priority)
func (e *GTTEngine) GetRules() []*GTTRule {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()

	// Return a copy to prevent external modification
	rulesCopy := make([]*GTTRule, len(e.rules))
	copy(rulesCopy, e.rules)
	return rulesCopy
}

// Translate performs Global Title Translation
func (e *GTTEngine) Translate(gt *GlobalTitle) (*Destination, error) {
	startTime := time.Now()
	defer func() {
		latency := time.Since(startTime).Nanoseconds()
		e.metrics.AverageLatency.Store(uint64(latency))
	}()

	e.metrics.TranslationsTotal.Add(1)

	// Find matching rule
	rule := e.findMatchingRule(gt)
	if rule == nil {
		e.logger.Warn("No matching GTT rule found",
			zap.String("gt_address", gt.Address),
			zap.Uint8("tt", gt.TranslationType))
		e.metrics.TranslationsFailed.Add(1)
		return nil, ErrNoMatchingRule
	}

	e.logger.Debug("Matched GTT rule",
		zap.Uint64("rule_id", rule.ID),
		zap.String("pattern", rule.Pattern),
		zap.String("gt_address", gt.Address))

	// Select destination using load balancing
	dest, err := e.selectDestination(rule)
	if err != nil {
		e.logger.Error("Failed to select destination", zap.Error(err))
		e.metrics.TranslationsFailed.Add(1)
		return nil, err
	}

	e.metrics.TranslationsSuccess.Add(1)

	return dest, nil
}

// findMatchingRule finds the first matching rule (rules are sorted by priority)
func (e *GTTEngine) findMatchingRule(gt *GlobalTitle) *GTTRule {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()

	for _, rule := range e.rules {
		if !rule.Enabled {
			continue
		}

		// Match pattern (prefix match with wildcard support)
		if !e.matchPattern(gt.Address, rule.Pattern) {
			continue
		}

		// Match Translation Type if specified
		if rule.TranslationType != nil && *rule.TranslationType != gt.TranslationType {
			continue
		}

		// Match Numbering Plan if specified
		if rule.NumberingPlan != nil && *rule.NumberingPlan != gt.NumberingPlan {
			continue
		}

		// Match NAI if specified
		if rule.NAI != nil && *rule.NAI != gt.NAI {
			continue
		}

		// Rule matched
		return rule
	}

	return nil
}

// matchPattern performs pattern matching with wildcard support
func (e *GTTEngine) matchPattern(address, pattern string) bool {
	// Support wildcards: * (any digits), ? (single digit)
	if pattern == "*" {
		return true
	}

	// Simple prefix match
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(address, prefix)
	}

	// Exact match
	return address == pattern
}

// selectDestination selects a destination using load balancing algorithm
func (e *GTTEngine) selectDestination(rule *GTTRule) (*Destination, error) {
	// Get active destinations
	activeDestinations := e.getActiveDestinations(rule)
	if len(activeDestinations) == 0 {
		return nil, ErrNoActiveDestination
	}

	// Single destination - return immediately
	if len(activeDestinations) == 1 {
		dest := activeDestinations[0]
		dest.TotalRequests.Add(1)
		dest.LastUsedNanos.Store(time.Now().UnixNano())
		return dest, nil
	}

	// Multiple destinations - apply load balancing
	var dest *Destination
	var err error

	switch rule.LoadBalanceMode {
	case LBRoundRobin:
		dest, err = e.selectRoundRobin(rule, activeDestinations)
	case LBWeightedRR:
		dest, err = e.selectWeightedRoundRobin(rule, activeDestinations)
	case LBLeastConnections:
		dest, err = e.selectLeastConnections(activeDestinations)
	case LBRandom:
		dest, err = e.selectRandom(activeDestinations)
	default:
		dest, err = e.selectRoundRobin(rule, activeDestinations)
	}

	if err != nil {
		return nil, err
	}

	// Update destination statistics
	dest.TotalRequests.Add(1)
	dest.LastUsedNanos.Store(time.Now().UnixNano())

	return dest, nil
}

// getActiveDestinations returns list of active destinations
func (e *GTTEngine) getActiveDestinations(rule *GTTRule) []*Destination {
	activeDestinations := make([]*Destination, 0, len(rule.Destinations))

	for i := range rule.Destinations {
		dest := &rule.Destinations[i]
		if dest.Active {
			activeDestinations = append(activeDestinations, dest)
		}
	}

	// Sort by priority (higher priority first)
	sort.Slice(activeDestinations, func(i, j int) bool {
		return activeDestinations[i].Priority > activeDestinations[j].Priority
	})

	return activeDestinations
}

// selectRoundRobin implements round-robin load balancing
func (e *GTTEngine) selectRoundRobin(rule *GTTRule, destinations []*Destination) (*Destination, error) {
	e.lbMu.RLock()
	counter, exists := e.rrCounters[rule.ID]
	e.lbMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("round-robin counter not found for rule %d", rule.ID)
	}

	// Atomic increment and modulo
	index := counter.Add(1) % uint64(len(destinations))

	return destinations[index], nil
}

// selectWeightedRoundRobin implements weighted round-robin load balancing
func (e *GTTEngine) selectWeightedRoundRobin(rule *GTTRule, destinations []*Destination) (*Destination, error) {
	// Build weighted pool
	var totalWeight int
	for _, dest := range destinations {
		totalWeight += dest.Weight
	}

	if totalWeight == 0 {
		// No weights set, fall back to regular round-robin
		return e.selectRoundRobin(rule, destinations)
	}

	e.lbMu.RLock()
	counter, exists := e.rrCounters[rule.ID]
	e.lbMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("round-robin counter not found for rule %d", rule.ID)
	}

	// Select based on weight distribution
	currentWeight := int(counter.Add(1) % uint64(totalWeight))

	var cumulativeWeight int
	for _, dest := range destinations {
		cumulativeWeight += dest.Weight
		if currentWeight < cumulativeWeight {
			return dest, nil
		}
	}

	// Should never reach here, but return first destination as fallback
	return destinations[0], nil
}

// selectLeastConnections implements least connections load balancing
func (e *GTTEngine) selectLeastConnections(destinations []*Destination) (*Destination, error) {
	var selectedDest *Destination
	var minConnections uint64 = ^uint64(0) // Max uint64

	for _, dest := range destinations {
		connections := dest.CurrentConnections.Load()
		if connections < minConnections {
			minConnections = connections
			selectedDest = dest
		}
	}

	if selectedDest == nil {
		return nil, ErrNoActiveDestination
	}

	// Increment connection count
	selectedDest.CurrentConnections.Add(1)

	return selectedDest, nil
}

// selectRandom implements random load balancing
func (e *GTTEngine) selectRandom(destinations []*Destination) (*Destination, error) {
	if len(destinations) == 0 {
		return nil, ErrNoActiveDestination
	}

	// Use time-based seed for randomness
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	index := r.Intn(len(destinations))

	return destinations[index], nil
}

// ReleaseConnection decrements connection count (for LeastConnections algorithm)
func (e *GTTEngine) ReleaseConnection(dest *Destination) {
	if dest.CurrentConnections.Load() > 0 {
		dest.CurrentConnections.Add(^uint64(0)) // Subtract 1
	}
}

// GetMetrics returns GTT metrics
func (e *GTTEngine) GetMetrics() *GTTMetrics {
	return e.metrics
}

// validateRule validates GTT rule
func (e *GTTEngine) validateRule(rule *GTTRule) error {
	if rule.ID == 0 {
		return errors.New("rule ID must be non-zero")
	}
	if rule.Pattern == "" {
		return errors.New("pattern is required")
	}
	if len(rule.Destinations) == 0 {
		return errors.New("at least one destination is required")
	}

	// Validate destinations. Index-based iteration — Destination embeds
	// atomic counters (CurrentConnections / TotalRequests) which carry
	// sync/atomic's noCopy guard; range-by-value would copy the lock.
	for i := range rule.Destinations {
		dest := &rule.Destinations[i]
		if dest.PointCode == 0 {
			return fmt.Errorf("destination %d: point code is required", i)
		}
		if rule.LoadBalanceMode == LBWeightedRR && dest.Weight == 0 {
			return fmt.Errorf("destination %d: weight must be > 0 for weighted round-robin", i)
		}
	}

	return nil
}

// SetDestinationStatus updates destination availability
func (e *GTTEngine) SetDestinationStatus(ruleID uint64, destPC uint32, active bool) error {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()

	for _, rule := range e.rules {
		if rule.ID == ruleID {
			for i := range rule.Destinations {
				if rule.Destinations[i].PointCode == destPC {
					rule.Destinations[i].Active = active
					e.logger.Info("Updated destination status",
						zap.Uint64("rule_id", ruleID),
						zap.Uint32("dest_pc", destPC),
						zap.Bool("active", active))
					return nil
				}
			}
			return fmt.Errorf("destination %d not found in rule %d", destPC, ruleID)
		}
	}

	return ErrRuleNotFound
}

// GetRuleByID returns a GTT rule by ID
func (e *GTTEngine) GetRuleByID(ruleID uint64) (*GTTRule, error) {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()

	for _, rule := range e.rules {
		if rule.ID == ruleID {
			// Return a copy
			ruleCopy := *rule
			return &ruleCopy, nil
		}
	}

	return nil, ErrRuleNotFound
}

// GetDestinationStats returns statistics for all destinations in a rule
func (e *GTTEngine) GetDestinationStats(ruleID uint64) ([]DestinationStats, error) {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()

	for _, rule := range e.rules {
		if rule.ID == ruleID {
			stats := make([]DestinationStats, len(rule.Destinations))
			// Index-based iteration: Destination embeds atomic counters
			// (noCopy); range-by-value would copy the lock.
			for i := range rule.Destinations {
				dest := &rule.Destinations[i]
				stats[i] = DestinationStats{
					PointCode:          dest.PointCode,
					SSN:                dest.SSN,
					Weight:             dest.Weight,
					Priority:           dest.Priority,
					Active:             dest.Active,
					CurrentConnections: dest.CurrentConnections.Load(),
					TotalRequests:      dest.TotalRequests.Load(),
					LastUsed:           time.Unix(0, dest.LastUsedNanos.Load()),
				}
			}
			return stats, nil
		}
	}

	return nil, ErrRuleNotFound
}

// DestinationStats holds destination statistics
type DestinationStats struct {
	PointCode          uint32
	SSN                SubsystemNumber
	Weight             int
	Priority           int
	Active             bool
	CurrentConnections uint64
	TotalRequests      uint64
	LastUsed           time.Time
}
