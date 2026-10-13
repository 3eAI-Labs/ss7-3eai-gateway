// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ha

import (
	"context"
	"sync/atomic"

	"go.uber.org/zap"
)

// Handler provides simplified HA status reporting
// NOTE: Raft consensus has been replaced with NATS JetStream KV for configuration storage
// This module is kept for compatibility and basic status reporting only
type Handler struct {
	config  *Config
	logger  *zap.Logger
	state   atomic.Value // NodeState
	metrics *Metrics
}

// NewHandler creates a new simplified HA handler
func NewHandler(cfg *Config, logger *zap.Logger) (*Handler, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:  cfg,
		logger:  logger,
		metrics: &Metrics{},
	}

	// Initialize as standalone (no leader election)
	handler.state.Store(NodeStateLeader)

	return handler, nil
}

// Start starts the HA module (simplified - no Raft)
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("HA module started (NATS JetStream KV mode - no Raft consensus)",
		zap.String("node_id", h.config.NodeID))
	return nil
}

// Stop gracefully stops the HA module
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping HA module")
	h.state.Store(NodeStateShutdown)
	return nil
}

// IsLeader always returns true (no leader election with NATS KV)
func (h *Handler) IsLeader() bool {
	return true // All nodes are equal with NATS KV
}

// GetState returns the current node state
func (h *Handler) GetState() NodeState {
	return h.state.Load().(NodeState)
}

// GetLeaderAddr returns empty (no leader with NATS KV)
func (h *Handler) GetLeaderAddr() string {
	return ""
}

// GetMetrics returns HA metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetNodeID returns the node ID
func (h *Handler) GetNodeID() string {
	return h.config.NodeID
}

// HealthCheck returns health status
func (h *Handler) HealthCheck() map[string]interface{} {
	return map[string]interface{}{
		"state":   h.GetState().String(),
		"node_id": h.config.NodeID,
		"mode":    "nats-kv",
		"leader":  true,
	}
}

// GetHealthStatus returns detailed health status (for API compatibility)
func (h *Handler) GetHealthStatus() map[string]interface{} {
	return h.HealthCheck()
}

// GetClusterState returns cluster state (simplified - no Raft cluster)
func (h *Handler) GetClusterState() map[string]interface{} {
	return map[string]interface{}{
		"mode":    "nats-kv",
		"nodes":   1,
		"message": "Using NATS JetStream KV - no Raft cluster",
	}
}

// AddVoter is a no-op (no Raft cluster)
func (h *Handler) AddVoter(nodeID string, addr string) error {
	h.logger.Warn("AddVoter called but Raft is disabled",
		zap.String("node_id", nodeID),
		zap.String("addr", addr))
	return nil
}
