// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

var (
	ErrInvalidRequest        = errors.New("MAP: invalid request")
	ErrOperationNotSupported = errors.New("MAP: operation not supported")
	ErrHandlerNotSet         = errors.New("MAP: operation handler not set")
	ErrTimeout               = errors.New("MAP: operation timeout")
	ErrShutdown              = errors.New("MAP: handler is shutting down")
)

const (
	ChannelBufferSize       = 100
	DefaultWorkerPoolSize   = 4
	DefaultOperationTimeout = 30 * time.Second
)

// NewHandler creates a new MAP handler instance.
//
// The handler implements tcap.AppContextHandler synchronously: TCAP delivers
// messages via HandleTCAPMessage, the dispatcher decodes the BER parameter,
// invokes the registered Process* method, encodes the response, and pushes
// an outbound *tcap.Message via the channel installed by SetTCAPOutbound.
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
// performed after both MAP and TCAP handlers exist (cmd/gateway/main.go).
// nil disables outbound emission — Process* results are dropped after metric
// accounting (useful in tests that don't want to wire the full TCAP stack).
func (h *Handler) SetTCAPOutbound(ch chan<- *tcap.Message) {
	h.tcapOutbound = ch
}

// SetResponseSink installs the callback fired when an inbound ReturnResult
// arrives for an originated dialogue (i.e. one we opened via tcap.Originate).
// The sink receives the dialogue's NATSCorrelationID, the originating op
// code, and a typed response struct (any of: *SendRoutingInfoForSMResponse,
// *MtForwardSMResponse, *MoForwardSMResponse). Composition root republishes
// the response on the matching NATS subject. nil disables the sink — useful
// in tests that exercise codec correctness without round-tripping NATS.
func (h *Handler) SetResponseSink(sink func(natsCorrelationID string, op OperationCode, resp any) error) {
	h.responseSink = sink
}

// SetErrorSink installs the callback fired when an originated dialogue is
// answered with a ReturnError or a Reject instead of a ReturnResult. nil
// disables it: the caller then only sees its own deadline.
func (h *Handler) SetErrorSink(sink ErrorSink) {
	h.errorSink = sink
}

// Start is a no-op kept for lifecycle symmetry with peer protocol handlers.
// MAP runs synchronously inside the TCAP dispatcher's goroutine — there are
// no worker pools to spin up.
func (h *Handler) Start(_ context.Context) error {
	h.logger.Info("MAP handler ready",
		zap.String("hlr_address", h.config.HLRAddress),
		zap.Uint32("hlr_pc", h.config.HLRPointCode))
	return nil
}

// Stop is a no-op kept for lifecycle symmetry with peer protocol handlers.
func (h *Handler) Stop(_ context.Context) error {
	h.logger.Info("MAP handler stopped")
	return nil
}

// SetUpdateLocationHandler sets the handler for UpdateLocation operations
func (h *Handler) SetUpdateLocationHandler(handler UpdateLocationHandler) {
	h.updateLocationHandler = handler
}

// SetSendRoutingInfoHandler sets the handler for SendRoutingInfo operations
func (h *Handler) SetSendRoutingInfoHandler(handler SendRoutingInfoHandler) {
	h.sendRoutingInfoHandler = handler
}

// SetProvideRoamingNumberHandler sets the handler for ProvideRoamingNumber operations
func (h *Handler) SetProvideRoamingNumberHandler(handler ProvideRoamingNumberHandler) {
	h.provideRoamingNumHandler = handler
}

// SetAnyTimeInterrogationHandler sets the handler for AnyTimeInterrogation operations
func (h *Handler) SetAnyTimeInterrogationHandler(handler AnyTimeInterrogationHandler) {
	h.anyTimeIntHandler = handler
}

// SetCancelLocationHandler sets the handler for CancelLocation operations
func (h *Handler) SetCancelLocationHandler(handler CancelLocationHandler) {
	h.cancelLocationHandler = handler
}

// SetSendRoutingInfoForSMHandler installs the SRI-for-SM (op 45) handler.
func (h *Handler) SetSendRoutingInfoForSMHandler(handler SendRoutingInfoForSMHandler) {
	h.sendRoutingInfoForSMHandler = handler
}

// SetMtForwardSMHandler installs the MT-Forward-SM (op 44) handler.
func (h *Handler) SetMtForwardSMHandler(handler MtForwardSMHandler) {
	h.mtForwardSMHandler = handler
}

// SetMoForwardSMHandler installs the MO-Forward-SM (op 46) handler.
func (h *Handler) SetMoForwardSMHandler(handler MoForwardSMHandler) {
	h.moForwardSMHandler = handler
}

