// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package natsbridge

import (
	"sync"
	"sync/atomic"
	"time"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// The MAP SMS wire contract — subjects, message schema, parameter keys,
// status and failure codes — is owned by the Apache-2.0 client module so
// applications can link it without this AGPL server. The aliases below keep
// the server's names; change the contract there, not here.

// NATS subjects of the MAP interfaces (contract: ss7client).
const (
	// Inbound: application → gateway
	SubjectMTSmsRequest   = ss7client.SubjectMTSMSRequest
	SubjectSRIRequest     = ss7client.SubjectSRIRequest
	SubjectATIRequest     = ss7client.SubjectATIRequest
	SubjectPSIRequest     = ss7client.SubjectPSIRequest
	SubjectRoutingRequest = ss7client.SubjectRoutingRequest
	SubjectUSSDStart      = ss7client.SubjectUSSDStart
	SubjectUSSDCommand    = ss7client.SubjectUSSDCommand

	// Outbound: gateway → application
	SubjectMOSms           = ss7client.SubjectMOSMS
	SubjectMTSmsResponse   = ss7client.SubjectMTSMSResponse
	SubjectSRIResponse     = ss7client.SubjectSRIResponse
	SubjectATIResponse     = ss7client.SubjectATIResponse
	SubjectPSIResponse     = ss7client.SubjectPSIResponse
	SubjectRoutingResponse = ss7client.SubjectRoutingResponse
	SubjectUSSDResponse    = ss7client.SubjectUSSDResponse
	SubjectUSSDEventPrefix = ss7client.SubjectUSSDEventPrefix
)

// Server-private subjects: CAP/INAP are bound but not part of the published
// contract (CAP InitialDP needs pre-encoded BER and has no response path;
// INAP is rejected per ADR-002). The control subjects are reserved.
const (
	SubjectCAPInitialDP    = "cap.initial.dp"
	SubjectINAPInitialDP   = "inap.initial.dp"
	SubjectDeliveryReport  = "map.delivery.report"
	SubjectCAPEventReport  = "cap.event.report"
	SubjectINAPEventReport = "inap.event.report"

	SubjectHealthCheck = "ss7.health.check"
	SubjectMetrics     = "ss7.metrics"
	SubjectConfig      = "ss7.config.>"
	SubjectAlert       = "ss7.alert.>"
)

// MessageType represents the type of SS7 message
type MessageType = ss7client.MessageType

const (
	// MAP Message Types (contract: ss7client)
	MsgTypeMAPMTSms   = ss7client.MsgTypeMAPMTSMS
	MsgTypeMAPMOSms   = ss7client.MsgTypeMAPMOSMS
	MsgTypeMAPSRI     = ss7client.MsgTypeMAPSRI
	MsgTypeMAPATI     = ss7client.MsgTypeMAPATI
	MsgTypeMAPPSI     = ss7client.MsgTypeMAPPSI
	MsgTypeMAPRouting = ss7client.MsgTypeMAPRouting
	MsgTypeMAPUSSD    = ss7client.MsgTypeMAPUSSD

	// Server-private message types
	MsgTypeMAPUpdateLocation MessageType = "MAP_UL"
	MsgTypeMAPCancelLocation MessageType = "MAP_CL"

	MsgTypeCAPInitialDP   MessageType = "CAP_IDP"
	MsgTypeCAPEventReport MessageType = "CAP_ERB"
	MsgTypeCAPConnect     MessageType = "CAP_CON"
	MsgTypeCAPContinue    MessageType = "CAP_CONT"
	MsgTypeCAPRelease     MessageType = "CAP_REL"

	MsgTypeINAPInitialDP   MessageType = "INAP_IDP"
	MsgTypeINAPEventReport MessageType = "INAP_ERB"
	MsgTypeINAPConnect     MessageType = "INAP_CON"
	MsgTypeINAPContinue    MessageType = "INAP_CONT"
	MsgTypeINAPRelease     MessageType = "INAP_REL"
)

// Direction represents message direction
type Direction = ss7client.Direction

const (
	DirectionInbound  = ss7client.DirectionInbound  // To SS7 network
	DirectionOutbound = ss7client.DirectionOutbound // From SS7 network
)

// NatsMessage is the unified message structure for NATS transport
// (contract: ss7client.Message).
type NatsMessage = ss7client.Message

// Config holds NATS bridge configuration
type Config struct {
	// NATS Connection
	ServerURL         string        `yaml:"server_url" json:"server_url"`
	MaxReconnects     int           `yaml:"max_reconnects" json:"max_reconnects"`
	ReconnectWait     time.Duration `yaml:"reconnect_wait" json:"reconnect_wait"`
	ConnectionTimeout time.Duration `yaml:"connection_timeout" json:"connection_timeout"`
	PingInterval      time.Duration `yaml:"ping_interval" json:"ping_interval"`

	// Authentication (optional)
	Username        string `yaml:"username" json:"username,omitempty"`
	Password        string `yaml:"password" json:"password,omitempty"`
	Token           string `yaml:"token" json:"token,omitempty"`
	CredentialsFile string `yaml:"credentials_file" json:"credentials_file,omitempty"`

	// TLS (optional)
	TLSEnabled    bool   `yaml:"tls_enabled" json:"tls_enabled"`
	TLSCertFile   string `yaml:"tls_cert_file" json:"tls_cert_file,omitempty"`
	TLSKeyFile    string `yaml:"tls_key_file" json:"tls_key_file,omitempty"`
	TLSCACertFile string `yaml:"tls_ca_cert_file" json:"tls_ca_cert_file,omitempty"`

	// Message Processing
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	MessageTimeout    time.Duration `yaml:"message_timeout" json:"message_timeout"`

	// Gateway Identity
	NodeID      string `yaml:"node_id" json:"node_id"`
	ClusterName string `yaml:"cluster_name" json:"cluster_name"`
}

// Handler manages NATS pub/sub operations
type Handler struct {
	config *Config
	nc     *nats.Conn
	logger *zap.Logger

	// Subscriptions: the intake (request subjects, paused by PauseIntake)
	// and the session commands, which keep flowing during a drain so the
	// node's open USSD sessions can finish.
	subscriptions []*nats.Subscription
	sessionSubs   []*nats.Subscription

	// Message handlers (callbacks)
	onMTSmsRequest   func(*NatsMessage) error
	onSRIRequest     func(*NatsMessage) error
	onATIRequest     func(*NatsMessage) error
	onPSIRequest     func(*NatsMessage) error
	onRoutingRequest func(*NatsMessage) error
	onUSSDStart      func(*NatsMessage) error
	onUSSDCommand    func(*NatsMessage) error
	onCAPInitialDP   func(*NatsMessage) error
	onINAPInitialDP  func(*NatsMessage) error

	// Channels
	incomingCh chan *NatsMessage
	outgoingCh chan outgoing
	errorCh    chan error
	shutdownCh chan struct{}

	// Metrics
	metrics *Metrics
	mu      sync.RWMutex

	// workerWg tracks worker + publishWorker goroutines so Stop can
	// drain in-flight messages instead of relying on a fixed sleep.
	workerWg sync.WaitGroup
}

// outgoing is a message for the publish worker. subject, when set, is
// where it goes; otherwise its message type decides.
type outgoing struct {
	msg     *NatsMessage
	subject string
}

// Metrics tracks NATS bridge statistics
type Metrics struct {
	// Connection stats
	ConnectionStatus atomic.Int32 // 0=disconnected, 1=connected
	ReconnectCount   atomic.Uint64
	LastReconnect    time.Time

	// Message stats
	MessagesPublished atomic.Uint64
	MessagesReceived  atomic.Uint64
	BytesPublished    atomic.Uint64
	BytesReceived     atomic.Uint64

	// Per-subject counters
	MTSmsRequestCount   atomic.Uint64
	MOSmsPublishCount   atomic.Uint64
	SRIRequestCount     atomic.Uint64
	ATIRequestCount     atomic.Uint64
	PSIRequestCount     atomic.Uint64
	RoutingRequestCount atomic.Uint64
	USSDStartCount      atomic.Uint64
	USSDCommandCount    atomic.Uint64
	CAPInitialDPCount   atomic.Uint64
	INAPInitialDPCount  atomic.Uint64

	// Errors
	PublishErrors    atomic.Uint64
	SubscribeErrors  atomic.Uint64
	ProcessingErrors atomic.Uint64

	// Latency tracking (nanoseconds)
	PublishLatencySum   atomic.Uint64
	PublishLatencyCount atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents NATS bridge health
type HealthStatus struct {
	Connected         bool      `json:"connected"`
	ServerURL         string    `json:"server_url"`
	NodeID            string    `json:"node_id"`
	ReconnectCount    uint64    `json:"reconnect_count"`
	MessagesPublished uint64    `json:"messages_published"`
	MessagesReceived  uint64    `json:"messages_received"`
	PublishErrors     uint64    `json:"publish_errors"`
	AverageLatencyMs  float64   `json:"average_latency_ms"`
	LastUpdate        time.Time `json:"last_update"`
}

// PublishOption represents options for publishing messages
type PublishOption func(*publishOptions)

type publishOptions struct {
	timeout time.Duration
	headers map[string][]string
}

// WithTimeout sets publish timeout
func WithTimeout(timeout time.Duration) PublishOption {
	return func(o *publishOptions) {
		o.timeout = timeout
	}
}

// WithHeaders sets NATS headers
func WithHeaders(headers map[string][]string) PublishOption {
	return func(o *publishOptions) {
		o.headers = headers
	}
}
