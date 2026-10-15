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

// Location updating dialogues (networkLocUpContext-v3), with the gateway as
// HLR. The MAP process in the HLR (TS 29.002 §19.1.1.5, figure 19.1.1/13)
// relays between the VLR and the HLR's location management application:
// the application decides what to send — insertSubscriberData, as many as
// it needs, then the updateLocation result or error — so the dialogue's
// components are handed to a LocationUpdateHandler, which answers through
// the senders below.

// LocationUpdateHandler is the gateway side of location updating
// dialogues. Its methods run on a TCAP worker and must not block.
type LocationUpdateHandler interface {
	// UpdateLocation: a VLR opened a dialogue with updateLocation (2). It
	// reports false when the dialogue already has a location update, and
	// the Invoke is rejected.
	UpdateLocation(d *tcap.Dialogue, invokeID int32, arg *UpdateLocationArg) bool
	// InsertSubscriberDataResult: the ReturnResult of an
	// insertSubscriberData the gateway sent. ended: the message carrying it
	// ends the dialogue, and LocationUpdateEnded follows.
	InsertSubscriberDataResult(d *tcap.Dialogue, invokeID int32, res *InsertSubscriberDataRes, ended bool)
	// InsertSubscriberDataError: its ReturnError (code) or a Reject (code
	// 0). ended: the message carrying it ends the dialogue, and
	// LocationUpdateEnded follows.
	InsertSubscriberDataError(d *tcap.Dialogue, invokeID int32, code ErrorCode, detail string, ended bool)
	// LocationUpdateEnded: the VLR ended the dialogue — END (aborted false)
	// or ABORT — after the components the message carried.
	LocationUpdateEnded(d *tcap.Dialogue, aborted bool, detail string)
	// LocationUpdateActive reports whether the dialogue has a location
	// update: an Invoke refused in it is then rejected in a CONTINUE.
	LocationUpdateActive(d *tcap.Dialogue) bool
}

// SetLocationUpdateHandler installs the location update handler. Set
// before traffic flows.
func (h *Handler) SetLocationUpdateHandler(l LocationUpdateHandler) { h.locationUpdate = l }

func isLocationUpdateDialogue(d *tcap.Dialogue) bool {
	return d != nil && d.ApplicationContext != nil && slices.Equal(d.ApplicationContext.OID, tcap.ACNetworkLocUpV3.OID)
}

// handleLocationUpdate routes a location updating dialogue's message.
func (h *Handler) handleLocationUpdate(d *tcap.Dialogue, msg *tcap.Message) {
	ended := msg.MessageType == tcap.MsgTypeEnd || msg.MessageType == tcap.MsgTypeAbort
	for _, comp := range msg.Components {
		if comp == nil || comp.InvokeID == nil {
			continue
		}
		id := *comp.InvokeID
		switch comp.Type {
		case tcap.CompTypeInvoke:
			if h.locationUpdateInvoke(d, msg, comp) {
				return // rejected in an END: the dialogue is over
			}
		case tcap.CompTypeReturnResult:
			if comp.NotLast {
				// insertSubscriberData's result is not segmented (TS 29.002
				// Annex C: TC-Result-NL is not applicable to it).
				h.locationUpdate.InsertSubscriberDataError(d, id, 0, "segmented insertSubscriberData result", ended)
				continue
			}
			res, err := DecodeInsertSubscriberDataRes(comp.Parameter)
			if err != nil {
				h.metrics.Errors.Add(1)
				h.locationUpdate.InsertSubscriberDataError(d, id, 0, "undecodable InsertSubscriberDataRes: "+err.Error(), ended)
				continue
			}
			h.locationUpdate.InsertSubscriberDataResult(d, id, res, ended)
		case tcap.CompTypeReturnError:
			code, detail := ErrorCode(0), "MAP ReturnError without a local error code"
			if comp.ErrorCode != nil {
				code = ErrorCode(*comp.ErrorCode)
				detail = code.String()
			}
			h.locationUpdate.InsertSubscriberDataError(d, id, code, detail, ended)
		case tcap.CompTypeReject:
			detail := "TCAP Reject"
			if p := comp.ProblemCode; p != nil {
				detail = fmt.Sprintf("TCAP Reject problem type=%d code=%d", p.Type, p.Code)
			}
			h.locationUpdate.InsertSubscriberDataError(d, id, 0, detail, ended)
		}
	}
	switch msg.MessageType {
	case tcap.MsgTypeEnd:
		h.locationUpdate.LocationUpdateEnded(d, false, "ended by the VLR")
	case tcap.MsgTypeAbort:
		detail := "dialogue aborted by the VLR"
		if msg.PAbort {
			detail = "dialogue aborted by the VLR's TCAP provider"
		}
		h.locationUpdate.LocationUpdateEnded(d, true, detail)
	}
}

