// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap

import (
	"errors"
	"fmt"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// HandlerName is the dispatcher key for CAP, used in logs and per-OID metrics.
const HandlerName = "cap"

var errNilMessage = errors.New("cap: nil message")

// Name implements tcap.AppContextHandler.
func (h *Handler) Name() string { return HandlerName }

// HandleTCAPMessage implements tcap.AppContextHandler. It iterates components
// in the TCAP message and routes Invoke components by 3GPP TS 29.078
// operation code through the BER codec into the registered Process* method.
//
// CAP semantics differ from MAP: Invokes from gsmSSF (InitialDP, EventReport,
// ApplyChargingReport) do not expect a ReturnResult — instead the gsmSCF
// reacts with a series of subsequent Invokes (Connect, Continue, …) over
// the same dialogue. Those send paths are not implemented; this slice
// gets the inbound decode → Process* dispatch correct.
//
// Returns error only on routing-layer failures (nil msg). Decode failures
// surface as TCAP Reject components; handler-level failures are logged but
// do not fail the dispatch.
func (h *Handler) HandleTCAPMessage(dialogue *tcap.Dialogue, msg *tcap.Message) error {
	if msg == nil {
		return errNilMessage
	}

	h.metrics.OperationsReceived.Add(uint64(len(msg.Components)))

	for _, comp := range msg.Components {
		if comp == nil {
			continue
		}
		switch comp.Type {
		case tcap.CompTypeInvoke:
			h.routeInvoke(dialogue, comp)

		case tcap.CompTypeReturnResult:
			h.logger.Debug("CAP ReturnResult received",
				zap.Any("invoke_id", comp.InvokeID),
				zap.Uint32("tid", localTID(dialogue)))

		case tcap.CompTypeReturnError:
			h.metrics.Errors.Add(1)
			var errCode int32
			if comp.ErrorCode != nil {
				errCode = *comp.ErrorCode
			}
			h.logger.Warn("CAP ReturnError received",
				zap.Int32("error_code", errCode),
				zap.Any("invoke_id", comp.InvokeID),
				zap.Uint32("tid", localTID(dialogue)))

		case tcap.CompTypeReject:
			h.metrics.Errors.Add(1)
			h.logger.Warn("CAP Reject received",
				zap.Any("invoke_id", comp.InvokeID),
				zap.Uint32("tid", localTID(dialogue)))
		}
	}

	return nil
}

// errCodecDecode tags ASN.1 decode failures so routeInvoke can map them to a
// TCAP Reject (mistyped-parameter).
var errCodecDecode = errors.New("cap: BER decode failed")

// routeInvoke dispatches an incoming CAP Invoke through the codec → Process*
// pipeline. Decode errors emit a TCAP Reject; handler errors are logged.
func (h *Handler) routeInvoke(dialogue *tcap.Dialogue, comp *tcap.Component) {
	if comp.OperationCode == nil {
		h.logger.Warn("CAP Invoke component missing operation code",
			zap.Uint32("tid", localTID(dialogue)))
		h.metrics.Errors.Add(1)
		return
	}

	op := OperationCode(*comp.OperationCode)
	tid := localTID(dialogue)

	// Per-op counters reflect "how many of this op arrived on the wire" — the
	// operability metric carriers expose in their EMS dashboards. Bump before
	// decode so malformed parameters still surface in op-traffic reports.
	bumpPerOpCounter(h.metrics, op)

	if err := h.processInvoke(op, comp.Parameter, tid); err != nil {
		if errors.Is(err, errCodecDecode) {
			h.emitReject(dialogue, comp)
		}
		// Handler-level errors are already metric-counted inside Process*.
	}
}

// processInvoke decodes the parameter for op and runs the registered
// Process* method. Returns errCodecDecode-wrapped error on BER failures so
// the caller emits a Reject; other errors propagate the handler's error.
func (h *Handler) processInvoke(op OperationCode, param []byte, tid uint32) error {
	switch op {
	case OpInitialDP:
		req, err := decodeInitialDPArg(param)
		if err != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("CAP InitialDP decode failed",
				zap.Error(err), zap.Uint32("tid", tid))
			return fmt.Errorf("%w: %v", errCodecDecode, err)
		}
		return h.ProcessInitialDP(req)

	case OpEventReportBCSM:
		req, err := decodeEventReportBCSMArg(param)
		if err != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("CAP EventReportBCSM decode failed",
				zap.Error(err), zap.Uint32("tid", tid))
			return fmt.Errorf("%w: %v", errCodecDecode, err)
		}
		return h.ProcessEventReportBCSM(req)

	case OpApplyChargingReport:
		req, err := decodeApplyChargingReportArg(param)
		if err != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("CAP ApplyChargingReport decode failed",
				zap.Error(err), zap.Uint32("tid", tid))
			return fmt.Errorf("%w: %v", errCodecDecode, err)
		}
		// ApplyChargingReport reuses the ApplyChargingHandler interface in v1;
		// the typed *Request struct flows through ProcessApplyCharging via a
		// shape-compatible wrapper. Carrier integrations that need richer
		// dispatch can split the interface in a later slice.
		return h.ProcessApplyCharging(&ApplyChargingRequest{
			ACHBillingChargingCharacteristics: req.CallResult,
		})

	case OpConnect, OpContinue, OpReleaseCall, OpApplyCharging,
		OpRequestReportBCSMEvent, OpFurnishChargingInformation,
		OpPlayAnnouncement, OpPromptAndCollectUserInformation,
		OpResetTimer, OpActivityTest:
		// gsmSCF→gsmSSF Invokes — we shouldn't normally receive these as
		// gsmSCF. Counters already bumped by routeInvoke; just trace.
		h.logger.Info("CAP gsmSCF→gsmSSF op observed (no-op for gateway)",
			zap.Int32("op_code", int32(op)),
			zap.Uint32("tid", tid))
		return nil
	}

	h.logger.Warn("CAP unsupported operation code",
		zap.Int32("op_code", int32(op)),
		zap.Uint32("tid", tid))
	h.metrics.Errors.Add(1)
	return nil
}

