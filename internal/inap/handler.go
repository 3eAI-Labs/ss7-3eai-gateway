// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package inap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

var (
	ErrInvalidRequest        = errors.New("INAP: invalid request")
	ErrOperationNotSupported = errors.New("INAP: operation not supported")
	ErrHandlerNotSet         = errors.New("INAP: operation handler not set")
	ErrTimeout               = errors.New("INAP: operation timeout")
	ErrShutdown              = errors.New("INAP: handler is shutting down")
	ErrUnknownServiceKey     = errors.New("INAP: unknown service key")
)

const (
	ChannelBufferSize       = 100
	DefaultWorkerPoolSize   = 4
	DefaultOperationTimeout = 30 * time.Second
)

// NewHandler creates a new INAP handler instance
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
		config:     config,
		errorCh:    make(chan error, 10),
		shutdownCh: make(chan struct{}),
		metrics:    &Metrics{},
		logger:     logger,
	}

	return handler, nil
}

// Start initializes INAP handler and worker pool
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("Starting INAP handler",
		zap.String("scp_address", h.config.SCPAddress),
		zap.Uint32("scp_pc", h.config.SCPPointCode),
		zap.Int("workers", h.config.WorkerPoolSize))

	// Start worker pool
	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}

	h.logger.Info("INAP handler started successfully")
	return nil
}

// Stop gracefully stops the INAP handler
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping INAP handler")

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
		h.logger.Info("INAP handler stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("INAP handler stop timeout")
	}

	return nil
}

// worker processes incoming INAP operations from TCAP
func (h *Handler) worker(ctx context.Context, id int) {
	h.logger.Info("INAP worker started", zap.Int("worker_id", id))
	defer h.logger.Info("INAP worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case msg := <-h.tcapIncomingCh:
			h.handleIncomingMessage(msg)
		}
	}
}

// handleIncomingMessage processes incoming INAP messages from TCAP
func (h *Handler) handleIncomingMessage(rawMsg interface{}) {
	h.metrics.OperationsReceived.Add(1)

	// Parse INAP operation from TCAP component
	// In real implementation, this would extract operation code and parameters

	h.logger.Debug("Received INAP operation")

	// Dispatch to appropriate handler based on operation code
	// This is a placeholder - real implementation would parse TCAP components
}

// SetInitialDPHandler sets the handler for InitialDP operations
func (h *Handler) SetInitialDPHandler(handler InitialDPHandler) {
	h.initialDPHandler = handler
}

// SetEventReportBCSMHandler sets the handler for EventReportBCSM operations
func (h *Handler) SetEventReportBCSMHandler(handler EventReportBCSMHandler) {
	h.eventReportHandler = handler
}

// SetApplyChargingHandler sets the handler for ApplyCharging operations
func (h *Handler) SetApplyChargingHandler(handler ApplyChargingHandler) {
	h.applyChargingHandler = handler
}

// SetCallGapHandler sets the handler for CallGap operations
func (h *Handler) SetCallGapHandler(handler CallGapHandler) {
	h.callGapHandler = handler
}

