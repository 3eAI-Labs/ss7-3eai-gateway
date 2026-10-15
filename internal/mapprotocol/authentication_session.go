// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Authentication information retrieval (infoRetrievalContext-v3) and MS
// purging (msPurgingContext-v3) dialogues, with the gateway as HLR. A VLR
// or SGSN opens each with one request; the gateway asks the application
// and answers.
//
// sendAuthenticationInfo's result may be divided by MAP segmentation, the
// mechanism TS 29.002 Annex C recommends for it in version 3 (C.3.3): each
// part a ReturnResultLast, all but the last in a CONTINUE, and the VLR
// asks for the next with a new sendAuthenticationInfo without argument.
// TC-Result-NL and the empty Continue are not allowed for it, SCCP
// segmentation is "risky".

// HLRRequestHandler is the gateway side of these dialogues. Its methods
// run on a TCAP worker and must not block.
type HLRRequestHandler interface {
	// SendAuthenticationInfo: a VLR or SGSN opened the dialogue with
	// sendAuthenticationInfo. It reports false when the dialogue already
	// has a request, and the Invoke is rejected.
	SendAuthenticationInfo(d *tcap.Dialogue, invokeID int32, arg *SendAuthenticationInfoArg) bool
	// MoreAuthenticationInfo: a later sendAuthenticationInfo in the
	// dialogue, asking for the next part of the result. It reports false
	// when no part is waiting, and the Invoke is rejected.
	MoreAuthenticationInfo(d *tcap.Dialogue, invokeID int32) bool
	// PurgeMS: a VLR or SGSN opened the dialogue with purgeMS. It reports
	// false when the dialogue already has a request.
	PurgeMS(d *tcap.Dialogue, invokeID int32, arg *PurgeMSArg) bool
	// HLRRequestEnded: the peer ended the dialogue — END (aborted false)
	// or ABORT — after the components the message carried.
	HLRRequestEnded(d *tcap.Dialogue, aborted bool, detail string)
	// HLRRequestRejected: the peer rejected what the gateway sent (a part
	// of the result it could not take, say) and leaves the dialogue open.
	HLRRequestRejected(d *tcap.Dialogue, detail string)
	// HLRRequestActive reports whether the dialogue has a request: an
	// Invoke refused in it is then rejected in a CONTINUE.
	HLRRequestActive(d *tcap.Dialogue) bool
}

// SetHLRRequestHandler installs the handler of authentication and purging
// dialogues. Set before traffic flows.
func (h *Handler) SetHLRRequestHandler(r HLRRequestHandler) { h.hlrRequest = r }

func isAuthenticationDialogue(d *tcap.Dialogue) bool {
	return d != nil && d.ApplicationContext != nil && slices.Equal(d.ApplicationContext.OID, tcap.ACInfoRetrievalV3.OID)
}

func isPurgeDialogue(d *tcap.Dialogue) bool {
	return d != nil && d.ApplicationContext != nil && slices.Equal(d.ApplicationContext.OID, tcap.ACMSPurgingV3.OID)
}

func isHLRRequestDialogue(d *tcap.Dialogue) bool {
	return isAuthenticationDialogue(d) || isPurgeDialogue(d)
}

// handleHLRRequest routes an authentication or purging dialogue's message.
// The gateway invokes nothing in these dialogues: a ReturnResult or
// ReturnError from the peer is logged and dropped; a Reject of what it sent
// goes to the handler, which closes the dialogue.
func (h *Handler) handleHLRRequest(d *tcap.Dialogue, msg *tcap.Message) {
	ending := msg.MessageType == tcap.MsgTypeEnd || msg.MessageType == tcap.MsgTypeAbort
	wasLive := h.hlrRequest.HLRRequestActive(d)
	for _, comp := range msg.Components {
		if comp == nil || comp.InvokeID == nil {
			continue
		}
		switch {
		case wasLive && !h.hlrRequest.HLRRequestActive(d):
			// The gateway ended the dialogue answering an earlier
			// component of this message: nothing after it can be answered.
			h.logger.Warn("component after the gateway ended the dialogue, dropped", zap.Uint32("tid", localTID(d)))
			return
		case comp.Type == tcap.CompTypeInvoke && ending:
			// An Invoke in the peer's END cannot be answered (Q.774: the
			// dialogue is over); the end is handled below.
			h.metrics.Errors.Add(1)
			h.logger.Warn("Invoke in the peer's END, dropped", zap.Uint32("tid", localTID(d)), zap.Int32("invoke_id", *comp.InvokeID))
			continue
		case comp.Type == tcap.CompTypeInvoke:
			if h.hlrRequestInvoke(d, msg, comp) {
				return // rejected in an END: the dialogue is over
			}
			continue
		case comp.Type == tcap.CompTypeReject && !ending:
			h.metrics.Errors.Add(1)
			detail := "TCAP Reject"
			if p := comp.ProblemCode; p != nil {
				detail = fmt.Sprintf("TCAP Reject problem type=%d code=%d", p.Type, p.Code)
			}
			h.hlrRequest.HLRRequestRejected(d, detail)
			continue
		}
		h.metrics.Errors.Add(1)
		h.logger.Warn("unexpected component in an HLR request dialogue, dropped",
			zap.Uint32("tid", localTID(d)), zap.Int("component_type", int(comp.Type)), zap.Int32("invoke_id", *comp.InvokeID))
	}
	switch msg.MessageType {
	case tcap.MsgTypeEnd:
		h.hlrRequest.HLRRequestEnded(d, false, "ended by the peer")
	case tcap.MsgTypeAbort:
		detail := "dialogue aborted by the peer"
		if msg.PAbort {
			detail = "dialogue aborted by the peer's TCAP provider"
		}
		h.hlrRequest.HLRRequestEnded(d, true, detail)
	}
}

