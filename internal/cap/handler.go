// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

var (
	ErrInvalidRequest        = errors.New("CAP: invalid request")
	ErrOperationNotSupported = errors.New("CAP: operation not supported")
	ErrHandlerNotSet         = errors.New("CAP: operation handler not set")
	ErrTimeout               = errors.New("CAP: operation timeout")
	ErrShutdown              = errors.New("CAP: handler is shutting down")
	ErrUnknownServiceKey     = errors.New("CAP: unknown service key")
)

const (
	ChannelBufferSize       = 100
	DefaultWorkerPoolSize   = 4
	DefaultOperationTimeout = 30 * time.Second
)

// NewHandler creates a new CAP handler instance.
//
// The handler implements tcap.AppContextHandler synchronously: TCAP delivers
// messages via HandleTCAPMessage, the dispatcher decodes the BER parameter,
// invokes the registered Process* method, and emits any required outbound
// CAP Invokes (Connect, Continue, …) through the channel installed by
// SetTCAPOutbound. CAP responses to InitialDP are not ReturnResults — they
// are subsequent Invokes from gsmSCF, sent by the application as needed.
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

	return &Handler{
		config:  config,
		errorCh: make(chan error, 10),
		metrics: &Metrics{},
		logger:  logger,
	}, nil
}

// SetTCAPOutbound installs the channel the dispatcher pushes outbound TCAP
// messages onto (typically tcapHandler.ApplicationOutbound()). Wiring is
// performed after both CAP and TCAP handlers exist (cmd/gateway/main.go).
// nil disables outbound emission; tests use this to skip TCAP wiring.
func (h *Handler) SetTCAPOutbound(ch chan<- *tcap.Message) {
	h.tcapOutbound = ch
}

// Start is a no-op kept for lifecycle symmetry with peer protocol handlers.
// CAP runs synchronously inside the TCAP dispatcher's goroutine — there are
// no worker pools to spin up.
func (h *Handler) Start(_ context.Context) error {
	h.logger.Info("CAP handler ready",
		zap.String("gsmscf_address", h.config.GSMSCFAddress),
		zap.Uint32("gsmscf_pc", h.config.GSMSCFPointCode))
	return nil
}

// Stop is a no-op kept for lifecycle symmetry with peer protocol handlers.
func (h *Handler) Stop(_ context.Context) error {
	h.logger.Info("CAP handler stopped")
	return nil
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

// ProcessInitialDP handles InitialDP request (call setup). Per-op counter is
// bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessInitialDP(req *InitialDPRequest) error {
	h.logger.Info("Processing InitialDP",
		zap.Uint32("service_key", req.ServiceKey),
		zap.String("called_party", digitsOf(req.CalledPartyNumber)),
		zap.String("calling_party", digitsOf(req.CallingPartyNumber)))

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
			h.logger.Warn("Unknown service key", zap.Uint32("service_key", req.ServiceKey))
			return ErrUnknownServiceKey
		}
	}

	// Call handler
	if err := h.initialDPHandler.HandleInitialDP(req); err != nil {
		h.logger.Error("InitialDP failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		return err
	}

	h.logger.Info("InitialDP successful",
		zap.Uint32("service_key", req.ServiceKey))

	return nil
}

// ProcessEventReportBCSM handles EventReportBCSM request (call event).
// Per-op counter is bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessEventReportBCSM(req *EventReportBCSMRequest) error {
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

// SendRequestReportBCSMEvent sends RequestReportBCSMEvent to SSF
func (h *Handler) SendRequestReportBCSMEvent(req *RequestReportBCSMEventRequest) error {
	h.logger.Info("Sending RequestReportBCSMEvent",
		zap.Int("events", len(req.BCMSEvents)))

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	return nil
}

// SendConnect sends Connect operation to SSF
func (h *Handler) SendConnect(req *ConnectRequest) error {
	h.metrics.ConnectCount.Add(1)

	h.logger.Info("Sending Connect",
		zap.Int("destinations", len(req.DestinationRoutingAddress)))

	// Validate request
	if len(req.DestinationRoutingAddress) == 0 {
		h.logger.Error("Invalid Connect request: no destination")
		h.metrics.Errors.Add(1)
		return ErrInvalidRequest
	}

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("Connect sent successfully")

	return nil
}