// ProcessUpdateLocation handles UpdateLocation request. Per-op counter is
// bumped at the dispatch entry point (routeInvoke) so malformed parameters
// still register in op-traffic reports; this method only counts errors.
func (h *Handler) ProcessUpdateLocation(req *UpdateLocationRequest) (*UpdateLocationResponse, *ErrorCode, error) {
	h.logger.Info("Processing UpdateLocation",
		zap.String("imsi", string(req.IMSI)),
		zap.String("vlr", req.VLRNumber.Digits))

	if h.updateLocationHandler == nil {
		h.logger.Error("UpdateLocation handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}

	// Validate request
	if err := h.validateUpdateLocationRequest(req); err != nil {
		h.logger.Error("Invalid UpdateLocation request", zap.Error(err))
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, err
	}

	// Call handler
	resp, errCode, err := h.updateLocationHandler.HandleUpdateLocation(req)
	if err != nil {
		h.logger.Error("UpdateLocation failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}

	h.logger.Info("UpdateLocation successful",
		zap.String("imsi", string(req.IMSI)))

	return resp, nil, nil
}

// ProcessCancelLocation handles CancelLocation request. Per-op counter is
// bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessCancelLocation(req *CancelLocationRequest) (*CancelLocationResponse, *ErrorCode, error) {
	h.logger.Info("Processing CancelLocation",
		zap.String("imsi", string(req.IMSI)),
		zap.Uint8("cancellation_type", uint8(req.CancellationType)))

	if h.cancelLocationHandler == nil {
		h.logger.Error("CancelLocation handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}

	// Validate request
	if req.IMSI == "" {
		h.logger.Error("Invalid CancelLocation request: IMSI missing")
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	// Call handler
	resp, errCode, err := h.cancelLocationHandler.HandleCancelLocation(req)
	if err != nil {
		h.logger.Error("CancelLocation failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}

	h.logger.Info("CancelLocation successful",
		zap.String("imsi", string(req.IMSI)))

	return resp, nil, nil
}

// ProcessSendRoutingInfo handles SendRoutingInfo request. Per-op counter is
// bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessSendRoutingInfo(req *SendRoutingInfoRequest) (*SendRoutingInfoResponse, *ErrorCode, error) {
	h.logger.Info("Processing SendRoutingInfo",
		zap.String("msisdn", string(req.MSISDN)),
		zap.Uint8("interrogation_type", uint8(req.InterrogationType)))

	if h.sendRoutingInfoHandler == nil {
		h.logger.Error("SendRoutingInfo handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}

	// Validate request
	if req.MSISDN == "" {
		h.logger.Error("Invalid SendRoutingInfo request: MSISDN missing")
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	// Call handler
	resp, errCode, err := h.sendRoutingInfoHandler.HandleSendRoutingInfo(req)
	if err != nil {
		h.logger.Error("SendRoutingInfo failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}

	h.logger.Info("SendRoutingInfo successful",
		zap.String("msisdn", string(req.MSISDN)))

	return resp, nil, nil
}

// ProcessProvideRoamingNumber handles ProvideRoamingNumber request. Per-op
// counter is bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessProvideRoamingNumber(req *ProvideRoamingNumberRequest) (*ProvideRoamingNumberResponse, *ErrorCode, error) {
	h.logger.Info("Processing ProvideRoamingNumber",
		zap.String("imsi", string(req.IMSI)),
		zap.String("msc", req.MSCNumber.Digits))

	if h.provideRoamingNumHandler == nil {
		h.logger.Error("ProvideRoamingNumber handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}

	// Validate request
	if err := h.validateProvideRoamingNumberRequest(req); err != nil {
		h.logger.Error("Invalid ProvideRoamingNumber request", zap.Error(err))
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, err
	}

	// Call handler
	resp, errCode, err := h.provideRoamingNumHandler.HandleProvideRoamingNumber(req)
	if err != nil {
		h.logger.Error("ProvideRoamingNumber failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}

	h.logger.Info("ProvideRoamingNumber successful",
		zap.String("imsi", string(req.IMSI)),
		zap.String("roaming_number", resp.RoamingNumber.Digits))

	return resp, nil, nil
}

// ProcessAnyTimeInterrogation handles AnyTimeInterrogation request. Per-op
// counter is bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessAnyTimeInterrogation(req *AnyTimeInterrogationRequest) (*AnyTimeInterrogationResponse, *ErrorCode, error) {
	subscriberID := ""
	if req.SubscriberIdentity.IMSI != nil {
		subscriberID = string(*req.SubscriberIdentity.IMSI)
	} else if req.SubscriberIdentity.MSISDN != nil {
		subscriberID = string(*req.SubscriberIdentity.MSISDN)
	}

	h.logger.Info("Processing AnyTimeInterrogation",
		zap.String("subscriber_id", subscriberID),
		zap.String("gsmscf", req.GSMSCFAddress.Digits))

	if h.anyTimeIntHandler == nil {
		h.logger.Error("AnyTimeInterrogation handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}

	// Validate request
	if req.SubscriberIdentity.IMSI == nil && req.SubscriberIdentity.MSISDN == nil {
		h.logger.Error("Invalid AnyTimeInterrogation request: subscriber identity missing")
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	// Call handler
	resp, errCode, err := h.anyTimeIntHandler.HandleAnyTimeInterrogation(req)
	if err != nil {
		h.logger.Error("AnyTimeInterrogation failed", zap.Error(err))
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}

	h.logger.Info("AnyTimeInterrogation successful",
		zap.String("subscriber_id", subscriberID))

	return resp, nil, nil
}

// ProcessSendRoutingInfoForSM handles SRI-for-SM (op 45). Per-op counter is
// bumped at the dispatch entry point (routeInvoke).
func (h *Handler) ProcessSendRoutingInfoForSM(req *SendRoutingInfoForSMRequest) (*SendRoutingInfoForSMResponse, *ErrorCode, error) {
	h.logger.Info("Processing SendRoutingInfoForSM",
		zap.String("msisdn", string(req.MSISDN)),
		zap.String("sca", req.ServiceCentreAddress.Digits))

	if h.sendRoutingInfoForSMHandler == nil {
		h.logger.Error("SendRoutingInfoForSM handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}
	if req.MSISDN == "" {
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	resp, errCode, err := h.sendRoutingInfoForSMHandler.HandleSendRoutingInfoForSM(req)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}
	return resp, nil, nil
}

// ProcessMtForwardSM handles MT-Forward-SM (op 44). Per-op counter bumped
// at routeInvoke.
func (h *Handler) ProcessMtForwardSM(req *MtForwardSMRequest) (*MtForwardSMResponse, *ErrorCode, error) {
	h.logger.Info("Processing MtForwardSM",
		zap.Int("tpdu_len", len(req.SMRPUI)),
		zap.Bool("more_messages", req.MoreMessagesToSend))

	if h.mtForwardSMHandler == nil {
		h.logger.Error("MtForwardSM handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}
	if len(req.SMRPUI) == 0 {
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	resp, errCode, err := h.mtForwardSMHandler.HandleMtForwardSM(req)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}
	return resp, nil, nil
}

// ProcessMoForwardSM handles MO-Forward-SM (op 46). Per-op counter bumped
// at routeInvoke.
func (h *Handler) ProcessMoForwardSM(req *MoForwardSMRequest) (*MoForwardSMResponse, *ErrorCode, error) {
	h.logger.Info("Processing MoForwardSM",
		zap.Int("tpdu_len", len(req.SMRPUI)))

	if h.moForwardSMHandler == nil {
		h.logger.Error("MoForwardSM handler not set")
		h.metrics.Errors.Add(1)
		return nil, nil, ErrHandlerNotSet
	}
	if len(req.SMRPUI) == 0 {
		h.metrics.Errors.Add(1)
		errCode := ErrDataMissing
		return nil, &errCode, ErrInvalidRequest
	}

	resp, errCode, err := h.moForwardSMHandler.HandleMoForwardSM(req)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.updateErrorMetrics(errCode)
		return nil, errCode, err
	}
	return resp, nil, nil
}

// validateUpdateLocationRequest validates UpdateLocation request
func (h *Handler) validateUpdateLocationRequest(req *UpdateLocationRequest) error {
	if req.IMSI == "" {
		return fmt.Errorf("IMSI is required")
	}
	if req.MSCNumber == nil {
		return fmt.Errorf("MSC number is required")
	}
	if req.VLRNumber == nil {
		return fmt.Errorf("VLR number is required")
	}
	return nil
}

// validateProvideRoamingNumberRequest validates ProvideRoamingNumber request
func (h *Handler) validateProvideRoamingNumberRequest(req *ProvideRoamingNumberRequest) error {
	if req.IMSI == "" {
		return fmt.Errorf("IMSI is required")
	}
	if req.MSCNumber == nil {
		return fmt.Errorf("MSC number is required")
	}
	return nil
}

// updateErrorMetrics updates error-specific metrics
func (h *Handler) updateErrorMetrics(errCode *ErrorCode) {
	if errCode == nil {
		return
	}

	switch *errCode {
	case ErrUnknownSubscriber:
		h.metrics.UnknownSubscriberErrors.Add(1)
	case ErrRoamingNotAllowed:
		h.metrics.RoamingNotAllowedErrors.Add(1)
	case ErrSystemFailure:
		h.metrics.SystemFailureErrors.Add(1)
	}
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	return &HealthStatus{
		OperationsReceived:   h.metrics.OperationsReceived.Load(),
		OperationsSent:       h.metrics.OperationsSent.Load(),
		Errors:               h.metrics.Errors.Load(),
		UpdateLocationCount:  h.metrics.UpdateLocationCount.Load(),
		SendRoutingInfoCount: h.metrics.SendRoutingInfoCount.Load(),
		LastUpdate:           time.Now(),
	}
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates MAP configuration
func validateConfig(cfg *Config) error {
	switch cfg.SRIRouting {
	case "":
		cfg.SRIRouting = SRIRoutingMSISDN
	case SRIRoutingMSISDN:
	case SRIRoutingHLR:
		if cfg.HLRGlobalTitle == "" && cfg.HLRPointCode == 0 {
			return errors.New("sri_routing \"hlr\" needs hlr_gt or hlr_pc")
		}
	default:
		return fmt.Errorf("sri_routing must be %q or %q, got %q", SRIRoutingMSISDN, SRIRoutingHLR, cfg.SRIRouting)
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

// Helper functions for creating common responses

// NewUpdateLocationResponse creates a successful UpdateLocation response
func NewUpdateLocationResponse(hlrNumber *AddressString) *UpdateLocationResponse {
	return &UpdateLocationResponse{
		HLRNumber: hlrNumber,
	}
}

// NewSendRoutingInfoResponse creates a successful SendRoutingInfo response
func NewSendRoutingInfoResponse(imsi IMSI, roamingNumber *AddressString) *SendRoutingInfoResponse {
	return &SendRoutingInfoResponse{
		IMSI: &imsi,
		ExtendedRoutingInfo: &ExtendedRoutingInfo{
			RoutingInfo: &RoutingInfo{
				RoamingNumber: roamingNumber,
			},
		},
	}
}

// NewProvideRoamingNumberResponse creates a successful ProvideRoamingNumber response
func NewProvideRoamingNumberResponse(roamingNumber AddressString) *ProvideRoamingNumberResponse {
	return &ProvideRoamingNumberResponse{
		RoamingNumber: roamingNumber,
	}
}

// NewAnyTimeInterrogationResponse creates a successful AnyTimeInterrogation response
func NewAnyTimeInterrogationResponse(info SubscriberInfo) *AnyTimeInterrogationResponse {
	return &AnyTimeInterrogationResponse{
		SubscriberInfo: info,
	}
}

// NewCancelLocationResponse creates a successful CancelLocation response
func NewCancelLocationResponse() *CancelLocationResponse {
	return &CancelLocationResponse{}
}

// Error helper functions

// NewMapError creates a MAP error with code
func NewMapError(code ErrorCode) *ErrorCode {
	return &code
}

// IsMapError checks if error is a specific MAP error
func IsMapError(errCode *ErrorCode, code ErrorCode) bool {
	return errCode != nil && *errCode == code
}

// GetErrorDescription returns human-readable error description
func GetErrorDescription(code ErrorCode) string {
	switch code {
	case ErrUnknownSubscriber:
		return "Unknown Subscriber"
	case ErrUnknownMSC:
		return "Unknown MSC"
	case ErrUnidentifiedSubscriber:
		return "Unidentified Subscriber"
	case ErrAbsentSubscriberSM:
		return "Absent Subscriber SM"
	case ErrUnknownEquipment:
		return "Unknown Equipment"
	case ErrRoamingNotAllowed:
		return "Roaming Not Allowed"
	case ErrIllegalSubscriber:
		return "Illegal Subscriber"
	case ErrBearerServiceNotProvisioned:
		return "Bearer Service Not Provisioned"
	case ErrTeleserviceNotProvisioned:
		return "Teleservice Not Provisioned"
	case ErrIllegalEquipment:
		return "Illegal Equipment"
	case ErrCallBarred:
		return "Call Barred"
	case ErrForwardingViolation:
		return "Forwarding Violation"
	case ErrCUGReject:
		return "CUG Reject"
	case ErrIllegalSSOperation:
		return "Illegal SS Operation"
	case ErrSSErrorStatus:
		return "SS Error Status"
	case ErrSSNotAvailable:
		return "SS Not Available"
	case ErrSSSubscriptionViolation:
		return "SS Subscription Violation"
	case ErrSSIncompatibility:
		return "SS Incompatibility"
	case ErrFacilityNotSupported:
		return "Facility Not Supported"
	case ErrAbsentSubscriber:
		return "Absent Subscriber"
	case ErrSystemFailure:
		return "System Failure"
	case ErrDataMissing:
		return "Data Missing"
	case ErrUnexpectedDataValue:
		return "Unexpected Data Value"
	case ErrResourceLimitation:
		return "Resource Limitation"
	default:
		return fmt.Sprintf("Unknown Error (%d)", code)
	}
}
