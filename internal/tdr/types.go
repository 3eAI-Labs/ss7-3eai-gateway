// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tdr

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// TDR (Transaction Data Record) represents a complete SS7 transaction
type TDR struct {
	// Record Metadata
	RecordID   string     `json:"record_id"`
	SessionID  string     `json:"session_id"`
	Timestamp  time.Time  `json:"timestamp"`
	RecordType RecordType `json:"record_type"`
	Protocol   string     `json:"protocol"`  // MAP, CAP, INAP
	Direction  string     `json:"direction"` // INBOUND, OUTBOUND

	// Network Elements
	OriginatingNE string   `json:"originating_ne"` // Network Element
	DestinationNE string   `json:"destination_ne"`
	TransitNEs    []string `json:"transit_nes,omitempty"`

	// SS7 Routing
	OPC           uint32 `json:"opc"`
	DPC           uint32 `json:"dpc"`
	SSN           uint8  `json:"ssn"`
	SourceGT      string `json:"source_gt"`
	DestinationGT string `json:"destination_gt"`

	// Subscriber Information (PII - must be encrypted)
	CallingParty string `json:"calling_party,omitempty"`
	CalledParty  string `json:"called_party,omitempty"`
	IMSI         string `json:"imsi,omitempty"`
	MSISDN       string `json:"msisdn,omitempty"`
	IMEI         string `json:"imei,omitempty"`

	// Location Information (PII - must be encrypted)
	LocationInfo *LocationInfo `json:"location_info,omitempty"`

	// Transaction Details
	OperationCode int32  `json:"operation_code,omitempty"`
	OperationName string `json:"operation_name"`
	ServiceKey    uint32 `json:"service_key,omitempty"`

	// Charging Information
	ChargingInfo *ChargingInfo `json:"charging_info,omitempty"`

	// Result
	ResultCode    int32  `json:"result_code"`
	ResultMessage string `json:"result_message"`
	ErrorCode     *int32 `json:"error_code,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`

	// Quality Metrics
	ProcessingTime int64 `json:"processing_time_ms"` // Milliseconds
	HopCount       int   `json:"hop_count"`
	Retries        int   `json:"retries"`

	// Firewall Information
	FirewallAction string `json:"firewall_action,omitempty"`
	ThreatCategory string `json:"threat_category,omitempty"`
	ThreatScore    int    `json:"threat_score,omitempty"`

	// Searchable Fields (HMAC for GDPR DSAR compliance)
	CallingPartyHMAC string `json:"calling_party_hmac,omitempty"`
	CalledPartyHMAC  string `json:"called_party_hmac,omitempty"`
	IMSHMAC          string `json:"imsi_hmac,omitempty"`
	MSISDNHMAC       string `json:"msisdn_hmac,omitempty"`

	// Metadata
	NodeID       string                 `json:"node_id"`
	ClusterName  string                 `json:"cluster_name"`
	Tags         map[string]string      `json:"tags,omitempty"`
	CustomFields map[string]interface{} `json:"custom_fields,omitempty"`
}

// RecordType represents type of TDR
type RecordType string

const (
	RecordTypeSMS           RecordType = "SMS"
	RecordTypeVoiceCall     RecordType = "VOICE_CALL"
	RecordTypeDataSession   RecordType = "DATA_SESSION"
	RecordTypeSupplementary RecordType = "SUPPLEMENTARY_SERVICE"
	RecordTypeLocation      RecordType = "LOCATION_UPDATE"
	RecordTypeFraud         RecordType = "FRAUD_ATTEMPT"
)

// LocationInfo represents subscriber location (PII)
type LocationInfo struct {
	CellID    string    `json:"cell_id,omitempty"`
	LAC       uint16    `json:"lac,omitempty"` // Location Area Code
	MCC       string    `json:"mcc,omitempty"` // Mobile Country Code
	MNC       string    `json:"mnc,omitempty"` // Mobile Network Code
	Latitude  float64   `json:"latitude,omitempty"`
	Longitude float64   `json:"longitude,omitempty"`
	Accuracy  int       `json:"accuracy,omitempty"` // Meters
	Timestamp time.Time `json:"timestamp"`
}

// ChargingInfo represents charging information
type ChargingInfo struct {
	ChargeAmount float64 `json:"charge_amount"`
	Currency     string  `json:"currency"`
	ChargeType   string  `json:"charge_type"` // PREPAID, POSTPAID
	Balance      float64 `json:"balance,omitempty"`
	Units        int     `json:"units,omitempty"`
}

// Handler manages TDR generation and storage
type Handler struct {
	config *Config

	// AWS S3 Client
	s3Client *s3.Client

	// Encryption
	encryptor *Encryptor

	// Buffering
	buffer   []*TDR
	bufferMu sync.Mutex

	// Channels
	tdrCh      chan *TDR
	shutdownCh chan struct{}

	// Metrics
	metrics *Metrics
	logger  *zap.Logger
}

