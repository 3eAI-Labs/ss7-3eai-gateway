// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// GSMA FS.11 Categories (Permanent Reference Document FS.11)
// https://www.gsma.com/security/resources/fs-11-ss7-interconnect-security-monitoring/

// ThreatCategory represents GSMA FS.11 threat categories
type ThreatCategory uint8

const (
	// Category 1: Location Tracking & Interception (High Severity)
	Cat1LocationTracking ThreatCategory = 0x01
	Cat1CallInterception ThreatCategory = 0x02
	Cat1SMSInterception  ThreatCategory = 0x03
	Cat1IMSIDisclosure   ThreatCategory = 0x04

	// Category 2: Fraud & Revenue Loss (Medium-High Severity)
	Cat2SMSFraud                ThreatCategory = 0x10
	Cat2CallFraud               ThreatCategory = 0x11
	Cat2SubscriberImpersonation ThreatCategory = 0x12
	Cat2USSDFraud               ThreatCategory = 0x13

	// Category 3: Denial of Service (Medium Severity)
	Cat3MessageFlooding     ThreatCategory = 0x20
	Cat3ResourceExhaustion  ThreatCategory = 0x21
	Cat3RoutingManipulation ThreatCategory = 0x22
	Cat3SubscriberDOS       ThreatCategory = 0x23
)

// Action represents firewall action
type Action uint8

const (
	ActionAllow      Action = 0x00 // Allow message through
	ActionBlock      Action = 0x01 // Block message
	ActionQuarantine Action = 0x02 // Quarantine for analysis
	ActionAlert      Action = 0x03 // Allow but generate alert
	ActionRateLimit  Action = 0x04 // Apply rate limiting
)

// RuleType represents type of firewall rule
type RuleType uint8

const (
	RuleTypeWhitelist  RuleType = 0x00 // Explicit allow
	RuleTypeBlacklist  RuleType = 0x01 // Explicit deny
	RuleTypeGeoFencing RuleType = 0x02 // Geographic restrictions
	RuleTypeRateLimit  RuleType = 0x03 // Rate limiting
	RuleTypeAnomaly    RuleType = 0x04 // Anomaly detection
	RuleTypeSignature  RuleType = 0x05 // Signature-based detection
)

// Protocol represents SS7 protocol type
type Protocol uint8

const (
	ProtocolMAP  Protocol = 0x00
	ProtocolCAP  Protocol = 0x01
	ProtocolINAP Protocol = 0x02
	ProtocolSCCP Protocol = 0x03
	ProtocolM3UA Protocol = 0x04
)

// FirewallRule represents a firewall rule (GSMA FS.11 compliant)
type FirewallRule struct {
	ID             uint64         // Unique rule ID
	Name           string         // Rule name
	Description    string         // Human-readable description
	Enabled        bool           // Is rule active
	Priority       int            // Higher = more important (0-1000)
	RuleType       RuleType       // Type of rule
	ThreatCategory ThreatCategory // GSMA FS.11 category
	Action         Action         // Action to take

	// Protocol Matching
	Protocol       *Protocol // Specific protocol (nil = all)
	OperationCodes []int32   // MAP/CAP/INAP operation codes
	MessageTypes   []uint8   // SCCP/M3UA message types

	// Address Matching (SCCP layer)
	SourceGT      []string // Source Global Title patterns
	DestinationGT []string // Destination Global Title patterns
	SourcePC      []uint32 // Source Point Codes
	DestinationPC []uint32 // Destination Point Codes
	SSN           []uint8  // Subsystem Numbers

	// MSISDN/IMSI Matching (MAP layer)
	CallingParty []string // Calling party patterns (E.164)
	CalledParty  []string // Called party patterns (E.164)
	IMSI         []string // IMSI patterns
	CountryCode  []string // Country codes (MCC)
	NetworkCode  []string // Network codes (MNC)

	// Geo-Fencing
	AllowedCountries []string // ISO 3166-1 alpha-2 codes
	BlockedCountries []string // ISO 3166-1 alpha-2 codes

	// Rate Limiting
	RateLimit *RateLimit // Rate limit configuration

	// Time-based
	Schedule *Schedule // Time-based activation

	// Metadata
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CreatedBy     string
	LastMatchedAt time.Time
	MatchCount    atomic.Uint64
}

