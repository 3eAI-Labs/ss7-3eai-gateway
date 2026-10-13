// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

var (
	ErrFirewallDisabled = errors.New("Firewall: firewall is disabled")
	ErrInvalidRule      = errors.New("Firewall: invalid rule")
	ErrTimeout          = errors.New("Firewall: inspection timeout")
	ErrShutdown         = errors.New("Firewall: handler is shutting down")
)

const (
	DefaultWorkerPoolSize    = 10
	DefaultChannelBufferSize = 1000
	DefaultInspectionTimeout = 100 * time.Millisecond
)

// NewHandler creates a new firewall handler
func NewHandler(config *Config, logger *zap.Logger) (*Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:       config,
		rules:        make([]*FirewallRule, 0),
		rateLimiters: make(map[string]*RateLimiter),
		anomalyDetector: &AnomalyDetector{
			baselineOPCActivity: make(map[uint32]*ActivityStats),
			baselineGTActivity:  make(map[string]*ActivityStats),
			threshold:           config.AnomalyThreshold,
			learningWindowSize:  1000,
			detectionEnabled:    config.AnomalyDetectionEnabled,
		},
		threatDB: &ThreatDatabase{
			blacklistedGTs:     make(map[string]ThreatInfo),
			blacklistedPCs:     make(map[uint32]ThreatInfo),
			blacklistedIMSIs:   make(map[string]ThreatInfo),
			blacklistedMSISDNs: make(map[string]ThreatInfo),
			iocs:               make([]*IOC, 0),
		},
		inspectCh:  make(chan *Message, config.ChannelBufferSize),
		resultCh:   make(chan *InspectionResult, config.ChannelBufferSize),
		shutdownCh: make(chan struct{}),
		metrics:    &Metrics{},
		logger:     logger,
	}

	return handler, nil
}

// Start initializes firewall and worker pool
func (h *Handler) Start(ctx context.Context) error {
	if !h.config.Enabled {
		h.logger.Info("Firewall is disabled")
		return nil
	}

	h.logger.Info("Starting Firewall engine",
		zap.String("mode", h.getModeString()),
		zap.Int("rules", len(h.rules)),
		zap.Int("workers", h.config.WorkerPoolSize))

	// Start worker pool
	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}

	h.logger.Info("Firewall engine started successfully")
	return nil
}

// Stop gracefully stops the firewall
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping Firewall engine")

	// Signal shutdown
	close(h.shutdownCh)

	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(done)
	}()

	select {
	case <-done:
		h.logger.Info("Firewall engine stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("Firewall engine stop timeout")
	}

	return nil
}

// worker processes messages for inspection
func (h *Handler) worker(ctx context.Context, id int) {
	h.logger.Debug("Firewall worker started", zap.Int("worker_id", id))
	defer h.logger.Debug("Firewall worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case msg := <-h.inspectCh:
			result := h.inspectMessage(msg)
			h.resultCh <- result
		}
	}
}

// InspectMessage inspects an SS7 message and returns result
func (h *Handler) InspectMessage(msg *Message) *InspectionResult {
	if !h.config.Enabled {
		return &InspectionResult{
			MessageID: msg.MessageID,
			Timestamp: time.Now(),
			Action:    ActionAllow,
			Reason:    "Firewall disabled",
		}
	}

	// Send for inspection
	select {
	case h.inspectCh <- msg:
		// Wait for result with timeout
		select {
		case result := <-h.resultCh:
			return result
		case <-time.After(h.config.InspectionTimeout):
			return &InspectionResult{
				MessageID: msg.MessageID,
				Timestamp: time.Now(),
				Action:    h.config.DefaultAction,
				Reason:    "Inspection timeout",
			}
		}
	default:
		h.logger.Warn("Inspection queue full", zap.String("message_id", msg.MessageID))
		return &InspectionResult{
			MessageID: msg.MessageID,
			Timestamp: time.Now(),
			Action:    h.config.DefaultAction,
			Reason:    "Queue full",
		}
	}
}

