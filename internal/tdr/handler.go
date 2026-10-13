// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tdr

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var (
	ErrTDRDisabled      = errors.New("TDR: generator is disabled")
	ErrInvalidTDR       = errors.New("TDR: invalid TDR record")
	ErrEncryptionFailed = errors.New("TDR: encryption failed")
	ErrS3UploadFailed   = errors.New("TDR: S3 upload failed")
	ErrBufferFull       = errors.New("TDR: buffer is full")
)

const (
	DefaultBufferSize        = 1000
	DefaultFlushInterval     = 60 * time.Second
	DefaultWorkerPoolSize    = 4
	DefaultChannelBufferSize = 10000
	DefaultCompressionLevel  = 6
)

// NewHandler creates a new TDR handler
func NewHandler(cfg *Config, logger *zap.Logger) (*Handler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:     cfg,
		buffer:     make([]*TDR, 0, cfg.BufferSize),
		tdrCh:      make(chan *TDR, cfg.ChannelBufferSize),
		shutdownCh: make(chan struct{}),
		metrics:    &Metrics{},
		logger:     logger,
	}

	// Initialize AWS S3 client
	if cfg.Enabled {
		if err := handler.initS3Client(context.Background()); err != nil {
			return nil, fmt.Errorf("failed to initialize S3 client: %w", err)
		}

		// Initialize encryptor
		if cfg.EncryptionEnabled {
			encryptor, err := newEncryptor(cfg)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize encryptor: %w", err)
			}
			handler.encryptor = encryptor
		}
	}

	return handler, nil
}

// Start initializes TDR generator and worker pool
func (h *Handler) Start(ctx context.Context) error {
	if !h.config.Enabled {
		h.logger.Info("TDR generator is disabled")
		return nil
	}

	h.logger.Info("Starting TDR generator",
		zap.String("s3_bucket", h.config.S3Bucket),
		zap.String("node_id", h.config.NodeID),
		zap.Int("buffer_size", h.config.BufferSize))

	// Start worker pool
	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}

	// Start flush timer
	go h.flushTimer(ctx)

	h.logger.Info("TDR generator started successfully")
	return nil
}

// Stop gracefully stops the TDR generator
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping TDR generator")

	// Signal shutdown
	close(h.shutdownCh)

	// Flush remaining buffer
	if err := h.flushBuffer(); err != nil {
		h.logger.Error("Failed to flush buffer during shutdown", zap.Error(err))
	}

	h.logger.Info("TDR generator stopped gracefully")
	return nil
}

// worker processes TDRs from channel
func (h *Handler) worker(ctx context.Context, id int) {
	h.logger.Debug("TDR worker started", zap.Int("worker_id", id))
	defer h.logger.Debug("TDR worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case tdr := <-h.tdrCh:
			h.processTDR(tdr)
		}
	}
}

// Flush forces an immediate flush of the buffered TDRs to S3. It is
// mutex-guarded (shares flushBuffer's bufferMu with the periodic flush) and
// idempotent when the buffer is empty. Backs the opsTrigger `flushTDR` action.
func (h *Handler) Flush() error {
	return h.flushBuffer()
}

// flushTimer periodically flushes buffer to S3
func (h *Handler) flushTimer(ctx context.Context) {
	ticker := time.NewTicker(h.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.flushBuffer(); err != nil {
				h.logger.Error("Failed to flush buffer", zap.Error(err))
			}
		}
	}
}

// GenerateTDR generates a TDR record
func (h *Handler) GenerateTDR(tdr *TDR) error {
	if !h.config.Enabled {
		return ErrTDRDisabled
	}

	// Set metadata
	if tdr.RecordID == "" {
		tdr.RecordID = uuid.New().String()
	}
	if tdr.Timestamp.IsZero() {
		tdr.Timestamp = time.Now()
	}
	tdr.NodeID = h.config.NodeID
	tdr.ClusterName = h.config.ClusterName

	// Generate searchable HMACs (GDPR DSAR compliance)
	if h.config.SearchableEncryption {
		if tdr.CallingParty != "" {
			tdr.CallingPartyHMAC = h.generateHMAC(tdr.CallingParty)
		}
		if tdr.CalledParty != "" {
			tdr.CalledPartyHMAC = h.generateHMAC(tdr.CalledParty)
		}
		if tdr.IMSI != "" {
			tdr.IMSHMAC = h.generateHMAC(tdr.IMSI)
		}
		if tdr.MSISDN != "" {
			tdr.MSISDNHMAC = h.generateHMAC(tdr.MSISDN)
		}
		h.metrics.HMACsGenerated.Add(4)
	}

	// Encrypt PII fields if enabled
	if h.config.PIIEncryption && h.encryptor != nil {
		if err := h.encryptPII(tdr); err != nil {
			h.logger.Error("Failed to encrypt PII", zap.Error(err))
			h.metrics.EncryptionErrors.Add(1)
			return err
		}
	}

	// Update metrics
	h.metrics.TDRsGenerated.Add(1)
	h.updateTypeMetrics(tdr.RecordType)

	// Send to processing channel
	select {
	case h.tdrCh <- tdr:
		return nil
	default:
		return ErrBufferFull
	}
}

