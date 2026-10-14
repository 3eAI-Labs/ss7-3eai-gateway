// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"go.uber.org/zap"
)

// AddressIndicator is the first octet of an SCCP address (ITU-T Q.713
// §3.4.1): bit 1 point code present, bit 2 SSN present, bits 3-6 global
// title indicator, bit 7 routing indicator (0 = route on GT, 1 = route on
// DPC/SSN), bit 8 reserved for national use.
type AddressIndicator uint8

const (
	AIPCPresent  AddressIndicator = 0x01 // PC present
	AISSNPresent AddressIndicator = 0x02 // SSN present
	// AIGTPresent is a caller-side hint only: on the wire the GTI field
	// (bits 3-6) says whether a GT is present, and encodeAddress writes it
	// from GlobalTitle.GTI.
	AIGTPresent AddressIndicator = 0x04
	// AIRouteOnSSN is the routing indicator. Clear means route on GT.
	AIRouteOnSSN AddressIndicator = 0x40
)

// RoutesOnGT reports whether the routing indicator says route on GT.
func (ai AddressIndicator) RoutesOnGT() bool { return ai&AIRouteOnSSN == 0 }

// GlobalTitleIndicator represents GT format (ITU-T Q.713)
type GlobalTitleIndicator uint8

const (
	GTI0000 GlobalTitleIndicator = 0x00 // No GT
	GTI0001 GlobalTitleIndicator = 0x01 // GT includes NAI only
	GTI0010 GlobalTitleIndicator = 0x02 // GT includes TT only
	GTI0011 GlobalTitleIndicator = 0x03 // GT includes TT + NP + ES
	GTI0100 GlobalTitleIndicator = 0x04 // GT includes TT + NP + ES + NAI
)

// NatureOfAddressIndicator (ITU-T Q.713)
type NatureOfAddressIndicator uint8

const (
	NAIUnknown       NatureOfAddressIndicator = 0x00
	NAISubscriber    NatureOfAddressIndicator = 0x01
	NAIReserved      NatureOfAddressIndicator = 0x02
	NAINational      NatureOfAddressIndicator = 0x03
	NAIInternational NatureOfAddressIndicator = 0x04
)

// NumberingPlan (ITU-T Q.713)
type NumberingPlan uint8

const (
	NPUnknown  NumberingPlan = 0x00
	NPISDN     NumberingPlan = 0x01 // E.164
	NPData     NumberingPlan = 0x03 // X.121
	NPTelex    NumberingPlan = 0x04
	NPMaritime NumberingPlan = 0x05
	NPLand     NumberingPlan = 0x06
	NPPrivate  NumberingPlan = 0x07
)

// EncodingScheme represents digit encoding
type EncodingScheme uint8

const (
	ESUnknown EncodingScheme = 0x00
	ESBCD     EncodingScheme = 0x01 // BCD, odd number of digits
	ESBCDEven EncodingScheme = 0x02 // BCD, even number of digits
)

// SubsystemNumber (ITU-T Q.713)
type SubsystemNumber uint8

const (
	SSNUnknown        SubsystemNumber = 0x00
	SSNSCCPManagement SubsystemNumber = 0x01
	SSNReserved       SubsystemNumber = 0x02
	SSNISUPManagement SubsystemNumber = 0x03
	SSNOmap           SubsystemNumber = 0x04
	SSNMAP            SubsystemNumber = 0x06
	SSNHLR            SubsystemNumber = 0x06
	SSNVLR            SubsystemNumber = 0x07
	SSNMSC            SubsystemNumber = 0x08
	SSNEIC            SubsystemNumber = 0x09
	SSNCAP            SubsystemNumber = 0x92 // 146
	SSNGSMSCF         SubsystemNumber = 0x93 // 147
	SSNGSMSRF         SubsystemNumber = 0x94 // 148
)

// MessageType represents SCCP message types (ITU-T Q.713)
type MessageType uint8