// RateLimit defines rate limiting parameters
type RateLimit struct {
	MaxRequests    int            // Max requests per window
	WindowDuration time.Duration  // Time window
	BurstSize      int            // Burst allowance
	Scope          RateLimitScope // Per what?
}

// RateLimitScope defines rate limit scope
type RateLimitScope uint8

const (
	ScopeGlobal      RateLimitScope = 0x00 // Global limit
	ScopePerSourceGT RateLimitScope = 0x01 // Per source GT
	ScopePerSourcePC RateLimitScope = 0x02 // Per source PC
	ScopePerMSISDN   RateLimitScope = 0x03 // Per MSISDN
	ScopePerIMSI     RateLimitScope = 0x04 // Per IMSI
)

// Schedule defines time-based rule activation
type Schedule struct {
	StartTime  time.Time
	EndTime    time.Time
	DaysOfWeek []time.Weekday
	TimeZone   *time.Location
}

// Message represents an SS7 message for inspection
type Message struct {
	// Metadata
	MessageID string
	Timestamp time.Time
	Protocol  Protocol
	Direction Direction // Inbound/Outbound

	// Layer 3 (SCCP)
	OPC             uint32
	DPC             uint32
	SSN             uint8
	SourceGT        string
	DestinationGT   string
	SCCPMessageType uint8

	// Layer 4 (TCAP)
	OriginatingTID []byte
	DestinationTID []byte

	// Layer 5 (MAP/CAP/INAP)
	OperationCode *int32
	CallingParty  string
	CalledParty   string
	IMSI          string
	MSISDN        string

	// Payload
	PayloadData []byte
	PayloadSize int

	// Context
	SessionID string
	TraceID   string
}

// Direction represents message direction
type Direction uint8

const (
	DirectionInbound  Direction = 0x00
	DirectionOutbound Direction = 0x01
)

// InspectionResult represents firewall inspection result
type InspectionResult struct {
	MessageID      string
	Timestamp      time.Time
	Action         Action
	MatchedRules   []*MatchedRule
	ThreatCategory ThreatCategory
	ThreatScore    int // 0-100
	Reason         string
	Details        map[string]interface{}
	ProcessingTime time.Duration
}

// MatchedRule represents a rule that matched
type MatchedRule struct {
	RuleID         uint64
	RuleName       string
	Priority       int
	ThreatCategory ThreatCategory
	Action         Action
	MatchedField   string
	MatchedValue   string
}

// Handler manages firewall operations
type Handler struct {
	config  *Config
	rules   []*FirewallRule
	rulesMu sync.RWMutex

	// Rate limiting state
	rateLimiters   map[string]*RateLimiter
	rateLimitersMu sync.RWMutex

	// Anomaly detection
	anomalyDetector *AnomalyDetector

	// Threat intelligence
	threatDB *ThreatDatabase

	// Control channels
	inspectCh  chan *Message
	resultCh   chan *InspectionResult
	shutdownCh chan struct{}

	// Metrics
	metrics *Metrics
	logger  *zap.Logger
}

// RateLimiter implements token bucket rate limiting
type RateLimiter struct {
	maxRequests    int
	windowDuration time.Duration
	burstSize      int
	tokens         int
	lastRefill     time.Time
	mu             sync.Mutex
}

// AnomalyDetector detects anomalous behavior
type AnomalyDetector struct {
	// Baseline statistics
	baselineOPCActivity map[uint32]*ActivityStats
	baselineGTActivity  map[string]*ActivityStats
	baselineMu          sync.RWMutex

	// Detection parameters
	threshold          float64 // Deviation threshold (sigma)
	learningWindowSize int
	detectionEnabled   bool
}

// ActivityStats tracks activity statistics
type ActivityStats struct {
	Count       atomic.Uint64
	LastSeen    time.Time
	WindowStart time.Time
	MeanRate    float64
	StdDev      float64
	Values      []float64 // Sliding window
	mu          sync.RWMutex
}