// locationUpdateInvoke takes an Invoke in a location updating dialogue.
// Only the VLR's updateLocation opening the dialogue is served: in the
// BEGIN, or in the CONTINUE after an empty Begin (TS 29.002 §C.2.1). The
// VLR's other operations of the context (restoreData, forwardCheckSS-
// Indication) are not. It reports whether a rejection ended the dialogue.
func (h *Handler) locationUpdateInvoke(d *tcap.Dialogue, msg *tcap.Message, comp *tcap.Component) bool {
	op := OperationCode(-1)
	if comp.OperationCode != nil {
		op = OperationCode(*comp.OperationCode)
	}
	if op != OpUpdateLocation || d.InitiatedLocally() ||
		(msg.MessageType != tcap.MsgTypeBegin && msg.MessageType != tcap.MsgTypeContinue) {
		return h.locationUpdateReject(d, comp, tcap.InvokeProblemUnrecognizedOperation)
	}
	bumpPerOpCounter(h.metrics, op)
	arg, err := DecodeUpdateLocationArg(comp.Parameter)
	if err != nil {
		h.metrics.Errors.Add(1)
		h.logger.Warn("MAP updateLocation decode failed", zap.Error(err), zap.Uint32("tid", localTID(d)))
		return h.locationUpdateReject(d, comp, tcap.InvokeProblemMistypedParameter)
	}
	if !h.locationUpdate.UpdateLocation(d, *comp.InvokeID, arg) {
		return h.locationUpdateReject(d, comp, tcap.InvokeProblemUnrecognizedOperation)
	}
	return false
}

// locationUpdateReject rejects an Invoke of a location updating dialogue.
// It reports whether the dialogue ended.
func (h *Handler) locationUpdateReject(d *tcap.Dialogue, comp *tcap.Component, problem uint8) bool {
	return h.rejectInvoke(d, comp, problem, h.locationUpdate.LocationUpdateActive(d))
}

// rejectInvoke rejects an Invoke: in an END when it is all the dialogue
// has, in a CONTINUE when live, a session served in the dialogue, lives in
// it — even in the BEGIN that opened it (Q.774 §3.2.2.2: a Reject does not
// end the dialogue). It reports whether the dialogue ended.
func (h *Handler) rejectInvoke(d *tcap.Dialogue, comp *tcap.Component, problem uint8, live bool) bool {
	if !live {
		h.emitReject(d, comp, tcap.ProblemTypeInvoke, problem)
		return true
	}
	if err := h.sendSession(d, &tcap.Message{
		MessageType:    tcap.MsgTypeContinue,
		OriginatingTID: binary.BigEndian.AppendUint32(nil, d.LocalTID),
		DestinationTID: d.PeerTID(),
		Dialogue:       d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReject, InvokeID: comp.InvokeID,
			ProblemCode: &tcap.ProblemCode{Type: tcap.ProblemTypeInvoke, Code: problem}}},
	}); err != nil {
		h.logger.Warn("Reject not sent", zap.Uint32("tid", localTID(d)), zap.Error(err))
	}
	return false
}

// InsertSubscriberDataMessage is the CONTINUE carrying insertSubscriberData
// (7) with param, an encoded InsertSubscriberDataArg, in an open location
// updating dialogue. The caller asks TCAP whether it fits before sending
// it with Send.
func (h *Handler) InsertSubscriberDataMessage(d *tcap.Dialogue, invokeID int32, param []byte) *tcap.Message {
	code := int32(OpInsertSubscriberData)
	return &tcap.Message{
		MessageType:    tcap.MsgTypeContinue,
		OriginatingTID: binary.BigEndian.AppendUint32(nil, d.LocalTID),
		DestinationTID: d.PeerTID(),
		Dialogue:       d,
		Components: []*tcap.Component{{
			Type: tcap.CompTypeInvoke, InvokeID: &invokeID, OperationCode: &code, Parameter: param,
		}},
	}
}

// Send sends a message built for a session dialogue (location update).
func (h *Handler) Send(d *tcap.Dialogue, msg *tcap.Message) error { return h.sendSession(d, msg) }

// UpdateLocationAccept ends a location update: updateLocation's
// ReturnResult with the HLR number, in an END.
func (h *Handler) UpdateLocationAccept(d *tcap.Dialogue, invokeID int32, hlrNumber AddressString) error {
	param, err := EncodeUpdateLocationRes(&UpdateLocationRes{HLRNumber: hlrNumber})
	if err != nil {
		return err
	}
	op := int32(OpUpdateLocation)
	return h.sendSession(d, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: d.PeerTID(), Dialogue: d,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: &invokeID, OperationCode: &op, Parameter: param}},
	})
}

// UpdateLocationRefuse ends a location update with a ReturnError of
// updateLocation, in an END.
// roamingNotAllowed carries its cause, which its parameter must (TS
// 29.002 MAP-Errors: the parameter is not optional).
func (h *Handler) UpdateLocationRefuse(d *tcap.Dialogue, invokeID int32, e UserError) error {
	if !UpdateLocationErrors[e.Code] {
		return fmt.Errorf("location update: %s is not an error updateLocation allows", e.Code)
	}
	return h.sendUserError(d, invokeID, e)
}

// LocationUpdateAbort aborts a location updating dialogue (TC-U-ABORT).
func (h *Handler) LocationUpdateAbort(d *tcap.Dialogue) error {
	return h.sendSession(d, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.PeerTID(), Dialogue: d})
}