// processTDR adds TDR to buffer
func (h *Handler) processTDR(tdr *TDR) {
	h.bufferMu.Lock()
	defer h.bufferMu.Unlock()

	h.buffer = append(h.buffer, tdr)

	// Flush if buffer is full
	if len(h.buffer) >= h.config.BufferSize {
		go func() {
			if err := h.flushBuffer(); err != nil {
				h.logger.Error("Failed to flush full buffer", zap.Error(err))
			}
		}()
	}
}

// flushBuffer flushes buffer to S3
func (h *Handler) flushBuffer() error {
	h.bufferMu.Lock()
	if len(h.buffer) == 0 {
		h.bufferMu.Unlock()
		return nil
	}

	// Take snapshot of buffer
	tdrs := make([]*TDR, len(h.buffer))
	copy(tdrs, h.buffer)
	h.buffer = h.buffer[:0] // Clear buffer
	h.bufferMu.Unlock()

	h.logger.Info("Flushing TDR buffer", zap.Int("count", len(tdrs)))

	// Serialize TDRs to JSON Lines format
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, tdr := range tdrs {
		if err := encoder.Encode(tdr); err != nil {
			h.logger.Error("Failed to encode TDR", zap.Error(err))
			continue
		}
	}

	data := buf.Bytes()
	originalSize := len(data)

	// Compress if enabled
	if h.config.CompressionEnabled {
		compressed, err := h.compress(data)
		if err != nil {
			h.logger.Error("Failed to compress data", zap.Error(err))
		} else {
			data = compressed
			h.metrics.BytesCompressed.Add(uint64(originalSize - len(data)))
		}
	}

	// Upload to S3
	startTime := time.Now()
	if err := h.uploadToS3(data, len(tdrs)); err != nil {
		h.logger.Error("Failed to upload to S3", zap.Error(err))
		h.metrics.TDRsFailed.Add(uint64(len(tdrs)))
		h.metrics.S3Errors.Add(1)
		return err
	}

	// Update metrics
	uploadTime := time.Since(startTime)
	h.metrics.TDRsUploaded.Add(uint64(len(tdrs)))
	h.metrics.BytesUploaded.Add(uint64(len(data)))
	h.metrics.ObjectsUploaded.Add(1)
	h.metrics.BufferFlushes.Add(1)
	h.metrics.UploadLatencySum.Add(uint64(uploadTime.Nanoseconds()))
	h.metrics.UploadLatencyCount.Add(1)
	h.metrics.mu.Lock()
	h.metrics.LastFlush = time.Now()
	h.metrics.mu.Unlock()

	h.logger.Info("TDR buffer flushed successfully",
		zap.Int("count", len(tdrs)),
		zap.Int("bytes", len(data)),
		zap.Duration("upload_time", uploadTime))

	return nil
}

// uploadToS3 uploads data to AWS S3
func (h *Handler) uploadToS3(data []byte, count int) error {
	// Generate S3 key with timestamp partitioning
	now := time.Now()
	key := fmt.Sprintf("%s/%s/%d/%02d/%02d/%s-%d.jsonl",
		h.config.S3Prefix,
		h.config.NodeID,
		now.Year(),
		now.Month(),
		now.Day(),
		now.Format("20060102-150405"),
		count)

	if h.config.CompressionEnabled {
		key += ".gz"
	}

	// Prepare upload input
	input := &s3.PutObjectInput{
		Bucket:       aws.String(h.config.S3Bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(data),
		ContentType:  aws.String("application/x-ndjson"),
		StorageClass: types.StorageClass(h.config.S3StorageClass),
		Metadata: map[string]string{
			"node-id":      h.config.NodeID,
			"cluster-name": h.config.ClusterName,
			"tdr-count":    fmt.Sprintf("%d", count),
			"compressed":   fmt.Sprintf("%t", h.config.CompressionEnabled),
		},
	}

	// Add S3 Object Lock if enabled
	if h.config.ObjectLockEnabled {
		retainUntil := time.Now().AddDate(0, 0, h.config.ObjectLockDays)
		input.ObjectLockMode = types.ObjectLockMode(h.config.ObjectLockMode)
		input.ObjectLockRetainUntilDate = &retainUntil
	}

	// Upload
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := h.s3Client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("S3 PutObject failed: %w", err)
	}

	return nil
}

// compress compresses data using gzip
func (h *Handler) compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, h.config.CompressionLevel)
	if err != nil {
		return nil, err
	}

	if _, err := writer.Write(data); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encryptPII encrypts PII fields