// hlrRequestInvoke takes an Invoke: the request opening the dialogue, in
// the BEGIN or in the CONTINUE after an empty Begin (TS 29.002 §C.2.1), or
// — authentication only — a later request for the next part of the
// result. It reports whether a rejection ended the dialogue.
func (h *Handler) hlrRequestInvoke(d *tcap.Dialogue, msg *tcap.Message, comp *tcap.Component) bool {
	op := OperationCode(-1)
	if comp.OperationCode != nil {
		op = OperationCode(*comp.OperationCode)
	}
	live := h.hlrRequest.HLRRequestActive(d)
	reject := func(problem uint8) bool { return h.rejectInvoke(d, comp, problem, live) }
	auth := isAuthenticationDialogue(d)
	if d.InitiatedLocally() || (msg.MessageType != tcap.MsgTypeBegin && msg.MessageType != tcap.MsgTypeContinue) ||
		(auth && op != OpSendAuthenticationInfo) || (!auth && op != OpPurgeMS) {
		return reject(tcap.InvokeProblemUnrecognizedOperation)
	}
	bumpPerOpCounter(h.metrics, op)
	id := *comp.InvokeID
	if live {
		// A later request. Its argument, if any, is discarded (TS 29.002
		// sendAuthenticationInfo: it shall not be present).
		if !auth || !h.hlrRequest.MoreAuthenticationInfo(d, id) {
			return reject(tcap.InvokeProblemUnrecognizedOperation)
		}
		return false
	}
	if len(comp.Parameter) == 0 {
		h.metrics.Errors.Add(1)
		if auth {
			// The argument is optional in the operation, present in the
			// first request of a dialogue (§8.5.2.3: IMSI "shall be
			// present"): its absence is dataMissing, not a typing error.
			h.emitReturnError(d, comp, op, ErrDataMissing)
			return true
		}
		return reject(tcap.InvokeProblemMistypedParameter)
	}
	var accepted bool
	if auth {
		arg, err := DecodeSendAuthenticationInfoArg(comp.Parameter)
		if err != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP sendAuthenticationInfo decode failed", zap.Error(err), zap.Uint32("tid", localTID(d)))
			return reject(tcap.InvokeProblemMistypedParameter)
		}
		accepted = h.hlrRequest.SendAuthenticationInfo(d, id, arg)
	} else {
		arg, err := DecodePurgeMSArg(comp.Parameter)
		if err != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("MAP purgeMS decode failed", zap.Error(err), zap.Uint32("tid", localTID(d)))
			return reject(tcap.InvokeProblemMistypedParameter)
		}
		accepted = h.hlrRequest.PurgeMS(d, id, arg)
	}
	if !accepted {
		return h.rejectInvoke(d, comp, tcap.InvokeProblemUnrecognizedOperation, h.hlrRequest.HLRRequestActive(d))
	}
	return false
}

// AuthenticationInfoMessage is sendAuthenticationInfo's ReturnResultLast
// with param, an encoded SendAuthenticationInfoRes: in an END when it is
// the last part, in a CONTINUE when more follow. The caller asks TCAP
// whether it fits before sending it with Send.
func (h *Handler) AuthenticationInfoMessage(d *tcap.Dialogue, invokeID int32, param []byte, more bool) *tcap.Message {
	op := int32(OpSendAuthenticationInfo)
	msg := &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: &invokeID, OperationCode: &op, Parameter: param}},
	}
	if more {
		msg.MessageType = tcap.MsgTypeContinue
		msg.OriginatingTID = binary.BigEndian.AppendUint32(nil, d.LocalTID)
	}
	return msg
}

// HLRRequestRefuse ends an authentication or purging dialogue with a
// ReturnError of its operation, in an END.
func (h *Handler) HLRRequestRefuse(d *tcap.Dialogue, invokeID int32, e UserError) error {
	op, allowed := OpPurgeMS, PurgeMSErrors
	if isAuthenticationDialogue(d) {
		op, allowed = OpSendAuthenticationInfo, SendAuthenticationInfoErrors
	}
	if !allowed[e.Code] {
		return fmt.Errorf("%s is not an error operation %d allows", e.Code, op)
	}
	return h.sendUserError(d, invokeID, e)
}

// sendUserError ends d with a ReturnError, its parameter included.
func (h *Handler) sendUserError(d *tcap.Dialogue, invokeID int32, e UserError) error {
	param, err := e.Parameter()
	if err != nil {
		return err
	}
	c := int32(e.Code)
	return h.sendSession(d, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnError, InvokeID: &invokeID, ErrorCode: &c, Parameter: param}},
	})
}

// PurgeMSAccept ends a purging dialogue with purgeMS's ReturnResult.
func (h *Handler) PurgeMSAccept(d *tcap.Dialogue, invokeID int32, res *PurgeMSRes) error {
	param, err := EncodePurgeMSRes(res)
	if err != nil {
		return err
	}
	op := int32(OpPurgeMS)
	return h.sendSession(d, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: &invokeID, OperationCode: &op, Parameter: param}},
	})
}

// Close ends a dialogue the gateway answers with an END without
// components (MAP-CLOSE).
func (h *Handler) Close(d *tcap.Dialogue) error {
	return h.sendSession(d, &tcap.Message{MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d})
}

// Abort aborts a dialogue the gateway answers (TC-U-ABORT).
func (h *Handler) Abort(d *tcap.Dialogue) error {
	return h.sendSession(d, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.PeerTID(), Dialogue: d})
}
