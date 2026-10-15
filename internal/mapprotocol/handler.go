// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
// ussdRouteName is what a route may be called: one NATS subject token
// (ss7client's rule).
var ussdRouteName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

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
	switch t := cfg.USSDSessionTimeout; {
	case t == 0:
		cfg.USSDSessionTimeout = DefaultUSSDSessionTimeout
	case t < MinUSSDSessionTimeout || t > MaxUSSDSessionTimeout:
		return fmt.Errorf("ussd_session_timeout %s is outside %s..%s (TS 29.002 timer ml)", t, MinUSSDSessionTimeout, MaxUSSDSessionTimeout)
	}
	switch t := cfg.HLRAnswerTimeout; {
	case t == 0:
		cfg.HLRAnswerTimeout = DefaultHLRAnswerTimeout
	case t < MinHLRAnswerTimeout || t > MaxHLRAnswerTimeout:
		return fmt.Errorf("hlr_answer_timeout %s is outside %s..%s", t, MinHLRAnswerTimeout, MaxHLRAnswerTimeout)
	}
	if n := cfg.HLRNumber; n != "" && (len(n) > 15 || strings.Trim(n, "0123456789") != "") {
		return fmt.Errorf("hlr_number %q is not E.164 digits (at most 15)", n)
	}
	seen := make(map[string]bool, len(cfg.USSDRoutes))
	for i, r := range cfg.USSDRoutes {
		if r.Prefix == "" || len(r.Prefix) > 182 {
			return fmt.Errorf("ussd_routes[%d]: prefix of %d characters, want 1..182", i, len(r.Prefix))
		}
		if !ussdRouteName.MatchString(r.Route) {
			return fmt.Errorf("ussd_routes[%d]: route %q is not 1..32 of [A-Za-z0-9_-]", i, r.Route)
		}
		if seen[r.Prefix] {
			return fmt.Errorf("ussd_routes[%d]: prefix %q listed twice", i, r.Prefix)
		}
		seen[r.Prefix] = true
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