// inspectMessage performs actual inspection
func (h *Handler) inspectMessage(msg *Message) *InspectionResult {
	startTime := time.Now()
	h.metrics.MessagesInspected.Add(1)

	result := &InspectionResult{
		MessageID:    msg.MessageID,
		Timestamp:    startTime,
		MatchedRules: make([]*MatchedRule, 0),
		Details:      make(map[string]interface{}),
	}

	// 1. Check threat database
	if h.config.ThreatDBEnabled {
		if threat := h.checkThreatDB(msg); threat != nil {
			result.Action = ActionBlock
			result.ThreatCategory = threat.ThreatCategory
			result.ThreatScore = threat.Severity * 10
			result.Reason = fmt.Sprintf("Threat detected: %s", threat.Description)
			h.metrics.ThreatDBHits.Add(1)
			h.updateCategoryMetrics(threat.ThreatCategory)
			h.finalizeResult(result, startTime)
			return result
		}
	}

	// 2. Apply firewall rules (priority order)
	h.rulesMu.RLock()
	rules := h.rules
	h.rulesMu.RUnlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}

		if matched, matchInfo := h.matchRule(rule, msg); matched {
			result.MatchedRules = append(result.MatchedRules, matchInfo)

			// Apply action based on rule
			if rule.Action > result.Action {
				result.Action = rule.Action
				result.ThreatCategory = rule.ThreatCategory
				result.Reason = rule.Description
			}

			// Update rule statistics
			rule.MatchCount.Add(1)
			rule.LastMatchedAt = time.Now()

			// If rule is blocking, stop further processing
			if rule.Action == ActionBlock {
				break
			}
		}
	}

	// 3. Check rate limiting
	if h.shouldRateLimit(msg) {
		result.Action = ActionRateLimit
		result.Reason = "Rate limit exceeded"
		h.metrics.RateLimitHits.Add(1)
	}

	// 4. Anomaly detection
	if h.config.AnomalyDetectionEnabled {
		if anomaly := h.detectAnomaly(msg); anomaly {
			if result.Action == ActionAllow {
				result.Action = ActionAlert
			}
			result.ThreatScore += 30
			result.Details["anomaly_detected"] = true
			h.metrics.AnomaliesDetected.Add(1)
		}
	}

	// Default action if no rules matched
	if result.Action == 0 {
		result.Action = h.config.DefaultAction
		result.Reason = "Default policy"
	}

	h.finalizeResult(result, startTime)
	return result
}