// emitReject builds an outbound TCAP END containing a Reject component
// (Q.773 §3.1.5.3.3 — Invoke / mistypedParameter) for malformed Invokes.
func (h *Handler) emitReject(dialogue *tcap.Dialogue, comp *tcap.Component) {
	if h.tcapOutbound == nil || dialogue == nil || len(dialogue.RemoteTID) == 0 {
		return
	}
	out := &tcap.Message{
		MessageType:    tcap.MsgTypeEnd,
		DestinationTID: dialogue.RemoteTID,
		Dialogue:       dialogue,
		Components: []*tcap.Component{{
			Type:     tcap.CompTypeReject,
			InvokeID: comp.InvokeID,
			ProblemCode: &tcap.ProblemCode{
				Type: tcap.ProblemTypeInvoke,
				Code: tcap.InvokeProblemMistypedParameter,
			},
		}},
	}
	select {
	case h.tcapOutbound <- out:
		h.metrics.OperationsSent.Add(1)
	default:
		h.metrics.Errors.Add(1)
		h.logger.Warn("CAP outbound channel full — Reject dropped",
			zap.Uint32("tid", localTID(dialogue)))
		if h.notSent != nil {
			h.notSent(dialogue, "not sent: CAP outbound channel full")
		}
	}
}

func localTID(dialogue *tcap.Dialogue) uint32 {
	if dialogue == nil {
		return 0
	}
	return dialogue.LocalTID
}

// bumpPerOpCounter increments the per-op counter for op. Wire-arrival metric
// per 3GPP TS 29.078 op enumeration; called from routeInvoke before decode.
// gsmSCF→gsmSSF ops (Connect/Continue/ReleaseCall/ApplyCharging) are counted
// here even though we shouldn't normally receive them as gsmSCF — symmetry
// with the existing send-side counters in handler.go.
func bumpPerOpCounter(m *Metrics, op OperationCode) {
	switch op {
	case OpInitialDP:
		m.InitialDPCount.Add(1)
	case OpEventReportBCSM:
		m.EventReportBCSMCount.Add(1)
	case OpConnect:
		m.ConnectCount.Add(1)
	case OpContinue:
		m.ContinueCount.Add(1)
	case OpReleaseCall:
		m.ReleaseCallCount.Add(1)
	case OpApplyCharging:
		m.ApplyChargingCount.Add(1)
	}
}
