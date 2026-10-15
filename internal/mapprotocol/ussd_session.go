// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// USSD dialogues (networkUnstructuredSsContext-v2) are sessions: they stay
// open while the subscriber reads and answers, so their components are not
// answered here but handed to a USSDSessionHandler, which answers through
// the USSD* senders below.

// USSDSessionHandler is the gateway side of USSD dialogues. Its methods run
// on a TCAP worker and must not block.
type USSDSessionHandler interface {
	// USSDBegin: the HLR opened a dialogue with processUnstructuredSS-Request
	// (59); open is its MAP-OpenInfo (nil when the AARQ carried none). It
	// reports false when the dialogue already has a session, and the Invoke
	// is rejected.
	USSDBegin(d *tcap.Dialogue, invokeID int32, arg *USSDArg, open *MAPOpenInfo) bool
	// USSDAnswer: the ReturnResult of an unstructuredSS-Request or -Notify
	// the gateway sent; res is nil when the result carried no parameter.
	USSDAnswer(d *tcap.Dialogue, invokeID int32, res *USSDArg)
	// USSDError: a ReturnError (code) or a Reject (code 0) of one. ended:
	// the message carrying it ends the dialogue, and USSDEnded follows.
	USSDError(d *tcap.Dialogue, invokeID int32, code ErrorCode, detail string, ended bool)
	// USSDEnded: the peer ended the dialogue — END (aborted false) or ABORT.
	// It comes after the components the message carried.
	USSDEnded(d *tcap.Dialogue, aborted bool, detail string)
	// USSDActive reports whether the dialogue has a session: an Invoke the
	// gateway refuses in it is then rejected in a CONTINUE, which leaves the
	// session as it is, rather than in an END.
	USSDActive(d *tcap.Dialogue) bool
}

// SetUSSDSessionHandler installs the USSD session handler. Set before
// traffic flows.
func (h *Handler) SetUSSDSessionHandler(s USSDSessionHandler) { h.ussd = s }

func isUSSDDialogue(d *tcap.Dialogue) bool {
	if d == nil || d.ApplicationContext == nil {
		return false
	}
	oid, want := d.ApplicationContext.OID, tcap.ACNetworkUnstructuredSsV2.OID
	if len(oid) != len(want) {
		return false
	}
	for i := range oid {
		if oid[i] != want[i] {
			return false
		}
	}
	return true
}

// handleUSSD routes a USSD dialogue's message to the session handler.
func (h *Handler) handleUSSD(d *tcap.Dialogue, msg *tcap.Message) {
	ended := msg.MessageType == tcap.MsgTypeEnd || msg.MessageType == tcap.MsgTypeAbort
	for _, comp := range msg.Components {
		if comp == nil || comp.InvokeID == nil {
			continue
		}
		id := *comp.InvokeID
		switch comp.Type {
		case tcap.CompTypeInvoke:
			if h.ussdInvoke(d, msg, comp) {
				return // rejected in an END: the dialogue is over
			}
		case tcap.CompTypeReturnResult:
			var res *USSDArg
			if len(comp.Parameter) > 0 {
				var err error
				if res, err = DecodeUSSDArg(comp.Parameter); err != nil {
					h.ussd.USSDError(d, id, 0, "undecodable USSD-Res: "+err.Error(), ended)
					continue
				}
			}
			h.ussd.USSDAnswer(d, id, res)
		case tcap.CompTypeReturnError:
			code := ErrorCode(0)
			detail := "MAP ReturnError without a local error code"
			if comp.ErrorCode != nil {
				code = ErrorCode(*comp.ErrorCode)
				detail = code.String()
			}
			h.ussd.USSDError(d, id, code, detail, ended)
		case tcap.CompTypeReject:
			detail := "TCAP Reject"
			if p := comp.ProblemCode; p != nil {
				detail = fmt.Sprintf("TCAP Reject problem type=%d code=%d", p.Type, p.Code)
			}
			h.ussd.USSDError(d, id, 0, detail, ended)
		}
	}
	switch msg.MessageType {
	case tcap.MsgTypeEnd:
		h.ussd.USSDEnded(d, false, "released by the network")
	case tcap.MsgTypeAbort:
		detail := "dialogue aborted by the network"
		if msg.PAbort {
			detail = "dialogue aborted by the network's TCAP provider"
		}
		h.ussd.USSDEnded(d, true, detail)
	}
}

// ussdInvoke takes an Invoke in a USSD dialogue. Only the HLR's
// processUnstructuredSS-Request opening the dialogue is served — in the
// BEGIN, or in the CONTINUE after an empty Begin (TS 29.002 §C.2.1): the
// gateway is the gsmSCF, which the HLR does not send unstructuredSS-Request
// or -Notify to. It reports whether a rejection ended the dialogue.
func (h *Handler) ussdInvoke(d *tcap.Dialogue, msg *tcap.Message, comp *tcap.Component) bool {
	op := OperationCode(-1)
	if comp.OperationCode != nil {
		op = OperationCode(*comp.OperationCode)
	}
	if op != OpProcessUnstructuredSSReq || d.InitiatedLocally() ||
		(msg.MessageType != tcap.MsgTypeBegin && msg.MessageType != tcap.MsgTypeContinue) {
		return h.ussdReject(d, comp, tcap.InvokeProblemUnrecognizedOperation)
	}
	bumpPerOpCounter(h.metrics, op)
	arg, err := DecodeUSSDArg(comp.Parameter)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.logger.Warn("MAP processUnstructuredSS-Request decode failed", zap.Error(err), zap.Uint32("tid", localTID(d)))
		return h.ussdReject(d, comp, tcap.InvokeProblemMistypedParameter)
	}
	// The MAP-OpenInfo is in the AARQ that opened the dialogue.
	var open *MAPOpenInfo
	if ui := d.OpeningUserInformation; len(ui) > 0 {
		o, ok, err := DecodeMAPOpenInfo(ui)
		if err != nil {
			h.logger.Warn("MAP-OpenInfo undecodable", zap.Error(err), zap.Uint32("tid", localTID(d)))
		} else if ok {
			open = o
		}
	}
	if !h.ussd.USSDBegin(d, *comp.InvokeID, arg, open) {
		return h.ussdReject(d, comp, tcap.InvokeProblemUnrecognizedOperation)
	}
	return false
}