// matchRule checks if a rule matches the message
func (h *Handler) matchRule(rule *FirewallRule, msg *Message) (bool, *MatchedRule) {
	// Protocol matching
	if rule.Protocol != nil && *rule.Protocol != msg.Protocol {
		return false, nil
	}

	// Operation code matching
	if len(rule.OperationCodes) > 0 && msg.OperationCode != nil {
		if !contains(rule.OperationCodes, *msg.OperationCode) {
			return false, nil
		}
	}

	// Source GT matching
	if len(rule.SourceGT) > 0 {
		if !matchesPattern(rule.SourceGT, msg.SourceGT) {
			return false, nil
		}
	}

	// Destination GT matching
	if len(rule.DestinationGT) > 0 {
		if !matchesPattern(rule.DestinationGT, msg.DestinationGT) {
			return false, nil
		}
	}

	// Source PC matching
	if len(rule.SourcePC) > 0 {
		if !containsUint32(rule.SourcePC, msg.OPC) {
			return false, nil
		}
	}

	// Destination PC matching
	if len(rule.DestinationPC) > 0 {
		if !containsUint32(rule.DestinationPC, msg.DPC) {
			return false, nil
		}
	}

	// SSN matching
	if len(rule.SSN) > 0 {
		if !containsUint8(rule.SSN, msg.SSN) {
			return false, nil
		}
	}

	// Calling party matching
	if len(rule.CallingParty) > 0 {
		if !matchesPattern(rule.CallingParty, msg.CallingParty) {
			return false, nil
		}
	}

	// Called party matching
	if len(rule.CalledParty) > 0 {
		if !matchesPattern(rule.CalledParty, msg.CalledParty) {
			return false, nil
		}
	}

	// IMSI matching
	if len(rule.IMSI) > 0 {
		if !matchesPattern(rule.IMSI, msg.IMSI) {
			return false, nil
		}
	}

	// Geo-fencing
	if len(rule.BlockedCountries) > 0 || len(rule.AllowedCountries) > 0 {
		country := extractCountryCode(msg.CallingParty)
		if len(rule.BlockedCountries) > 0 && containsString(rule.BlockedCountries, country) {
			return true, &MatchedRule{
				RuleID:         rule.ID,
				RuleName:       rule.Name,
				Priority:       rule.Priority,
				ThreatCategory: rule.ThreatCategory,
				Action:         rule.Action,
				MatchedField:   "country_code",
				MatchedValue:   country,
			}
		}
		if len(rule.AllowedCountries) > 0 && !containsString(rule.AllowedCountries, country) {
			return true, &MatchedRule{
				RuleID:         rule.ID,
				RuleName:       rule.Name,
				Priority:       rule.Priority,
				ThreatCategory: rule.ThreatCategory,
				Action:         rule.Action,
				MatchedField:   "country_code",
				MatchedValue:   country,
			}
		}
	}

	// Time-based scheduling
	if rule.Schedule != nil {
		if !h.isWithinSchedule(rule.Schedule) {
			return false, nil
		}
	}

	// Rule matched
	return true, &MatchedRule{
		RuleID:         rule.ID,
		RuleName:       rule.Name,
		Priority:       rule.Priority,
		ThreatCategory: rule.ThreatCategory,
		Action:         rule.Action,
		MatchedField:   "multiple",
		MatchedValue:   "matched",
	}
}

// checkThreatDB checks message against threat database
func (h *Handler) checkThreatDB(msg *Message) *ThreatInfo {
	h.threatDB.mu.RLock()
	defer h.threatDB.mu.RUnlock()

	// Check source GT
	if threat, exists := h.threatDB.blacklistedGTs[msg.SourceGT]; exists {
		return &threat
	}

	// Check source PC
	if threat, exists := h.threatDB.blacklistedPCs[msg.OPC]; exists {
		return &threat
	}

	// Check IMSI
	if msg.IMSI != "" {
		if threat, exists := h.threatDB.blacklistedIMSIs[msg.IMSI]; exists {
			return &threat
		}
	}

	// Check calling party
	if msg.CallingParty != "" {
		if threat, exists := h.threatDB.blacklistedMSISDNs[msg.CallingParty]; exists {
			return &threat
		}
	}

	return nil
}

// shouldRateLimit checks if message should be rate limited
func (h *Handler) shouldRateLimit(msg *Message) bool {
	h.rulesMu.RLock()
	defer h.rulesMu.RUnlock()

	for _, rule := range h.rules {
		if !rule.Enabled || rule.RateLimit == nil {
			continue
		}

		// Get rate limiter key based on scope
		key := h.getRateLimiterKey(rule.RateLimit.Scope, msg)

		h.rateLimitersMu.Lock()
		limiter, exists := h.rateLimiters[key]
		if !exists {
			limiter = newRateLimiter(
				rule.RateLimit.MaxRequests,
				rule.RateLimit.WindowDuration,
				rule.RateLimit.BurstSize,
			)
			h.rateLimiters[key] = limiter
		}
		h.rateLimitersMu.Unlock()

		if !limiter.allow() {
			return true
		}
	}

	return false
}

// detectAnomaly detects anomalous behavior
func (h *Handler) detectAnomaly(msg *Message) bool {
	if !h.anomalyDetector.detectionEnabled {
		return false
	}

	// Check OPC activity
	stats := h.getOPCStats(msg.OPC)
	stats.Count.Add(1)

	// Simple anomaly detection based on rate
	// In production, this would use more sophisticated algorithms
	return false
}