func (h *Handler) encryptPII(tdr *TDR) error {
	// Encrypt calling party
	if tdr.CallingParty != "" {
		encrypted, err := h.encryptor.encrypt([]byte(tdr.CallingParty))
		if err != nil {
			return err
		}
		tdr.CallingParty = hex.EncodeToString(encrypted)
		h.metrics.FieldsEncrypted.Add(1)
	}

	// Encrypt called party
	if tdr.CalledParty != "" {
		encrypted, err := h.encryptor.encrypt([]byte(tdr.CalledParty))
		if err != nil {
			return err
		}
		tdr.CalledParty = hex.EncodeToString(encrypted)
		h.metrics.FieldsEncrypted.Add(1)
	}

	// Encrypt IMSI
	if tdr.IMSI != "" {
		encrypted, err := h.encryptor.encrypt([]byte(tdr.IMSI))
		if err != nil {
			return err
		}
		tdr.IMSI = hex.EncodeToString(encrypted)
		h.metrics.FieldsEncrypted.Add(1)
	}

	// Encrypt MSISDN
	if tdr.MSISDN != "" {
		encrypted, err := h.encryptor.encrypt([]byte(tdr.MSISDN))
		if err != nil {
			return err
		}
		tdr.MSISDN = hex.EncodeToString(encrypted)
		h.metrics.FieldsEncrypted.Add(1)
	}

	return nil
}

// generateHMAC generates HMAC-SHA256 for searchable encryption
func (h *Handler) generateHMAC(value string) string {
	if h.encryptor == nil {
		return ""
	}

	mac := hmac.New(sha256.New, h.encryptor.hmacSecret)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// initS3Client initializes AWS S3 client
func (h *Handler) initS3Client(ctx context.Context) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(h.config.S3Region),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	h.s3Client = s3.NewFromConfig(cfg)

	h.logger.Info("AWS S3 client initialized",
		zap.String("region", h.config.S3Region),
		zap.String("bucket", h.config.S3Bucket))

	return nil
}

// updateTypeMetrics updates type-specific metrics
func (h *Handler) updateTypeMetrics(recordType RecordType) {
	switch recordType {
	case RecordTypeSMS:
		h.metrics.SMSTDRs.Add(1)
	case RecordTypeVoiceCall:
		h.metrics.VoiceCallTDRs.Add(1)
	case RecordTypeDataSession:
		h.metrics.DataSessionTDRs.Add(1)
	}
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	avgLatency := float64(0)
	if count := h.metrics.UploadLatencyCount.Load(); count > 0 {
		sum := h.metrics.UploadLatencySum.Load()
		avgLatency = float64(sum) / float64(count) / 1e6 // Convert to ms
	}

	h.bufferMu.Lock()
	bufferSize := len(h.buffer)
	h.bufferMu.Unlock()

	h.metrics.mu.RLock()
	lastFlush := h.metrics.LastFlush
	h.metrics.mu.RUnlock()

	return &HealthStatus{
		Enabled:          h.config.Enabled,
		NodeID:           h.config.NodeID,
		S3Bucket:         h.config.S3Bucket,
		TDRsGenerated:    h.metrics.TDRsGenerated.Load(),
		TDRsUploaded:     h.metrics.TDRsUploaded.Load(),
		TDRsFailed:       h.metrics.TDRsFailed.Load(),
		BytesUploaded:    h.metrics.BytesUploaded.Load(),
		ObjectsUploaded:  h.metrics.ObjectsUploaded.Load(),
		AverageLatencyMs: avgLatency,
		BufferSize:       bufferSize,
		LastFlush:        lastFlush,
		LastUpdate:       time.Now(),
	}
}

// validateConfig validates TDR configuration
func validateConfig(cfg *Config) error {
	if !cfg.Enabled {
		return nil
	}

	if cfg.S3Bucket == "" {
		return errors.New("s3_bucket is required")
	}
	if cfg.S3Region == "" {
		return errors.New("s3_region is required")
	}
	if cfg.NodeID == "" {
		return errors.New("node_id is required")
	}

	// Set defaults
	if cfg.BufferSize == 0 {
		cfg.BufferSize = DefaultBufferSize
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = DefaultChannelBufferSize
	}
	if cfg.CompressionLevel == 0 {
		cfg.CompressionLevel = DefaultCompressionLevel
	}
	if cfg.S3StorageClass == "" {
		cfg.S3StorageClass = "INTELLIGENT_TIERING"
	}
	if cfg.S3Prefix == "" {
		cfg.S3Prefix = "tdrs"
	}

	return nil
}

// Encryptor implementation

func newEncryptor(cfg *Config) (*Encryptor, error) {
	// Generate AES key from HMAC secret (in production, use KMS)
	key := sha256.Sum256([]byte(cfg.HMACSecret))

	return &Encryptor{
		aesKey:     key[:],
		hmacSecret: []byte(cfg.HMACSecret),
		kmsKeyID:   cfg.KMSKeyID,
	}, nil
}

// encrypt encrypts data using AES-256-GCM
func (e *Encryptor) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.aesKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// decrypt decrypts data using AES-256-GCM
func (e *Encryptor) decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.aesKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