// ussdReject rejects an Invoke: in an END when it is all the dialogue has,
// in a CONTINUE when a session lives in it, even in the BEGIN that opened
// it (Q.774 §3.2.2.2: a Reject does not end the dialogue). It reports
// whether the dialogue ended.
func (h *Handler) ussdReject(d *tcap.Dialogue, comp *tcap.Component, problem uint8) bool {
	if !h.ussd.USSDActive(d) {
		h.emitReject(d, comp, tcap.ProblemTypeInvoke, problem)
		return true
	}
	peer := d.PeerTID()
	if err := h.sendSession(d, &tcap.Message{
		MessageType:    tcap.MsgTypeContinue,
		OriginatingTID: binary.BigEndian.AppendUint32(nil, d.LocalTID),
		DestinationTID: peer,
		Dialogue:       d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReject, InvokeID: comp.InvokeID,
			ProblemCode: &tcap.ProblemCode{Type: tcap.ProblemTypeInvoke, Code: problem}}},
	}); err != nil {
		h.logger.Warn("USSD Reject not sent", zap.Uint32("tid", localTID(d)), zap.Error(err))
	}
	return false
}

// ErrUSSDNoPeerTID: the peer has not answered the dialogue yet, so nothing
// can be sent in it; end it with tcap.Handler.EndLocally.
var ErrUSSDNoPeerTID = errors.New("ussd: the dialogue has no peer transaction ID yet")

// USSDInvoke sends unstructuredSS-Request (60) or -Notify (61) in an open
// USSD dialogue, in a CONTINUE.
func (h *Handler) USSDInvoke(d *tcap.Dialogue, invokeID int32, op OperationCode, arg *USSDArg) error {
	if op != OpUnstructuredSSRequest && op != OpUnstructuredSSNotify {
		return fmt.Errorf("ussd: op %d is not unstructuredSS-Request or -Notify", op)
	}
	param, err := EncodeUSSDArg(arg)
	if err != nil {
		return err
	}
	code := int32(op)
	return h.sendSession(d, &tcap.Message{
		MessageType:    tcap.MsgTypeContinue,
		OriginatingTID: binary.BigEndian.AppendUint32(nil, d.LocalTID),
		DestinationTID: d.PeerTID(),
		Dialogue:       d,
		Components: []*tcap.Component{{
			Type: tcap.CompTypeInvoke, InvokeID: &invokeID, OperationCode: &code, Parameter: param,
		}},
	})
}

// USSDRelease ends a subscriber-initiated session: processUnstructuredSS-
// Request's ReturnResult with the final USSD-Res, in an END.
func (h *Handler) USSDRelease(d *tcap.Dialogue, invokeID int32, dcs byte, str []byte) error {
	param, err := EncodeUSSDRes(dcs, str)
	if err != nil {
		return err
	}
	op := int32(OpProcessUnstructuredSSReq)
	return h.sendSession(d, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: &invokeID, OperationCode: &op, Parameter: param}},
	})
}

// USSDRejectRequest ends a subscriber-initiated session with a ReturnError
// of processUnstructuredSS-Request.
func (h *Handler) USSDRejectRequest(d *tcap.Dialogue, invokeID int32, code ErrorCode) error {
	c := int32(code)
	return h.sendSession(d, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnError, InvokeID: &invokeID, ErrorCode: &c}},
	})
}

// USSDClose ends a USSD dialogue with an END without components (a
// network-initiated session's release).
func (h *Handler) USSDClose(d *tcap.Dialogue) error {
	return h.sendSession(d, &tcap.Message{MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d})
}

// USSDAbort aborts a USSD dialogue (TC-U-ABORT): the gateway gives a
// session up, at its timeout or on shutdown.
func (h *Handler) USSDAbort(d *tcap.Dialogue) error {
	return h.sendSession(d, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.PeerTID(), Dialogue: d})
}

// sendSession sends msg in a session dialogue (USSD, location update); its
// DestinationTID was read with PeerTID.
func (h *Handler) sendSession(d *tcap.Dialogue, msg *tcap.Message) error {
	if d == nil || len(msg.DestinationTID) == 0 {
		return ErrUSSDNoPeerTID
	}
	if h.tcapOutbound == nil {
		return errors.New("map: no TCAP outbound channel")
	}
	select {
	case h.tcapOutbound <- msg:
		h.metrics.OperationsSent.Add(1)
		return nil
	default:
		h.metrics.Errors.Add(1)
		h.reportNotSent(d, "not sent: MAP outbound channel full")
		return errors.New("map: TCAP outbound channel full")
	}
}