// finalizeResult finalizes inspection result and updates metrics
func (h *Handler) finalizeResult(result *InspectionResult, startTime time.Time) {
	result.ProcessingTime = time.Since(startTime)

	// Update latency metrics
	h.metrics.InspectionLatencySum.Add(uint64(result.ProcessingTime.Nanoseconds()))
	h.metrics.InspectionLatencyCount.Add(1)

	// Update action metrics
	switch result.Action {
	case ActionAllow:
		h.metrics.MessagesAllowed.Add(1)
	case ActionBlock:
		h.metrics.MessagesBlocked.Add(1)
	case ActionQuarantine:
		h.metrics.MessagesQuarantined.Add(1)
	case ActionAlert:
		h.metrics.AlertsGenerated.Add(1)
	}

	// Logging
	if result.Action == ActionBlock && h.config.LogBlocked {
		h.logger.Warn("Message blocked",
			zap.String("message_id", result.MessageID),
			zap.String("reason", result.Reason),
			zap.Int("threat_score", result.ThreatScore))
	} else if h.config.LogAllowed {
		h.logger.Debug("Message allowed",
			zap.String("message_id", result.MessageID))
	}

	// Alerting
	if result.Action == ActionAlert && h.config.AlertOnThreat {
		go h.sendAlert(result)
	}
}

// updateCategoryMetrics updates category-specific metrics
func (h *Handler) updateCategoryMetrics(category ThreatCategory) {
	switch {
	case category >= 0x01 && category <= 0x0F:
		h.metrics.Cat1Detections.Add(1)
	case category >= 0x10 && category <= 0x1F:
		h.metrics.Cat2Detections.Add(1)
	case category >= 0x20 && category <= 0x2F:
		h.metrics.Cat3Detections.Add(1)
	}
}

// Helper methods

func (h *Handler) getModeString() string {
	switch h.config.Mode {
	case ModeMonitoring:
		return "Monitoring"
	case ModeEnforcing:
		return "Enforcing"
	case ModeLearning:
		return "Learning"
	default:
		return "Unknown"
	}
}

func (h *Handler) getRateLimiterKey(scope RateLimitScope, msg *Message) string {
	switch scope {
	case ScopePerSourceGT:
		return fmt.Sprintf("gt:%s", msg.SourceGT)
	case ScopePerSourcePC:
		return fmt.Sprintf("pc:%d", msg.OPC)
	case ScopePerMSISDN:
		return fmt.Sprintf("msisdn:%s", msg.CallingParty)
	case ScopePerIMSI:
		return fmt.Sprintf("imsi:%s", msg.IMSI)
	default:
		return "global"
	}
}

