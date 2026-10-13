// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ha

import (
	"time"
)

// NodeState represents the HA state of this node
type NodeState uint8

const (
	NodeStateUnknown   NodeState = 0x00
	NodeStateFollower  NodeState = 0x01
	NodeStateCandidate NodeState = 0x02
	NodeStateLeader    NodeState = 0x03
	NodeStateShutdown  NodeState = 0x04
)

func (s NodeState) String() string {
	switch s {
	case NodeStateUnknown:
		return "UNKNOWN"
	case NodeStateFollower:
		return "FOLLOWER"
	case NodeStateCandidate:
		return "CANDIDATE"
	case NodeStateLeader:
		return "LEADER"
	case NodeStateShutdown:
		return "SHUTDOWN"
	default:
		return "UNKNOWN"
	}
}

// Config holds HA configuration. Cluster state is replicated via NATS
// JetStream KV (see ADR-005); this struct only carries identity fields.
type Config struct {
	NodeID      string `yaml:"node_id" json:"node_id"`
	ClusterName string `yaml:"cluster_name" json:"cluster_name"`
}

// Metrics tracks HA module metrics (simplified)
type Metrics struct {
	LastUpdate time.Time
}