const (
	MsgTypeCR    MessageType = 0x01 // Connection Request
	MsgTypeCC    MessageType = 0x02 // Connection Confirm
	MsgTypeCREF  MessageType = 0x03 // Connection Refused
	MsgTypeRLSD  MessageType = 0x04 // Released
	MsgTypeRLC   MessageType = 0x05 // Release Complete
	MsgTypeDT1   MessageType = 0x06 // Data Form 1
	MsgTypeDT2   MessageType = 0x07 // Data Form 2
	MsgTypeAK    MessageType = 0x08 // Data Acknowledgement
	MsgTypeUDT   MessageType = 0x09 // Unitdata
	MsgTypeUDTS  MessageType = 0x0A // Unitdata Service
	MsgTypeED    MessageType = 0x0B // Expedited Data
	MsgTypeEA    MessageType = 0x0C // Expedited Data Acknowledgement
	MsgTypeRSR   MessageType = 0x0D // Reset Request
	MsgTypeRSC   MessageType = 0x0E // Reset Confirm
	MsgTypeERR   MessageType = 0x0F // Protocol Data Unit Error
	MsgTypeIT    MessageType = 0x10 // Inactivity Test
	MsgTypeXUDT  MessageType = 0x11 // Extended Unitdata
	MsgTypeXUDTS MessageType = 0x12 // Extended Unitdata Service
	MsgTypeLUDT  MessageType = 0x13 // Long Unitdata
	MsgTypeLUDTS MessageType = 0x14 // Long Unitdata Service
)

// ReturnCause is the cause carried by a UDTS (Q.713 §3.12).
type ReturnCause uint8

const (
	CauseNoTranslationForNature  ReturnCause = 0
	CauseNoTranslationForAddress ReturnCause = 1
	CauseSubsystemCongestion     ReturnCause = 2
	CauseSubsystemFailure        ReturnCause = 3
	CauseUnequippedUser          ReturnCause = 4
	CauseMTPFailure              ReturnCause = 5
	CauseNetworkCongestion       ReturnCause = 6
	CauseUnqualified             ReturnCause = 7
	CauseErrorInMessageTransport ReturnCause = 8
	CauseErrorInLocalProcessing  ReturnCause = 9
	CauseCannotReassemble        ReturnCause = 10
	CauseSCCPFailure             ReturnCause = 11
	CauseHopCounterViolation     ReturnCause = 12
)

func (c ReturnCause) String() string {
	names := [...]string{
		"no translation for an address of such nature",
		"no translation for this specific address",
		"subsystem congestion",
		"subsystem failure",
		"unequipped user",
		"MTP failure",
		"network congestion",
		"unqualified",
		"error in message transport",
		"error in local processing",
		"destination cannot perform reassembly",
		"SCCP failure",
		"hop counter violation",
	}
	if int(c) < len(names) {
		return fmt.Sprintf("cause %d (%s)", uint8(c), names[c])
	}
	return fmt.Sprintf("cause %d", uint8(c))
}

// ProtocolClass represents SCCP protocol class
type ProtocolClass uint8

const (
	ClassBasicConnectionless           ProtocolClass = 0x00
	ClassInSequenceConnectionless      ProtocolClass = 0x01
	ClassBasicConnectionOriented       ProtocolClass = 0x02
	ClassFlowControlConnectionOriented ProtocolClass = 0x03
)

// GlobalTitle represents parsed GT (ITU-T Q.713)
type GlobalTitle struct {
	GTI             GlobalTitleIndicator
	TranslationType uint8
	NumberingPlan   NumberingPlan
	EncodingScheme  EncodingScheme
	NAI             NatureOfAddressIndicator
	Address         string // Digits in string format
}

// SccpAddress represents SCCP address (ITU-T Q.713)
type SccpAddress struct {
	AddressIndicator AddressIndicator
	PointCode        uint32
	SubsystemNumber  SubsystemNumber
	GlobalTitle      *GlobalTitle
}

// SccpMessage represents a parsed SCCP message
type SccpMessage struct {
	MessageType   MessageType
	ProtocolClass ProtocolClass
	// ReturnOnError is the "return message on error" option of the protocol
	// class octet (Q.713 §3.6, bit 8): when set, a node that cannot deliver
	// the message answers with a UDTS.
	ReturnOnError bool
	// ReturnCause is the UDTS return cause (Q.713 §3.12).
	ReturnCause      ReturnCause
	CallingAddress   *SccpAddress
	CalledAddress    *SccpAddress
	Data             []byte
	HopCounter       uint8
	Importance       uint8
	SegmentationData []byte
}