// ProcessInitialDP handles InitialDP request (call setup)
func (h *Handler) ProcessInitialDP(req *InitialDPRequest) error {
	h.metrics.InitialDPCount.Add(1)

	serviceType := h.getServiceType(req.ServiceKey)

	h.logger.Info("Processing InitialDP",
		zap.Uint32("service_key", uint32(req.ServiceKey)),
		zap.String("service_type", serviceType),
		zap.String("called_party", req.CalledPartyNumber.Digits),
		zap.String("calling_party", req.CallingPartyNumber.Digits))

	if h.initialDPHandler == nil {
		h.logger.Error("InitialDP handler not set")
		h.metrics.Errors.Add(1)
		return ErrHandlerNotSet
	}

	// Validate request
	if err := h.validateInitialDPRequest(req); err != nil {
		h.logger.Error("Invalid InitialDP request", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	// Check service key
	if h.config.ServiceKeys != nil {
		if _, exists := h.config.ServiceKeys[req.ServiceKey]; !exists {
			h.logger.Warn("Unknown service key", zap.Uint32("service_key", uint32(req.ServiceKey)))
			return ErrUnknownServiceKey
		}
	}

	// Update service-specific metrics
	h.updateServiceMetrics(req.ServiceKey)

	// Call handler
	if err := h.initialDPHandler.HandleInitialDP(req); err != nil {
		h.logger.Error("InitialDP failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	h.logger.Info("InitialDP successful",
		zap.Uint32("service_key", uint32(req.ServiceKey)))

	return nil
}

// ProcessEventReportBCSM handles EventReportBCSM request (call event)
func (h *Handler) ProcessEventReportBCSM(req *EventReportBCSMRequest) error {
	h.metrics.EventReportBCSMCount.Add(1)

	eventName := getEventTypeName(req.EventTypeBCSM)
	h.logger.Info("Processing EventReportBCSM",
		zap.String("event_type", eventName))

	if h.eventReportHandler == nil {
		h.logger.Error("EventReportBCSM handler not set")
		h.metrics.Errors.Add(1)
		return ErrHandlerNotSet
	}

	// Update event-specific metrics
	h.updateEventMetrics(req.EventTypeBCSM)

	// Call handler
	if err := h.eventReportHandler.HandleEventReportBCSM(req); err != nil {
		h.logger.Error("EventReportBCSM failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	h.logger.Info("EventReportBCSM successful",
		zap.String("event_type", eventName))

	return nil
}

// ProcessApplyCharging handles ApplyCharging request (charging control)
func (h *Handler) ProcessApplyCharging(req *ApplyChargingRequest) error {
	h.metrics.ApplyChargingCount.Add(1)

	h.logger.Info("Processing ApplyCharging")

	if h.applyChargingHandler == nil {
		h.logger.Error("ApplyCharging handler not set")
		h.metrics.Errors.Add(1)
		return ErrHandlerNotSet
	}

	// Call handler
	if err := h.applyChargingHandler.HandleApplyCharging(req); err != nil {
		h.logger.Error("ApplyCharging failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	h.logger.Info("ApplyCharging successful")

	return nil
}

// ProcessCallGap handles CallGap request (traffic management)
func (h *Handler) ProcessCallGap(req *CallGapRequest) error {
	h.metrics.CallGapCount.Add(1)

	h.logger.Info("Processing CallGap",
		zap.Int32("duration", req.GapIndicators.Duration),
		zap.Int32("gap_interval", req.GapIndicators.GapInterval))

	if h.callGapHandler == nil {
		h.logger.Error("CallGap handler not set")
		h.metrics.Errors.Add(1)
		return ErrHandlerNotSet
	}

	// Call handler
	if err := h.callGapHandler.HandleCallGap(req); err != nil {
		h.logger.Error("CallGap failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	h.logger.Info("CallGap successful")

	return nil
}

// SendRequestReportBCSMEvent sends RequestReportBCSMEvent to SSF
func (h *Handler) SendRequestReportBCSMEvent(req *RequestReportBCSMEventRequest) error {
	h.logger.Info("Sending RequestReportBCSMEvent",
		zap.Int("events", len(req.BCMSEvents)))

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	return nil
}

// SendConnect sends Connect operation to SSF
func (h *Handler) SendConnect(req *ConnectRequest) error {
	h.metrics.ConnectCount.Add(1)

	h.logger.Info("Sending Connect",
		zap.String("destination", req.DestinationRoutingAddress.Digits))

	// Validate request
	if req.DestinationRoutingAddress == nil {
		h.logger.Error("Invalid Connect request: no destination")
		h.metrics.Errors.Add(1)
		return ErrInvalidRequest
	}

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("Connect sent successfully")

	return nil
}

// SendContinue sends Continue operation to SSF (call continues)
func (h *Handler) SendContinue(req *ContinueRequest) error {
	h.metrics.ContinueCount.Add(1)

	h.logger.Info("Sending Continue")

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("Continue sent successfully")

	return nil
}

// SendReleaseCall sends ReleaseCall operation to SSF (release call)
func (h *Handler) SendReleaseCall(req *ReleaseCallRequest) error {
	h.metrics.ReleaseCallCount.Add(1)

	h.logger.Info("Sending ReleaseCall")

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("ReleaseCall sent successfully")

	return nil
}

// SendPlayAnnouncement sends PlayAnnouncement to SSF
func (h *Handler) SendPlayAnnouncement(req *PlayAnnouncementRequest) error {
	h.logger.Info("Sending PlayAnnouncement")

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("PlayAnnouncement sent successfully")

	return nil
}

// SendPromptAndCollectUserInformation sends PromptAndCollectUserInformation to SSF
func (h *Handler) SendPromptAndCollectUserInformation(req *PromptAndCollectUserInformationRequest) error {
	h.logger.Info("Sending PromptAndCollectUserInformation")

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("PromptAndCollectUserInformation sent successfully")

	return nil
}

// SendCallGap sends CallGap to SSF (traffic management)
func (h *Handler) SendCallGap(req *CallGapRequest) error {
	h.logger.Info("Sending CallGap",
		zap.Int32("duration", req.GapIndicators.Duration))

	// Encode and send via TCAP
	// In real implementation, this would encode INAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("CallGap sent successfully")

	return nil
}

// validateInitialDPRequest validates InitialDP request
func (h *Handler) validateInitialDPRequest(req *InitialDPRequest) error {
	if req.ServiceKey == 0 {
		return fmt.Errorf("service key is required")
	}
	if req.CalledPartyNumber == nil {
		return fmt.Errorf("called party number is required")
	}
	if req.CallingPartyNumber == nil {
		return fmt.Errorf("calling party number is required")
	}
	return nil
}

// updateServiceMetrics updates service-specific metrics
func (h *Handler) updateServiceMetrics(serviceKey ServiceKey) {
	switch serviceKey {
	case ServiceKeyTollFree:
		h.metrics.TollFreeCallsCount.Add(1)
	case ServiceKeyPremiumRate:
		h.metrics.PremiumRateCallsCount.Add(1)
	case ServiceKeyVirtualPrivateNetwork:
		h.metrics.VPNCallsCount.Add(1)
	}
}

// updateEventMetrics updates event-specific metrics
func (h *Handler) updateEventMetrics(eventType EventTypeBCSM) {
	switch eventType {
	case EventOAnswer, EventTAnswer:
		h.metrics.AnswerEvents.Add(1)
	case EventODisconnect, EventTDisconnect:
		h.metrics.DisconnectEvents.Add(1)
	case EventOCalledPartyBusy, EventTBusy:
		h.metrics.BusyEvents.Add(1)
	}
}

// getServiceType returns human-readable service type
func (h *Handler) getServiceType(serviceKey ServiceKey) string {
	switch serviceKey {
	case ServiceKeyTollFree:
		return "Toll-Free (800)"
	case ServiceKeyPremiumRate:
		return "Premium Rate (900)"
	case ServiceKeyVirtualPrivateNetwork:
		return "VPN (700)"
	case ServiceKeyUniversalAccess:
		return "Universal Access (888)"
	case ServiceKeyPersonalNumber:
		return "Personal Number (500)"
	case ServiceKeyFreephone:
		return "Freephone (808)"
	case ServiceKeySharedCost:
		return "Shared Cost (844)"
	default:
		if name, exists := h.config.ServiceKeys[serviceKey]; exists {
			return name
		}
		return fmt.Sprintf("Unknown (%d)", serviceKey)
	}
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	return &HealthStatus{
		OperationsReceived:    h.metrics.OperationsReceived.Load(),
		OperationsSent:        h.metrics.OperationsSent.Load(),
		Errors:                h.metrics.Errors.Load(),
		InitialDPCount:        h.metrics.InitialDPCount.Load(),
		EventReportCount:      h.metrics.EventReportBCSMCount.Load(),
		TollFreeCallsCount:    h.metrics.TollFreeCallsCount.Load(),
		PremiumRateCallsCount: h.metrics.PremiumRateCallsCount.Load(),
		LastUpdate:            time.Now(),
	}
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates INAP configuration
func validateConfig(cfg *Config) error {
	if cfg.SCPAddress == "" {
		return errors.New("scp_address is required")
	}
	if cfg.SCPPointCode == 0 {
		return errors.New("scp_point_code is required")
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = ChannelBufferSize
	}
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if cfg.OperationTimeout == 0 {
		cfg.OperationTimeout = DefaultOperationTimeout
	}
	return nil
}

// Helper functions

// getEventTypeName returns human-readable event type name
func getEventTypeName(eventType EventTypeBCSM) string {
	switch eventType {
	case EventOrigAttemptAuthorized:
		return "OrigAttemptAuthorized"
	case EventCollectedInfo:
		return "CollectedInfo"
	case EventAnalyzedInfo:
		return "AnalyzedInfo"
	case EventRouteSelectFailure:
		return "RouteSelectFailure"
	case EventOCalledPartyBusy:
		return "OCalledPartyBusy"
	case EventONoAnswer:
		return "ONoAnswer"
	case EventOAnswer:
		return "OAnswer"
	case EventODisconnect:
		return "ODisconnect"
	case EventOAbandon:
		return "OAbandon"
	case EventTermAttemptAuthorized:
		return "TermAttemptAuthorized"
	case EventTBusy:
		return "TBusy"
	case EventTNoAnswer:
		return "TNoAnswer"
	case EventTAnswer:
		return "TAnswer"
	case EventTDisconnect:
		return "TDisconnect"
	case EventTAbandon:
		return "TAbandon"
	default:
		return fmt.Sprintf("Unknown(%d)", eventType)
	}
}

// NewRequestReportBCSMEvent creates a new RequestReportBCSMEvent request
func NewRequestReportBCSMEvent(events []*BCSMEvent) *RequestReportBCSMEventRequest {
	return &RequestReportBCSMEventRequest{
		BCMSEvents: events,
	}
}

// NewBCSMEvent creates a new BCSM event monitoring request
func NewBCSMEvent(eventType EventTypeBCSM, monitorMode MonitorMode, legID *LegType) *BCSMEvent {
	return &BCSMEvent{
		EventTypeBCSM: eventType,
		MonitorMode:   monitorMode,
		LegID:         legID,
	}
}

// NewConnectRequest creates a new Connect request
func NewConnectRequest(destination *Digits) *ConnectRequest {
	return &ConnectRequest{
		DestinationRoutingAddress: destination,
	}
}

// NewContinueRequest creates a new Continue request
func NewContinueRequest() *ContinueRequest {
	return &ContinueRequest{}
}

// NewReleaseCallRequest creates a new ReleaseCall request
func NewReleaseCallRequest(cause []byte) *ReleaseCallRequest {
	return &ReleaseCallRequest{
		Cause: cause,
	}
}

// NewCallGapRequest creates a new CallGap request
func NewCallGapRequest(criteria *GapCriteria, indicators *GapIndicators) *CallGapRequest {
	return &CallGapRequest{
		GapCriteria:   criteria,
		GapIndicators: indicators,
	}
}

// GetErrorDescription returns human-readable error description
func GetErrorDescription(code ErrorCode) string {
	switch code {
	case ErrCancelled:
		return "Cancelled"
	case ErrCancelFailed:
		return "Cancel Failed"
	case ErrETCFailed:
		return "ETC Failed"
	case ErrImproperCallerResponse:
		return "Improper Caller Response"
	case ErrMissingCustomerRecord:
		return "Missing Customer Record"
	case ErrMissingParameter:
		return "Missing Parameter"
	case ErrParameterOutOfRange:
		return "Parameter Out Of Range"
	case ErrRequestedInfoError:
		return "Requested Info Error"
	case ErrSystemFailure:
		return "System Failure"
	case ErrTaskRefused:
		return "Task Refused"
	case ErrUnavailableResource:
		return "Unavailable Resource"
	case ErrUnexpectedComponentSequence:
		return "Unexpected Component Sequence"
	case ErrUnexpectedDataValue:
		return "Unexpected Data Value"
	case ErrUnexpectedParameter:
		return "Unexpected Parameter"
	case ErrUnknownLegID:
		return "Unknown Leg ID"
	case ErrUnknownResource:
		return "Unknown Resource"
	case ErrChainingRefused:
		return "Chaining Refused"
	case ErrScfReferralDenied:
		return "SCF Referral Denied"
	case ErrScfTaskRefused:
		return "SCF Task Refused"
	default:
		return fmt.Sprintf("Unknown Error (%d)", code)
	}
}