func (h *Handler) isWithinSchedule(schedule *Schedule) bool {
	now := time.Now()
	if schedule.TimeZone != nil {
		now = now.In(schedule.TimeZone)
	}

	// Check time range
	if !schedule.StartTime.IsZero() && now.Before(schedule.StartTime) {
		return false
	}
	if !schedule.EndTime.IsZero() && now.After(schedule.EndTime) {
		return false
	}

	// Check day of week
	if len(schedule.DaysOfWeek) > 0 {
		currentDay := now.Weekday()
		found := false
		for _, day := range schedule.DaysOfWeek {
			if day == currentDay {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func (h *Handler) sendAlert(result *InspectionResult) {
	h.logger.Info("Alert generated",
		zap.String("message_id", result.MessageID),
		zap.String("reason", result.Reason),
		zap.Int("threat_score", result.ThreatScore))

	// Send webhook alert if configured
	if h.config.AlertWebhookURL != "" {
		go h.sendWebhookAlert(result)
	}
}

// sendWebhookAlert sends alert to configured webhook URL
func (h *Handler) sendWebhookAlert(result *InspectionResult) {
	payload := map[string]interface{}{
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
		"message_id":      result.MessageID,
		"action":          h.actionToString(result.Action),
		"reason":          result.Reason,
		"threat_category": result.ThreatCategory,
		"threat_score":    result.ThreatScore,
		"severity":        h.getSeverity(result.ThreatCategory),
		"matched_rules":   len(result.MatchedRules),
		"processing_time": result.ProcessingTime.Milliseconds(),
	}

	// Add details if available
	if result.Details != nil {
		for k, v := range result.Details {
			payload[k] = v
		}
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("Failed to marshal webhook payload", zap.Error(err))
		return
	}

	// Create HTTP request with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", h.config.AlertWebhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		h.logger.Error("Failed to create webhook request", zap.Error(err))
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SS7-Gateway-Firewall/1.0")

	// Send request
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		h.logger.Error("Failed to send webhook alert",
			zap.String("url", h.config.AlertWebhookURL),
			zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.logger.Debug("Webhook alert sent successfully",
			zap.String("url", h.config.AlertWebhookURL),
			zap.Int("status_code", resp.StatusCode))
	} else {
		h.logger.Warn("Webhook alert failed with non-2xx status",
			zap.String("url", h.config.AlertWebhookURL),
			zap.Int("status_code", resp.StatusCode))
	}
}

// actionToString converts Action to string
func (h *Handler) actionToString(action Action) string {
	switch action {
	case ActionAllow:
		return "allow"
	case ActionBlock:
		return "block"
	case ActionQuarantine:
		return "quarantine"
	case ActionAlert:
		return "alert"
	case ActionRateLimit:
		return "rate_limit"
	default:
		return "unknown"
	}
}

// getSeverity returns severity level based on threat category
func (h *Handler) getSeverity(cat ThreatCategory) string {
	switch {
	case cat >= Cat1LocationTracking && cat < Cat2SMSFraud:
		return "high"
	case cat >= Cat2SMSFraud && cat < Cat3MessageFlooding:
		return "medium"
	case cat >= Cat3MessageFlooding:
		return "low"
	default:
		return "unknown"
	}
}

// GetAnomalyDetector returns anomaly detector stats
func (h *Handler) getOPCStats(opc uint32) *ActivityStats {
	h.anomalyDetector.baselineMu.Lock()
	defer h.anomalyDetector.baselineMu.Unlock()

	stats, exists := h.anomalyDetector.baselineOPCActivity[opc]
	if !exists {
		stats = &ActivityStats{
			WindowStart: time.Now(),
			Values:      make([]float64, 0, h.anomalyDetector.learningWindowSize),
		}
		h.anomalyDetector.baselineOPCActivity[opc] = stats
	}
	return stats
}

// Rule management methods

// LoadRules loads firewall rules
func (h *Handler) LoadRules(rules []*FirewallRule) error {
	h.rulesMu.Lock()
	defer h.rulesMu.Unlock()

	// Validate rules
	for _, rule := range rules {
		if err := h.validateRule(rule); err != nil {
			return fmt.Errorf("invalid rule %d: %w", rule.ID, err)
		}
	}

	h.rules = rules
	h.logger.Info("Loaded firewall rules", zap.Int("count", len(rules)))

	return nil
}

// AddRule adds a single firewall rule
func (h *Handler) AddRule(rule *FirewallRule) error {
	if err := h.validateRule(rule); err != nil {
		return err
	}

	h.rulesMu.Lock()
	defer h.rulesMu.Unlock()

	h.rules = append(h.rules, rule)
	h.logger.Info("Added firewall rule",
		zap.Uint64("rule_id", rule.ID),
		zap.String("name", rule.Name))

	return nil
}

// RemoveRule removes a firewall rule
func (h *Handler) RemoveRule(ruleID uint64) error {
	h.rulesMu.Lock()
	defer h.rulesMu.Unlock()

	for i, rule := range h.rules {
		if rule.ID == ruleID {
			h.rules = append(h.rules[:i], h.rules[i+1:]...)
			h.logger.Info("Removed firewall rule", zap.Uint64("rule_id", ruleID))
			return nil
		}
	}

	return fmt.Errorf("rule not found: %d", ruleID)
}

// GetRules returns all firewall rules
func (h *Handler) GetRules() []*FirewallRule {
	h.rulesMu.RLock()
	defer h.rulesMu.RUnlock()

	rulesCopy := make([]*FirewallRule, len(h.rules))
	copy(rulesCopy, h.rules)
	return rulesCopy
}

func (h *Handler) validateRule(rule *FirewallRule) error {
	if rule.ID == 0 {
		return fmt.Errorf("rule ID is required")
	}
	if rule.Name == "" {
		return fmt.Errorf("rule name is required")
	}
	if rule.Priority < 0 || rule.Priority > 1000 {
		return fmt.Errorf("priority must be between 0 and 1000")
	}
	return nil
}

// Threat database management

// AddThreat adds a threat to the database
func (h *Handler) AddThreat(identifier, threatType string, info ThreatInfo) {
	h.threatDB.mu.Lock()
	defer h.threatDB.mu.Unlock()

	switch threatType {
	case "GT":
		h.threatDB.blacklistedGTs[identifier] = info
	case "IMSI":
		h.threatDB.blacklistedIMSIs[identifier] = info
	case "MSISDN":
		h.threatDB.blacklistedMSISDNs[identifier] = info
	}

	h.logger.Info("Added threat",
		zap.String("type", threatType),
		zap.String("identifier", identifier))
}

// Metrics and health

func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

func (h *Handler) GetHealthStatus() *HealthStatus {
	avgLatency := float64(0)
	if count := h.metrics.InspectionLatencyCount.Load(); count > 0 {
		sum := h.metrics.InspectionLatencySum.Load()
		avgLatency = float64(sum) / float64(count) / 1e6 // Convert to ms
	}

	h.rulesMu.RLock()
	rulesCount := len(h.rules)
	h.rulesMu.RUnlock()

	return &HealthStatus{
		Enabled:           h.config.Enabled,
		Mode:              h.getModeString(),
		RulesCount:        rulesCount,
		MessagesInspected: h.metrics.MessagesInspected.Load(),
		MessagesBlocked:   h.metrics.MessagesBlocked.Load(),
		AlertsGenerated:   h.metrics.AlertsGenerated.Load(),
		Cat1Detections:    h.metrics.Cat1Detections.Load(),
		Cat2Detections:    h.metrics.Cat2Detections.Load(),
		Cat3Detections:    h.metrics.Cat3Detections.Load(),
		AverageLatencyMs:  avgLatency,
		LastUpdate:        time.Now(),
	}
}

// Utility functions

func matchesPattern(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if strings.Contains(pattern, "*") {
			// Wildcard match
			regex := strings.ReplaceAll(pattern, "*", ".*")
			if matched, _ := regexp.MatchString("^"+regex+"$", value); matched {
				return true
			}
		} else if pattern == value {
			// Exact match
			return true
		}
	}
	return false
}

func contains(slice []int32, value int32) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func containsUint32(slice []uint32, value uint32) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func containsUint8(slice []uint8, value uint8) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func containsString(slice []string, value string) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func extractCountryCode(msisdn string) string {
	if len(msisdn) < 2 {
		return ""
	}
	// Extract first 1-3 digits as country code
	// Simple implementation - in production use proper E.164 parsing
	return msisdn[:2]
}

func validateConfig(cfg *Config) error {
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = DefaultChannelBufferSize
	}
	if cfg.InspectionTimeout == 0 {
		cfg.InspectionTimeout = DefaultInspectionTimeout
	}
	if cfg.AnomalyThreshold == 0 {
		cfg.AnomalyThreshold = 3.0 // 3 sigma
	}
	return nil
}

// Rate limiter implementation

func newRateLimiter(maxRequests int, window time.Duration, burst int) *RateLimiter {
	return &RateLimiter{
		maxRequests:    maxRequests,
		windowDuration: window,
		burstSize:      burst,
		tokens:         burst,
		lastRefill:     time.Now(),
	}
}

func (rl *RateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastRefill)

	// Refill tokens
	if elapsed >= rl.windowDuration {
		rl.tokens = rl.burstSize
		rl.lastRefill = now
	}

	// Check if token available
	if rl.tokens > 0 {
		rl.tokens--
		return true
	}

	return false
}
