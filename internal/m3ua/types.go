// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package m3ua

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"go.uber.org/zap"
)

// ASPState represents M3UA Application Server Process state
type ASPState uint8

const (
	ASPStateDown ASPState = iota
	ASPStateInactive
	ASPStateActive
)

func (s ASPState) String() string {
	switch s {
	case ASPStateDown:
		return "DOWN"
	case ASPStateInactive:
		return "INACTIVE"
	case ASPStateActive:
		return "ACTIVE"
	default:
		return "UNKNOWN"
	}
}

// Config holds M3UA handler configuration
type Config struct {
	// SGW configuration
	SGWAddress        string `yaml:"sgw_address" json:"sgw_address"`
	SGWPort           int    `yaml:"sgw_port" json:"sgw_port"`
	ASPIdentifier     uint32 `yaml:"asp_id" json:"asp_id"`
	RoutingContext    uint32 `yaml:"routing_context" json:"routing_context"`
	NetworkAppearance uint32 `yaml:"network_appearance" json:"network_appearance"`
	// NetworkIndicator is the MTP3 network indicator stamped on every DATA
	// routing label (Q.704 §14.2.2): 0 = international, 2 = national,
	// 1 and 3 = spare / reserved for national use. It must match what the
	// SG/STP expects for this network, which discards a mismatch, so it has
	// no default: config loading rejects an unset value.
	NetworkIndicator uint8 `yaml:"network_indicator" json:"network_indicator"`
	// TrafficMode (RFC 4666 §1.4.4):
	//   1 = Override   — SG sends ALL traffic to one ASP at a time;
	//                    failover requires another ASP becoming primary.
	//                    Active-Passive semantic. Use for single-node
	//                    deployments or AP clusters.
	//   2 = Loadshare  — SG distributes traffic across all ASPs sharing
	//                    the routing context. Active-Active semantic.
	//                    REQUIRED for ADR-006 cluster mode where
	//                    sccp.cluster_global_title is set; otherwise
	//                    only one cluster node receives inbound traffic
	//                    and the cluster GT is wasted.
	// Mode 3 (Broadcast) is not supported. validateConfig rejects 0 so
	// operators must choose explicitly.
	TrafficMode uint32 `yaml:"traffic_mode" json:"traffic_mode"`

	// Multi-homing configuration
	MultiHomingEnabled bool     `yaml:"multi_homing_enabled" json:"multi_homing_enabled"`
	LocalIPs           []string `yaml:"local_ips" json:"local_ips"`

	// SCTP parameters (BR-M3UA-003: 2-second failover)
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval" json:"heartbeat_interval"`
	PathMaxRetrans    uint16        `yaml:"path_max_retrans" json:"path_max_retrans"`
	RTOMax            time.Duration `yaml:"rto_max" json:"rto_max"`
	RTOInitial        time.Duration `yaml:"rto_initial" json:"rto_initial"`
	RTOMin            time.Duration `yaml:"rto_min" json:"rto_min"`
	AssocMaxRetrans   uint16        `yaml:"assoc_max_retrans" json:"assoc_max_retrans"`

	// Timeouts (BR-M3UA-002)
	ASPUPACKTimeout      time.Duration `yaml:"aspup_ack_timeout" json:"aspup_ack_timeout"`
	ASPACACKTimeout      time.Duration `yaml:"aspac_ack_timeout" json:"aspac_ack_timeout"`
	ReconnectInterval    time.Duration `yaml:"reconnect_interval" json:"reconnect_interval"`
	ReconnectMaxAttempts int           `yaml:"reconnect_max_attempts" json:"reconnect_max_attempts"`
}

// Handler manages M3UA protocol operations
type Handler struct {
	config *Config

	// conn is the live association, nil between associations; the
	// supervisor replaces it.
	conn   *wire.Conn
	connMu sync.RWMutex

	state   ASPState
	stateMu sync.RWMutex

	started        atomic.Bool
	stopOnce       sync.Once
	supervisorDone chan struct{}

	// Channels for message flow. Both directions carry *MTP3Message so OPC/DPC/
	// SI/SLS survive the M3UA boundary — Q.713 §1.5 requires them for SCCP
	// return routing and SLS-based SCTP stream selection on responses.
	incomingCh chan *MTP3Message // MTP3 messages from SGW to SCCP layer
	outgoingCh chan *MTP3Message // Outgoing MTP3 messages from SCCP layer
	errorCh    chan error
	shutdownCh chan struct{}

	// Callbacks
	onStateChange func(oldState, newState ASPState)
	onLinkUp      func()
	onLinkDown    func()
	onSendFailure func(msg *MTP3Message, err error)

	// Metrics
	metrics *Metrics

	// Logger
	logger *zap.Logger
}

// Metrics tracks M3UA statistics
type Metrics struct {
	LinkStatus         atomic.Int64 // 1=UP, 0=DOWN
	MessagesSent       atomic.Uint64
	MessagesReceived   atomic.Uint64
	BytesSent          atomic.Uint64
	BytesReceived      atomic.Uint64
	Errors             atomic.Uint64
	StateTransitions   atomic.Uint64
	LastStateChange    time.Time
	ConnectionAttempts atomic.Uint64
	ActivePath         string // Current active SCTP path IP
	mu                 sync.RWMutex
}

// MTP3Message is one MTP3 user message with its routing label (RFC 4666
// §3.3.1 Protocol Data). On the way out, SCCP sets OPC/DPC/SI/MP/SLS and
// the handler stamps NI from Config.NetworkIndicator.
type MTP3Message struct {
	OPC         uint32 // Originating Point Code
	DPC         uint32 // Destination Point Code
	SI          uint8  // Service Indicator (SCCP = 3)
	NI          uint8  // Network Indicator (inbound only; see above)
	MP          uint8  // Message Priority
	SLS         uint8  // Signaling Link Selection
	PayloadData []byte // SCCP message
}

// HealthStatus represents handler health
type HealthStatus struct {
	State        string    `json:"state"`
	LinkStatus   string    `json:"link_status"`
	ActivePath   string    `json:"active_path"`
	MessagesSent uint64    `json:"messages_sent"`
	MessagesRecv uint64    `json:"messages_received"`
	Errors       uint64    `json:"errors"`
	LastStateChg time.Time `json:"last_state_change"`
}
