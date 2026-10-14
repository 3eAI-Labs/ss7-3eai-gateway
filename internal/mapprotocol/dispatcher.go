// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"errors"
	"fmt"
	"slices"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// HandlerName is the dispatcher key for MAP, used in logs and per-OID metrics.
const HandlerName = "map"

var errNilMessage = errors.New("map: nil message")

// Name implements tcap.AppContextHandler.
func (h *Handler) Name() string { return HandlerName }

// HandleTCAPMessage implements tcap.AppContextHandler. It iterates components
// in the TCAP message and routes Invoke components by 3GPP TS 29.002 §17
// operation code through the BER codec into the registered Process* method,
// then encodes the response and pushes it as an outbound TCAP End message.
//
// Returns error only on routing-layer failures (nil msg). Decode failures and
// per-operation errors are surfaced over the wire as TCAP Reject /
// ReturnError components and do not fail the dispatch — the dialogue is
// expected to recover at the application layer.
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
			h.handleReturnResult(dialogue, comp)

		case tcap.CompTypeReturnError:
			h.metrics.Errors.Add(1)
			var errCode int32
			if comp.ErrorCode != nil {
				errCode = *comp.ErrorCode
				h.updateErrorMetricsByCode(ErrorCode(errCode))
			}
			h.logger.Warn("MAP ReturnError received",
				zap.Int32("error_code", errCode),
				zap.Any("invoke_id", comp.InvokeID),
				zap.Uint32("tid", localTID(dialogue)))
			detail := "MAP ReturnError without a local error code"
			if comp.ErrorCode != nil {
				detail = ErrorCode(errCode).String()
			}
			h.reportRemoteError(dialogue, ErrorCode(errCode), detail)

		case tcap.CompTypeReject:
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP Reject received",
				zap.Any("invoke_id", comp.InvokeID),
				zap.Uint32("tid", localTID(dialogue)))
			detail := "TCAP Reject"
			if comp.ProblemCode != nil {
				detail = fmt.Sprintf("TCAP Reject problem type=%d code=%d", comp.ProblemCode.Type, comp.ProblemCode.Code)
			}
			h.reportRemoteError(dialogue, 0, detail)
		}
	}

	return nil
}

// reportRemoteError hands a ReturnError / Reject on a dialogue this gateway
// originated to the error sink. Dialogues opened by a peer carry no NATS
// correlation ID and have no waiter to tell.
func (h *Handler) reportRemoteError(dialogue *tcap.Dialogue, code ErrorCode, detail string) {
	if h.errorSink == nil || dialogue == nil || dialogue.NATSCorrelationID == "" || !dialogue.MarkAnswered() {
		return
	}
	h.errorSink(dialogue.NATSCorrelationID, OperationCode(dialogue.OriginatingOpCode), code, detail)
}