// Config holds TDR generator configuration
type Config struct {
	// General
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	NodeID      string `yaml:"node_id" json:"node_id"`
	ClusterName string `yaml:"cluster_name" json:"cluster_name"`

	// AWS S3 Configuration
	S3Bucket       string `yaml:"s3_bucket" json:"s3_bucket"`
	S3Region       string `yaml:"s3_region" json:"s3_region"`
	S3Prefix       string `yaml:"s3_prefix" json:"s3_prefix"`
	S3StorageClass string `yaml:"s3_storage_class" json:"s3_storage_class"` // STANDARD, INTELLIGENT_TIERING, GLACIER

	// AWS KMS Configuration (for encryption)
	KMSKeyID  string `yaml:"kms_key_id" json:"kms_key_id"`
	KMSRegion string `yaml:"kms_region" json:"kms_region"`

	// Encryption
	EncryptionEnabled bool   `yaml:"encryption_enabled" json:"encryption_enabled"`
	HMACSecret        string `yaml:"hmac_secret" json:"hmac_secret"` // For searchable encryption

	// Buffering & Performance
	BufferSize        int           `yaml:"buffer_size" json:"buffer_size"`
	FlushInterval     time.Duration `yaml:"flush_interval" json:"flush_interval"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`

	// Compression
	CompressionEnabled bool `yaml:"compression_enabled" json:"compression_enabled"`
	CompressionLevel   int  `yaml:"compression_level" json:"compression_level"` // 1-9 (gzip)

	// Retention & Lifecycle
	RetentionDays    int `yaml:"retention_days" json:"retention_days"`
	ArchiveAfterDays int `yaml:"archive_after_days" json:"archive_after_days"`

	// PII Protection (GDPR)
	PIIEncryption        bool `yaml:"pii_encryption" json:"pii_encryption"`
	SearchableEncryption bool `yaml:"searchable_encryption" json:"searchable_encryption"`

	// S3 Object Lock (WORM - Write Once Read Many)
	ObjectLockEnabled bool   `yaml:"object_lock_enabled" json:"object_lock_enabled"`
	ObjectLockMode    string `yaml:"object_lock_mode" json:"object_lock_mode"` // GOVERNANCE, COMPLIANCE
	ObjectLockDays    int    `yaml:"object_lock_days" json:"object_lock_days"`
}

// Encryptor handles encryption operations
type Encryptor struct {
	// AES-256-GCM for field-level encryption
	aesKey []byte

	// HMAC-SHA256 for searchable encryption
	hmacSecret []byte

	// KMS integration
	kmsClient interface{} // AWS KMS client
	kmsKeyID  string

	mu sync.RWMutex
}

// EncryptedField represents an encrypted field
type EncryptedField struct {
	Ciphertext     []byte `json:"ciphertext"`
	Nonce          []byte `json:"nonce"`
	Tag            []byte `json:"tag,omitempty"`
	EncryptionAlgo string `json:"encryption_algo"` // AES-256-GCM
	KMSKeyID       string `json:"kms_key_id,omitempty"`
}

// S3Object represents metadata for S3 upload
type S3Object struct {
	Key          string
	Bucket       string
	ContentType  string
	StorageClass string
	Metadata     map[string]string
	Tags         map[string]string
	Data         []byte
	ObjectLock   *ObjectLockConfig
}

// ObjectLockConfig represents S3 Object Lock configuration
type ObjectLockConfig struct {
	Mode            string // GOVERNANCE, COMPLIANCE
	RetainUntilDate time.Time
}

// Metrics tracks TDR generator statistics
type Metrics struct {
	// TDR Generation
	TDRsGenerated atomic.Uint64
	TDRsUploaded  atomic.Uint64
	TDRsFailed    atomic.Uint64

	// By Type
	SMSTDRs         atomic.Uint64
	VoiceCallTDRs   atomic.Uint64
	DataSessionTDRs atomic.Uint64

	// Storage
	BytesUploaded   atomic.Uint64
	BytesCompressed atomic.Uint64
	ObjectsUploaded atomic.Uint64

	// Encryption
	FieldsEncrypted atomic.Uint64
	HMACsGenerated  atomic.Uint64

	// Performance
	UploadLatencySum   atomic.Uint64 // Nanoseconds
	UploadLatencyCount atomic.Uint64
	BufferFlushes      atomic.Uint64

	// Errors
	EncryptionErrors atomic.Uint64
	S3Errors         atomic.Uint64
	KMSErrors        atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
	LastFlush  time.Time
}

// HealthStatus represents TDR generator health
type HealthStatus struct {
	Enabled          bool      `json:"enabled"`
	NodeID           string    `json:"node_id"`
	S3Bucket         string    `json:"s3_bucket"`
	TDRsGenerated    uint64    `json:"tdrs_generated"`
	TDRsUploaded     uint64    `json:"tdrs_uploaded"`
	TDRsFailed       uint64    `json:"tdrs_failed"`
	BytesUploaded    uint64    `json:"bytes_uploaded"`
	ObjectsUploaded  uint64    `json:"objects_uploaded"`
	AverageLatencyMs float64   `json:"average_latency_ms"`
	BufferSize       int       `json:"buffer_size"`
	LastFlush        time.Time `json:"last_flush"`
	LastUpdate       time.Time `json:"last_update"`
}

// SearchQuery represents a GDPR DSAR search query
type SearchQuery struct {
	// Search by HMAC (privacy-preserving)
	CallingPartyHMAC string
	CalledPartyHMAC  string
	IMSHMAC          string
	MSISDNHMAC       string

	// Time range
	StartTime time.Time
	EndTime   time.Time

	// Filters
	RecordType RecordType
	NodeID     string
	Protocol   string

	// Pagination
	Limit  int
	Offset int
}

// SearchResult represents search results for GDPR DSAR
type SearchResult struct {
	TDRs       []*TDR
	TotalCount int
	HasMore    bool
	NextOffset int
	QueryTime  time.Duration
}