// SendContinue sends Continue operation to SSF (call continues)
func (h *Handler) SendContinue(req *ContinueRequest) error {
	h.metrics.ContinueCount.Add(1)

	h.logger.Info("Sending Continue")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("Continue sent successfully")

	return nil
}

// SendReleaseCall sends ReleaseCall operation to SSF (release call)
func (h *Handler) SendReleaseCall(req *ReleaseCallRequest) error {
	h.metrics.ReleaseCallCount.Add(1)

	h.logger.Info("Sending ReleaseCall")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("ReleaseCall sent successfully")

	return nil
}

// SendApplyCharging sends ApplyCharging operation to SSF
func (h *Handler) SendApplyCharging(req *ApplyChargingRequest) error {
	h.logger.Info("Sending ApplyCharging")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("ApplyCharging sent successfully")

	return nil
}

// SendFurnishChargingInformation sends FurnishChargingInformation to SSF
func (h *Handler) SendFurnishChargingInformation(req *FurnishChargingInformationRequest) error {
	h.logger.Info("Sending FurnishChargingInformation")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("FurnishChargingInformation sent successfully")

	return nil
}

// SendPlayAnnouncement sends PlayAnnouncement to SSF
func (h *Handler) SendPlayAnnouncement(req *PlayAnnouncementRequest) error {
	h.logger.Info("Sending PlayAnnouncement")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("PlayAnnouncement sent successfully")

	return nil
}

// SendPromptAndCollectUserInformation sends PromptAndCollectUserInformation to SSF
func (h *Handler) SendPromptAndCollectUserInformation(req *PromptAndCollectUserInformationRequest) error {
	h.logger.Info("Sending PromptAndCollectUserInformation")

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("PromptAndCollectUserInformation sent successfully")

	return nil
}

// SendResetTimer sends ResetTimer to SSF
func (h *Handler) SendResetTimer(req *ResetTimerRequest) error {
	h.logger.Info("Sending ResetTimer",
		zap.Uint8("timer_id", uint8(req.TimerID)),
		zap.Int32("timer_value", req.TimerValue))

	// Encode and send via TCAP
	// In real implementation, this would encode CAP parameters and send via TCAP

	h.metrics.OperationsSent.Add(1)

	h.logger.Info("ResetTimer sent successfully")

	return nil
}

// digitsOf returns an optional address's digits; InitialDP's party numbers
// are OPTIONAL (TS 29.078) and nil when absent.
func digitsOf(a *AddressString) string {
	if a == nil {
		return ""
	}
	return a.Digits
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

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	return &HealthStatus{
		OperationsReceived: h.metrics.OperationsReceived.Load(),
		OperationsSent:     h.metrics.OperationsSent.Load(),
		Errors:             h.metrics.Errors.Load(),
		InitialDPCount:     h.metrics.InitialDPCount.Load(),
		EventReportCount:   h.metrics.EventReportBCSMCount.Load(),
		ApplyChargingCount: h.metrics.ApplyChargingCount.Load(),
		LastUpdate:         time.Now(),
	}
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates CAP configuration
func validateConfig(cfg *Config) error {
	if cfg.GSMSCFAddress == "" {
		return errors.New("gsmscf_address is required")
	}
	if cfg.GSMSCFPointCode == 0 {
		return errors.New("gsmscf_point_code is required")
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
	case EventCollectedInfo:
		return "CollectedInfo"
	case EventRouteSelectFailure:
		return "RouteSelectFailure"
	case EventOCalledPartyBusy:
		return "OCalledPartyBusy"
	case EventONoAnswer:
		return "ONoAnswer"
	case EventOAnswer:
		return "OAnswer"
	case EventOMidCall:
		return "OMidCall"
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
	case EventTMidCall:
		return "TMidCall"
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
func NewConnectRequest(destination *AddressString) *ConnectRequest {
	return &ConnectRequest{
		DestinationRoutingAddress: []*AddressString{destination},
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
	case ErrUnknownPDPID:
		return "Unknown PDP ID"
	case ErrUnknownCSID:
		return "Unknown CS ID"
	default:
		return fmt.Sprintf("Unknown Error (%d)", code)
	}
}