// GTTRule represents a Global Title Translation rule
type GTTRule struct {
	ID              uint64                    // Rule ID
	Priority        int                       // Higher = more important
	Pattern         string                    // GT prefix pattern (e.g., "90*", "44*")
	TranslationType *uint8                    // Match specific TT (nil = any)
	NumberingPlan   *NumberingPlan            // Match specific NP (nil = any)
	NAI             *NatureOfAddressIndicator // Match specific NAI (nil = any)

	// Translation result
	DestinationPC   uint32          // Destination Point Code
	DestinationSSN  SubsystemNumber // Destination SSN
	NewGT           *GlobalTitle    // Replace GT (nil = keep original)
	LoadBalanceMode LoadBalanceMode // Load balancing algorithm
	Destinations    []Destination   // Multiple destinations for load balancing

	// Metadata
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GTTRuleConfig is one operator-configured translation (sccp.gtt_rules).
//
// PointCode is the DPC the message is sent to. SSN selects the kind of
// translation (Q.714 §2.4):
//   - SSN 0: intermediate — the called address keeps routing on GT and
//     PointCode is the next SCCP relay (typically the STP), which
//     translates again;
//   - SSN > 0: final — the called address is rewritten to route on
//     PointCode + SSN.
type GTTRuleConfig struct {
	// Pattern is a GT digit string, a digit prefix followed by "*", or "*".
	Pattern     string `yaml:"pattern" json:"pattern" mapstructure:"pattern"`
	Priority    int    `yaml:"priority" json:"priority,omitempty" mapstructure:"priority"`
	PointCode   uint32 `yaml:"point_code" json:"point_code" mapstructure:"point_code"`
	SSN         uint8  `yaml:"ssn" json:"ssn,omitempty" mapstructure:"ssn"`
	Description string `yaml:"description" json:"description,omitempty" mapstructure:"description"`
}

// LoadBalanceMode defines load balancing algorithm
type LoadBalanceMode uint8

const (
	LBRoundRobin       LoadBalanceMode = 0x00
	LBWeightedRR       LoadBalanceMode = 0x01
	LBLeastConnections LoadBalanceMode = 0x02
	LBRandom           LoadBalanceMode = 0x03
)

// Destination represents a GTT destination with load balancing
type Destination struct {
	PointCode uint32          // Destination PC
	SSN       SubsystemNumber // Destination SSN
	Weight    int             // Weight for weighted round-robin (1-100)
	Priority  int             // Priority (higher = prefer)
	Active    bool            // Is destination available?

	// Load balancing state
	CurrentConnections atomic.Uint64 // For LeastConnections
	TotalRequests      atomic.Uint64 // Statistics
	LastUsedNanos      atomic.Int64  // Unix nanoseconds of the last selection; written concurrently by translations
}

// GTTEngine handles Global Title Translation
type GTTEngine struct {
	rules   []*GTTRule
	rulesMu sync.RWMutex

	// Load balancing state
	rrCounters map[uint64]*atomic.Uint64 // Rule ID -> Round-robin counter
	lbMu       sync.RWMutex

	// Metrics
	metrics *GTTMetrics
	logger  *zap.Logger
}

// GTTMetrics tracks GTT statistics
type GTTMetrics struct {
	TranslationsTotal   atomic.Uint64
	TranslationsSuccess atomic.Uint64
	TranslationsFailed  atomic.Uint64
	CacheHits           atomic.Uint64
	CacheMisses         atomic.Uint64
	AverageLatency      atomic.Uint64 // Nanoseconds
	mu                  sync.RWMutex
	LastUpdate          time.Time
}

// Handler manages SCCP protocol operations
type Handler struct {
	config    *Config
	gttEngine *GTTEngine

	// M3UA integration. Owner: m3ua.Handler. SCCP receives MTP3-wrapped SCCP
	// messages on m3uaIncomingCh (OPC/DPC/SI/SLS preserved for return routing
	// per Q.713 §1.5) and pushes encoded SCCP+MTP3 messages on m3uaOutgoingCh.
	// Q.713/RFC 4666 layering: SCCP rides on MTP3 SI=3.
	m3uaIncomingCh <-chan *m3ua.MTP3Message
	m3uaOutgoingCh chan<- *m3ua.MTP3Message

	// TCAP integration. Owner: sccp.Handler. tcapIncomingCh fans messages
	// out to TCAP after GTT/local-delivery routing; tcapOutgoingCh receives
	// SCCP-wrapped TCAP responses from TCAP for forwarding back to M3UA.
	tcapIncomingCh chan *SccpMessage
	tcapOutgoingCh chan *SccpMessage

	// Control channels
	errorCh    chan error
	shutdownCh chan struct{}

	// Subsystem status
	subsystems map[SubsystemNumber]*SubsystemStatus
	ssnMu      sync.RWMutex

	// deliveryFailureSink: see SetDeliveryFailureSink.
	deliveryFailureSink func(msg *SccpMessage, reason string)

	// slsCounter rotates the SLS of protocol class 0 messages.
	slsCounter atomic.Uint32

	// Metrics
	metrics *Metrics
	logger  *zap.Logger
}

// SubsystemStatus tracks SSN availability
type SubsystemStatus struct {
	SSN              SubsystemNumber
	Available        bool
	PointCode        uint32
	LastStatusChange time.Time
	Congested        bool
}

// Config holds SCCP handler configuration
// NetworkVariant selects the SS7 variant which determines on-the-wire
// SCCP point-code field width per ITU-T Q.713 §3.4.1:
//
//   - VariantITU  → 14-bit PC, encoded as 2 octets little-endian
//     (Europe, Asia, Middle East, Africa — global default)
//   - VariantANSI → 24-bit PC, encoded as 3 octets little-endian
//     (United States, Canada, parts of LATAM)
//
// Variant must match the peer's expectation; mismatch silently corrupts
// the address parameter. Operators MUST set this explicitly per network
// they connect to — there is no auto-detection in the SCCP layer.
type NetworkVariant uint8

const (
	VariantITU  NetworkVariant = 0 // 14-bit PC (default)
	VariantANSI NetworkVariant = 1 // 24-bit PC
)

func (v NetworkVariant) String() string {
	switch v {
	case VariantITU:
		return "ITU"
	case VariantANSI:
		return "ANSI"
	default:
		return "Unknown"
	}
}

// PCWidth returns the on-wire PC field length in octets for this variant.
func (v NetworkVariant) PCWidth() int {
	if v == VariantANSI {
		return 3
	}
	return 2
}

// PCMask returns the bitmask that should be applied when reading or
// validating a point code for this variant. Values larger than the mask
// would silently truncate on encode.
func (v NetworkVariant) PCMask() uint32 {
	if v == VariantANSI {
		return 0x00FFFFFF // 24-bit
	}
	return 0x00003FFF // 14-bit
}

// ValidPointCode reports whether pc fits this variant's point code width.
// An oversized value would be truncated in an SCCP address but sent whole
// as an MTP3 DPC, so it is rejected at configuration time.
func (v NetworkVariant) ValidPointCode(pc uint32) bool { return pc&^v.PCMask() == 0 }

// GTConfig describes a configured Global Title — used in two distinct
// roles by the cluster HA model (ADR-006):
//
//   - LocalGlobalTitle (per-node sticky CgPA): each cluster node has its
//     own GT digits (e.g. node1=905900000001, node2=905900000002). All
//     originated dialogues set CgPA = this. Peers pin it in their CdPA
//     on responses, so the SG routes responses back to ONLY this node —
//     no inter-node forwarding needed.
//
//   - ClusterGlobalTitle (cluster-shared inbound GT): all cluster nodes
//     share this digit string. New dialogues from foreign peers (MO-Fwd
//     from a roaming SMSC, SRI from a foreign HLR) target this GT; the
//     SG loadshares them across the cluster (M3UA traffic_mode=2). The
//     receiving node owns that dialogue from BEGIN to END.
//
// When Address is empty, the GT is disabled for that role. Single-node
// deployments may leave both empty (PC+SSN routing only).
//
// Defaults applied by Config.BuildLocalGT / BuildClusterGT when zero:
//   - GTI:            GTI0100 (TT + NP + ES + NAI — Q.713 §3.4.1)
//   - TranslationType: 0
//   - NumberingPlan:  NPISDN (1 = E.164)
//   - NAI:            NAIInternational (4)
//   - EncodingScheme: ESBCD (auto-flips to ESBCDEven if even-digit address)
type GTConfig struct {
	Address         string `yaml:"address" json:"address"`                               // E.164 digits; empty = disabled
	GTI             uint8  `yaml:"gti" json:"gti,omitempty"`                             // 0=default GTI0100; valid: 1,2,3,4
	TranslationType uint8  `yaml:"translation_type" json:"translation_type,omitempty"`   // ANSI carriers may need non-zero
	NumberingPlan   uint8  `yaml:"numbering_plan" json:"numbering_plan,omitempty"`       // 0=default NPISDN(1)
	NatureOfAddress uint8  `yaml:"nature_of_address" json:"nature_of_address,omitempty"` // 0=default NAIInternational(4)
}

type Config struct {
	// Local configuration
	LocalPointCode  uint32            `yaml:"local_point_code" json:"local_point_code"`
	LocalSubsystems []SubsystemNumber `yaml:"local_subsystems" json:"local_subsystems"`

	// LocalGlobalTitle is this node's per-node sticky GT for CgPA source
	// addressing. Empty Address disables — see GTConfig doc for HA roles.
	LocalGlobalTitle GTConfig `yaml:"local_global_title" json:"local_global_title,omitempty"`

	// ClusterGlobalTitle is the cluster-shared inbound GT for new
	// dialogues from foreign peers, loadshared by SG across the cluster.
	// Empty Address disables. MUST differ from LocalGlobalTitle.Address
	// when both are set (validateConfig enforces this) — same digits in
	// both roles defeats sticky response routing.
	ClusterGlobalTitle GTConfig `yaml:"cluster_global_title" json:"cluster_global_title,omitempty"`

	// NetworkVariant selects ITU 14-bit (default) or ANSI 24-bit PC width
	// on the wire. See NetworkVariant doc for which networks use which.
	NetworkVariant NetworkVariant `yaml:"network_variant" json:"network_variant"`

	// GTT configuration. GT-routed messages this node originates are
	// translated by GTTRules (loaded at NewHandler); with GTT disabled or no
	// rule matching, such a message cannot be sent.
	GTTEnabled      bool            `yaml:"gtt_enabled" json:"gtt_enabled"`
	GTTRules        []GTTRuleConfig `yaml:"gtt_rules" json:"gtt_rules,omitempty"`
	GTTCacheEnabled bool            `yaml:"gtt_cache_enabled" json:"gtt_cache_enabled"`
	GTTCacheTTL     time.Duration   `yaml:"gtt_cache_ttl" json:"gtt_cache_ttl"`
	GTTCacheMaxSize int             `yaml:"gtt_cache_max_size" json:"gtt_cache_max_size"`

	// Message handling
	MaxHopCounter       uint8 `yaml:"max_hop_counter" json:"max_hop_counter"`
	SegmentationEnabled bool  `yaml:"segmentation_enabled" json:"segmentation_enabled"`
	MaxSegmentSize      int   `yaml:"max_segment_size" json:"max_segment_size"`

	// Performance
	ChannelBufferSize int `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int `yaml:"worker_pool_size" json:"worker_pool_size"`
}

// Metrics tracks SCCP statistics
type Metrics struct {
	MessagesReceived atomic.Uint64
	MessagesSent     atomic.Uint64
	BytesReceived    atomic.Uint64
	BytesSent        atomic.Uint64
	Errors           atomic.Uint64

	// Message type counters
	UDTReceived  atomic.Uint64
	UDTSent      atomic.Uint64
	XUDTReceived atomic.Uint64
	XUDTSent     atomic.Uint64

	// GTT metrics
	GTTTranslations atomic.Uint64
	GTTFailures     atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents handler health
type HealthStatus struct {
	ActiveSubsystems []SubsystemNumber `json:"active_subsystems"`
	GTTRulesCount    int               `json:"gtt_rules_count"`
	MessagesReceived uint64            `json:"messages_received"`
	MessagesSent     uint64            `json:"messages_sent"`
	Errors           uint64            `json:"errors"`
	GTTTranslations  uint64            `json:"gtt_translations"`
	LastUpdate       time.Time         `json:"last_update"`
}