// routeInvoke dispatches an incoming MAP Invoke through the codec → Process*
// → response-encode → outbound-emit pipeline. The response component is sent
// in a TCAP END message: short MAP transactions (UpdateLocation, etc.)
// terminate the dialogue with the result.
func (h *Handler) routeInvoke(dialogue *tcap.Dialogue, comp *tcap.Component) {
	if comp.OperationCode == nil {
		h.logger.Warn("MAP Invoke component missing operation code",
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

	respBytes, errCode, err := h.processInvoke(op, comp.Parameter, tid)

	switch {
	case errors.Is(err, errUnsupportedOperation):
		// Q.773 §3.1.5.3.3 — Invoke / unrecognizedOperation: the peer is
		// answered instead of left to its timer.
		h.emitReject(dialogue, comp, tcap.ProblemTypeInvoke, tcap.InvokeProblemUnrecognizedOperation)
	case errors.Is(err, errCodecDecode):
		// Q.773 §3.1.5.3.3 — Invoke / mistypedParameter.
		h.emitReject(dialogue, comp, tcap.ProblemTypeInvoke, tcap.InvokeProblemMistypedParameter)
	case err != nil && errCode != nil:
		h.emitReturnError(dialogue, comp, op, *errCode)
	case err != nil:
		// Handler returned an error without a typed *ErrorCode — treat as
		// systemFailure (TS 29.002 §17.6.1) so the peer can recover.
		h.emitReturnError(dialogue, comp, op, ErrSystemFailure)
	case respBytes != nil:
		h.emitReturnResult(dialogue, comp, op, respBytes)
	}
}

// errCodecDecode tags ASN.1 decode failures so routeInvoke can map them to a
// TCAP Reject (mistyped-parameter) instead of a MAP-level ReturnError.
var errCodecDecode = errors.New("map: BER decode failed")

// errUnsupportedOperation tags an Invoke of an operation this gateway does
// not serve.
var errUnsupportedOperation = errors.New("map: unsupported operation")

// handleReturnResult decodes an inbound ReturnResult component and forwards
// the typed response to the registered sink. The dialogue carries the
// originating op code (set by tcap.Originate) which selects the decoder —
// MAP ReturnResult components do not echo the op code on the wire.
//
// Without an installed sink, the response is logged and dropped: the
// originator (NATS bridge) sees a timeout instead of a status update. This
// matches the "sink optional" contract used in tests.
func (h *Handler) handleReturnResult(dialogue *tcap.Dialogue, comp *tcap.Component) {
	if dialogue == nil {
		h.logger.Debug("MAP ReturnResult on nil dialogue (UNI?), ignoring",
			zap.Any("invoke_id", comp.InvokeID))
		return
	}
	op := OperationCode(dialogue.OriginatingOpCode)
	tid := dialogue.LocalTID
	corrID := dialogue.NATSCorrelationID

	h.logger.Debug("MAP ReturnResult received",
		zap.Int32("op_code", int32(op)),
		zap.Any("invoke_id", comp.InvokeID),
		zap.Uint32("tid", tid),
		zap.String("nats_correlation_id", corrID))

	resp, err := decodeReturnResult(op, comp.Parameter)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.logger.Warn("MAP ReturnResult decode failed",
			zap.Int32("op_code", int32(op)),
			zap.Uint32("tid", tid),
			zap.Error(err))
		// The dialogue has ended with an answer the caller cannot use: tell
		// it so rather than leave it to its deadline.
		h.reportRemoteError(dialogue, 0, "undecodable result: "+err.Error())
		return
	}
	if h.responseSink == nil || resp == nil {
		// No sink wired (test mode) or no decoder for this op — drop silently.
		return
	}
	if !dialogue.MarkAnswered() {
		// A second result, or one after the caller was told of a failure.
		h.logger.Debug("MAP ReturnResult after the outcome was given; dropped",
			zap.Uint32("tid", tid), zap.String("nats_correlation_id", corrID))
		return
	}
	if err := h.responseSink(corrID, op, resp); err != nil {
		h.metrics.Errors.Add(1)
		h.logger.Warn("MAP response sink failed",
			zap.Int32("op_code", int32(op)),
			zap.String("nats_correlation_id", corrID),
			zap.Error(err))
	}
}

// decodeReturnResult dispatches to the per-op response decoder. Returns
// (nil, nil) for op codes whose response is intentionally empty (e.g.
// CancelLocation) or for ops without an originated-response decoder yet.
func decodeReturnResult(op OperationCode, param []byte) (any, error) {
	switch op {
	case OpSendRoutingInfoForSM:
		return decodeSendRoutingInfoForSMRes(param)
	case OpMtForwardSM:
		return decodeMtForwardSMRes(param)
	case OpMoForwardSM:
		return decodeMoForwardSMRes(param)
	case OpAnyTimeInterrogation:
		return DecodeAnyTimeInterrogationRes(param)
	case OpProvideSubscriberInfo:
		return DecodeProvideSubscriberInfoRes(param)
	}
	return nil, nil
}

// processInvoke decodes the parameter bytes for op, runs the registered
// Process* method, and returns the encoded response. An operation this
// gateway does not serve returns errUnsupportedOperation.
func (h *Handler) processInvoke(op OperationCode, param []byte, tid uint32) (respBytes []byte, errCode *ErrorCode, err error) {
	switch op {
	case OpUpdateLocation:
		req, derr := decodeUpdateLocationArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP UpdateLocation decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessUpdateLocation(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeUpdateLocationRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpCancelLocation:
		req, derr := decodeCancelLocationArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP CancelLocation decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessCancelLocation(req)
		if perr != nil {
			return nil, ec, perr
		}
		out, eerr := encodeCancelLocationRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpSendRoutingInfo:
		req, derr := decodeSendRoutingInfoArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP SendRoutingInfo decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessSendRoutingInfo(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeSendRoutingInfoRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpProvideRoamingNumber:
		req, derr := decodeProvideRoamingNumberArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP ProvideRoamingNumber decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessProvideRoamingNumber(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeProvideRoamingNumberRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpSendRoutingInfoForSM:
		req, derr := decodeSendRoutingInfoForSMArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP SendRoutingInfoForSM decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessSendRoutingInfoForSM(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeSendRoutingInfoForSMRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpMtForwardSM:
		req, derr := decodeMtForwardSMArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP MtForwardSM decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessMtForwardSM(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeMtForwardSMRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil

	case OpMoForwardSM:
		req, derr := decodeMoForwardSMArg(param)
		if derr != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP MoForwardSM decode failed",
				zap.Error(derr), zap.Uint32("tid", tid))
			return nil, nil, fmt.Errorf("%w: %v", errCodecDecode, derr)
		}
		resp, ec, perr := h.ProcessMoForwardSM(req)
		if perr != nil || resp == nil {
			return nil, ec, perr
		}
		out, eerr := encodeMoForwardSMRes(resp)
		if eerr != nil {
			return nil, nil, eerr
		}
		return out, nil, nil
	}

	h.logger.Warn("MAP unsupported operation code",
		zap.Int32("op_code", int32(op)),
		zap.Uint32("tid", tid))
	h.metrics.Errors.Add(1)
	return nil, nil, errUnsupportedOperation
}

// emitReturnResult builds an outbound TCAP END containing a single
// ReturnResultLast component carrying the encoded response payload.
func (h *Handler) emitReturnResult(dialogue *tcap.Dialogue, comp *tcap.Component, op OperationCode, payload []byte) {
	if !h.canEmit(dialogue) {
		return
	}
	opCode := int32(op)
	out := &tcap.Message{
		MessageType:    tcap.MsgTypeEnd,
		DestinationTID: dialogue.RemoteTID,
		Dialogue:       dialogue,
		Components: []*tcap.Component{{
			Type:          tcap.CompTypeReturnResult,
			InvokeID:      comp.InvokeID,
			OperationCode: &opCode,
			Parameter:     payload,
		}},
	}
	h.send(dialogue, out)
}

// emitReturnError builds an outbound TCAP END containing a single
// ReturnError component with the typed MAP error code.
func (h *Handler) emitReturnError(dialogue *tcap.Dialogue, comp *tcap.Component, op OperationCode, code ErrorCode) {
	if !h.canEmit(dialogue) {
		return
	}
	errCode := int32(code)
	_ = op // op currently only used for logs
	out := &tcap.Message{
		MessageType:    tcap.MsgTypeEnd,
		DestinationTID: dialogue.RemoteTID,
		Dialogue:       dialogue,
		Components: []*tcap.Component{{
			Type:      tcap.CompTypeReturnError,
			InvokeID:  comp.InvokeID,
			ErrorCode: &errCode,
		}},
	}
	h.send(dialogue, out)
}

// emitReject builds an outbound TCAP END containing a Reject component
// (Q.773 §3.1.5.3.3) for malformed incoming Invokes.
func (h *Handler) emitReject(dialogue *tcap.Dialogue, comp *tcap.Component, problemType uint8, problemCode uint8) {
	if !h.canEmit(dialogue) {
		return
	}
	out := &tcap.Message{
		MessageType:    tcap.MsgTypeEnd,
		DestinationTID: dialogue.RemoteTID,
		Dialogue:       dialogue,
		Components: []*tcap.Component{{
			Type:        tcap.CompTypeReject,
			InvokeID:    comp.InvokeID,
			ProblemCode: &tcap.ProblemCode{Type: problemType, Code: problemCode},
		}},
	}
	h.send(dialogue, out)
}

// canEmit returns true if the handler has a TCAP outbound channel installed
// AND the dialogue has a usable RemoteTID. Tests deliberately exercise the
// codec path without a TCAP backend; in that case we silently skip emission.
func (h *Handler) canEmit(dialogue *tcap.Dialogue) bool {
	return h.tcapOutbound != nil && dialogue != nil && len(dialogue.RemoteTID) > 0
}

func (h *Handler) send(dialogue *tcap.Dialogue, msg *tcap.Message) {
	select {
	case h.tcapOutbound <- msg:
		h.metrics.OperationsSent.Add(1)
	default:
		h.metrics.Errors.Add(1)
		h.logger.Warn("MAP outbound channel full — response dropped",
			zap.Uint32("tid", localTID(dialogue)))
	}
}

// updateErrorMetricsByCode increments per-error-code counters for ReturnError
// components arriving from peers (mirror of the existing updateErrorMetrics
// path used by Process*).
func (h *Handler) updateErrorMetricsByCode(code ErrorCode) {
	switch code {
	case ErrUnknownSubscriber:
		h.metrics.UnknownSubscriberErrors.Add(1)
	case ErrRoamingNotAllowed:
		h.metrics.RoamingNotAllowedErrors.Add(1)
	case ErrSystemFailure:
		h.metrics.SystemFailureErrors.Add(1)
	}
}

func localTID(dialogue *tcap.Dialogue) uint32 {
	if dialogue == nil {
		return 0
	}
	return dialogue.LocalTID
}

// bumpPerOpCounter increments the per-op counter for op. Wire-arrival metric
// per 3GPP TS 29.002 op enumeration; called from routeInvoke before decode.
func bumpPerOpCounter(m *Metrics, op OperationCode) {
	switch op {
	case OpUpdateLocation:
		m.UpdateLocationCount.Add(1)
	case OpCancelLocation:
		m.CancelLocationCount.Add(1)
	case OpSendRoutingInfo:
		m.SendRoutingInfoCount.Add(1)
	case OpProvideRoamingNumber:
		m.ProvideRoamingNumberCount.Add(1)
	case OpAnyTimeInterrogation:
		m.AnyTimeInterrogationCount.Add(1)
	case OpSendRoutingInfoForSM:
		m.SendRoutingInfoForSMCount.Add(1)
	case OpMtForwardSM:
		m.MtForwardSMCount.Add(1)
	case OpMoForwardSM:
		m.MoForwardSMCount.Add(1)
	}
}

// VersionFallback is the TCAP version-fallback table for the operations
// this gateway originates (TS 29.002 §5.2.1): when a peer refuses a version 3
// short-message context and proposes version 2 of the same context, the
// request is sent again under it.
//   - shortMsgGateway v3 → v2: SendRoutingInfoForSM, unchanged.
//   - shortMsgMT-Relay v3 → v2: the operation is forwardSM (op 46) in
//     version 2 (§17.2.2.22, §17.2.2.24). Its argument is sent as encoded for
//     MT-ForwardSM: sm-RP-DA, sm-RP-OA, sm-RP-UI. That this matches
//     version 2's ForwardSM-Arg is from knowledge of MAP v2; the v2 ASN.1 is
//     no longer in TS 29.002 and was not checked.
func VersionFallback(refused []int, logicalOp int32, proposed []int) (*tcap.ApplicationContext, int32, bool) {
	is := func(ac tcap.ApplicationContext, oid []int) bool { return slices.Equal(ac.OID, oid) }
	switch {
	case is(tcap.ACShortMsgGatewayV3, refused) && is(tcap.ACShortMsgGatewayV2, proposed) &&
		logicalOp == int32(OpSendRoutingInfoForSM):
		return &tcap.ACShortMsgGatewayV2, logicalOp, true
	case is(tcap.ACShortMsgMTRelayV3, refused) && is(tcap.ACShortMsgMTRelayV2, proposed) &&
		logicalOp == int32(OpMtForwardSM):
		return &tcap.ACShortMsgMTRelayV2, int32(OpMoForwardSM), true // op 46 = forwardSM in v2
	}
	return nil, 0, false
}