// ThreatDatabase stores threat intelligence
type ThreatDatabase struct {
	// Blacklists
	blacklistedGTs     map[string]ThreatInfo
	blacklistedPCs     map[uint32]ThreatInfo
	blacklistedIMSIs   map[string]ThreatInfo
	blacklistedMSISDNs map[string]ThreatInfo
	mu                 sync.RWMutex

	// IOC (Indicators of Compromise)
	iocs   []*IOC
	iocsMu sync.RWMutex
}

// ThreatInfo represents threat intelligence information
type ThreatInfo struct {
	Identifier     string
	ThreatCategory ThreatCategory
	Severity       int // 1-10
	Description    string
	Source         string // e.g., "GSMA", "Internal", "External Feed"
	FirstSeen      time.Time
	LastSeen       time.Time
	Confidence     float64 // 0.0-1.0
}

// IOC represents an Indicator of Compromise
type IOC struct {
	ID             uint64
	Type           string // "GT", "PC", "IMSI", "MSISDN", "Operation"
	Value          string
	ThreatCategory ThreatCategory
	Severity       int
	Description    string
	ExpiresAt      time.Time
}

// Config holds firewall configuration
type Config struct {
	// General
	Enabled       bool         `yaml:"enabled" json:"enabled"`
	Mode          FirewallMode `yaml:"mode" json:"mode"`
	DefaultAction Action       `yaml:"default_action" json:"default_action"`

	// Performance
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	InspectionTimeout time.Duration `yaml:"inspection_timeout" json:"inspection_timeout"`
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`

	// Anomaly Detection
	AnomalyDetectionEnabled bool    `yaml:"anomaly_detection_enabled" json:"anomaly_detection_enabled"`
	AnomalyThreshold        float64 `yaml:"anomaly_threshold" json:"anomaly_threshold"` // Sigma

	// Threat Intelligence
	ThreatDBEnabled        bool          `yaml:"threat_db_enabled" json:"threat_db_enabled"`
	ThreatDBUpdateInterval time.Duration `yaml:"threat_db_update_interval" json:"threat_db_update_interval"`

	// Logging & Alerting
	LogBlocked      bool   `yaml:"log_blocked" json:"log_blocked"`
	LogAllowed      bool   `yaml:"log_allowed" json:"log_allowed"`
	AlertOnThreat   bool   `yaml:"alert_on_threat" json:"alert_on_threat"`
	AlertWebhookURL string `yaml:"alert_webhook_url" json:"alert_webhook_url"`
}

// FirewallMode represents firewall operating mode
type FirewallMode uint8

const (
	ModeMonitoring FirewallMode = 0x00 // Monitor only, don't block
	ModeEnforcing  FirewallMode = 0x01 // Actively block threats
	ModeLearning   FirewallMode = 0x02 // Learning mode for baseline
)

// Metrics tracks firewall statistics
type Metrics struct {
	// Inspection stats
	MessagesInspected   atomic.Uint64
	MessagesAllowed     atomic.Uint64
	MessagesBlocked     atomic.Uint64
	MessagesQuarantined atomic.Uint64
	AlertsGenerated     atomic.Uint64

	// Category-specific counters
	Cat1Detections atomic.Uint64 // Location tracking/interception
	Cat2Detections atomic.Uint64 // Fraud
	Cat3Detections atomic.Uint64 // DoS

	// Performance
	InspectionLatencySum   atomic.Uint64 // Nanoseconds
	InspectionLatencyCount atomic.Uint64

	// Rate limiting
	RateLimitHits atomic.Uint64

	// Anomaly detection
	AnomaliesDetected atomic.Uint64

	// Threat intelligence
	ThreatDBHits atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents firewall health
type HealthStatus struct {
	Enabled           bool      `json:"enabled"`
	Mode              string    `json:"mode"`
	RulesCount        int       `json:"rules_count"`
	MessagesInspected uint64    `json:"messages_inspected"`
	MessagesBlocked   uint64    `json:"messages_blocked"`
	AlertsGenerated   uint64    `json:"alerts_generated"`
	Cat1Detections    uint64    `json:"cat1_detections"`
	Cat2Detections    uint64    `json:"cat2_detections"`
	Cat3Detections    uint64    `json:"cat3_detections"`
	AverageLatencyMs  float64   `json:"average_latency_ms"`
	LastUpdate        time.Time `json:"last_update"`
}
