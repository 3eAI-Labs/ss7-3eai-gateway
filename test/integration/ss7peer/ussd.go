// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
)

// USSD: the peer plays the HLR toward the gateway as gsmSCF, under
// networkUnstructuredSsContext-v2 (TS 29.002 §11.9–11.11, §17.4).
//
//   - The gateway starts a session (unstructuredSS-Request or -Notify in a
//     BEGIN with a MAP-OpenInfo): OnUSSD answers each invoke, and the
//     dialogue stays open until the gateway ends it or an answer does.
//   - StartUSSD starts one as the subscriber's HLR does:
//     processUnstructuredSS-Request in a BEGIN with a MAP-OpenInfo naming
//     the subscriber. OnUSSD answers the gateway's invokes in it; the
//     gateway's END carries the session's outcome.
//
// Either way OnUSSDEnd is told how the gateway ended the dialogue. The peer
// checks what a real HLR relies on: the context and its version, the
// subsystems, the MAP-OpenInfo, the transaction IDs, the AARE of the first
// answer, and that the gateway does not reuse one of its invoke IDs while
// it is outstanding.

// USSDInvoke is an unstructuredSS-Request or -Notify from the gateway.
type USSDInvoke struct {
	Session  uint32 // the peer's TID of the dialogue
	Op       mapprotocol.OperationCode
	InvokeID int32
	Arg      *mapprotocol.USSDArg
	// Open is the gateway's MAP-OpenInfo, with the BEGIN that opened a
	// network-initiated session (nil after).
	Open *mapprotocol.MAPOpenInfo
}

// USSDReply is how the peer answers an invoke.
type USSDReply struct {
	// Result is a request's USSD-Res; nil sends a result without one.
	Result *mapprotocol.USSDArg
	// Error, when non-zero, answers with a ReturnError instead.
	Error mapprotocol.ErrorCode
	End   bool // the answer goes in an END: the HLR ends the dialogue
	Abort bool // a U-ABORT instead of an answer (the subscriber cancelled)
	Drop  bool // no answer
}

// USSDCallback answers the gateway's USSD invokes.
type USSDCallback func(*USSDInvoke) USSDReply

// USSDEnd is how the gateway ended a USSD dialogue.
type USSDEnd struct {
	Session uint32
	Aborted bool // an ABORT; otherwise an END
	// Result and Error: processUnstructuredSS-Request's outcome in the END
	// of a subscriber-initiated session (both empty: the END answered it
	// with nothing).
	Result *mapprotocol.USSDArg
	Error  mapprotocol.ErrorCode
}

type ussdDialogue struct {
	mu      sync.Mutex
	peerTID uint32
	gwTID   []byte // nil until the gateway's first answer to a StartUSSD
	mo      bool   // the peer started it
	begin   int32  // mo: processUnstructuredSS-Request's invoke ID
	conn    *wire.Conn
	pd      *wire.ProtocolData // routing label of a message from the gateway
	sccpReq *sccp.SccpMessage  // addresses of a message from the gateway
	// aarePending: the gateway's BEGIN carried an AARQ the peer's first
	// answer must accept.
	aarePending *tcap.DialoguePortion
	open        *mapprotocol.MAPOpenInfo
	outstanding map[int32]mapprotocol.OperationCode // the gateway's invokes not yet answered
}

var acUSSDv2 = acOID(19, 2)

const (
	// USSDHLRNumber is the GT the peer uses as the subscriber's HLR.
	USSDHLRNumber = "905320000009"
)

// USSDDialogues counts the USSD dialogues the peer took part in.
func (p *Peer) USSDDialogues() uint64 { return p.ussdCount.Load() }

// handleUSSD takes the messages of USSD dialogues: a BEGIN under the USSD
// context, and anything addressed to a USSD dialogue the peer holds.
func (p *Peer) handleUSSD(conn *wire.Conn, pd *wire.ProtocolData, sccpMsg *sccp.SccpMessage, msg *tcap.Message) bool {
	switch msg.MessageType {
	case tcap.MsgTypeBegin:
		ac := acValueOf(msg)
		if len(ac) < 2 || !bytes.Equal(ac[:len(ac)-1], acUSSDv2[:len(acUSSDv2)-1]) {
			return false
		}
		p.ussdBegin(conn, pd, sccpMsg, msg, ac)
		return true
	case tcap.MsgTypeContinue, tcap.MsgTypeEnd, tcap.MsgTypeAbort:
		v, ok := p.ussdDialogues.Load(bytesToUint32(msg.DestinationTID))
		if !ok {
			return false
		}
		p.ussdContinue(v.(*ussdDialogue), conn, pd, sccpMsg, msg)
		return true
	}
	return false
}

// ussdBegin: the gateway starts a session.
func (p *Peer) ussdBegin(conn *wire.Conn, pd *wire.ProtocolData, sccpMsg *sccp.SccpMessage, msg *tcap.Message, ac []byte) {
	p.ussdCount.Add(1)
	if !bytes.Equal(ac, acUSSDv2) {
		p.violate("USSD BEGIN under % X, want networkUnstructuredSsContext-v2 % X", ac, acUSSDv2)
		p.sendACNotSupported(conn, pd, sccpMsg, msg.OriginatingTID, acUSSDv2)
		return
	}
	if uint8(sccpMsg.CalledAddress.SubsystemNumber) != ssnHLR || uint8(sccpMsg.CallingAddress.SubsystemNumber) != ssnGSMSCF {
		p.violate("USSD BEGIN from SSN %d to SSN %d, want gsmSCF (%d) to HLR (%d)",
			sccpMsg.CallingAddress.SubsystemNumber, sccpMsg.CalledAddress.SubsystemNumber, ssnGSMSCF, ssnHLR)
	}
	open := p.checkOpenInfo(msg.DialoguePortion)
	d := &ussdDialogue{
		peerTID: p.originateTID.Add(1), gwTID: msg.OriginatingTID, conn: conn, pd: pd, sccpReq: sccpMsg,
		aarePending: aareFor(msg), open: open, outstanding: map[int32]mapprotocol.OperationCode{},
	}
	p.ussdDialogues.Store(d.peerTID, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	p.ussdComponents(d, msg)
}

// checkOpenInfo: a gsmSCF opening a USSD dialogue names the subscriber —
// IMSI (E.212) or MSISDN (E.164) — and itself (TS 29.002 §7.3, tables
// 7.3/2 and 7.3/3).
func (p *Peer) checkOpenInfo(dp *tcap.DialoguePortion) *mapprotocol.MAPOpenInfo {
	if dp == nil || len(dp.UserInformation) == 0 {
		p.violate("USSD BEGIN without MAP-OpenInfo")
		return nil
	}
	open, ok, err := mapprotocol.DecodeMAPOpenInfo(dp.UserInformation)
	if err != nil || !ok {
		p.violate("USSD BEGIN user-information is no MAP-OpenInfo (%v)", err)
		return nil
	}
	if r := open.DestinationReference; r == nil ||
		(r.NumberPlan != mapprotocol.NumberingPlanLandMobile && r.NumberPlan != mapprotocol.NumberingPlanISDN) {
		p.violate("MAP-OpenInfo destinationReference %+v is not an IMSI or MSISDN", r)
	}
	if r := open.OriginationReference; r == nil || r.NumberPlan != mapprotocol.NumberingPlanISDN || r.Digits == "" {
		p.violate("MAP-OpenInfo originationReference %+v is not the gsmSCF's ISDN address", r)
	}
	return open
}

// ussdContinue: a message from the gateway in a USSD dialogue the peer
// holds.
func (p *Peer) ussdContinue(d *ussdDialogue, conn *wire.Conn, pd *wire.ProtocolData, sccpMsg *sccp.SccpMessage, msg *tcap.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conn, d.pd, d.sccpReq = conn, pd, sccpMsg
	if msg.MessageType == tcap.MsgTypeContinue {
		switch {
		case d.gwTID == nil:
			// The gateway's first answer to the peer's BEGIN: its TID, and the
			// AARE accepting the context.
			d.gwTID = msg.OriginatingTID
			if dp := msg.DialoguePortion; dp == nil || dp.ApplicationContext == nil || dp.Rejected ||
				!bytes.Equal(acValueOf(msg), acUSSDv2) {
				p.violate("the gateway's first USSD answer does not accept the context with an AARE")
			}
		case !bytes.Equal(msg.OriginatingTID, d.gwTID):
			p.violate("USSD CONTINUE from TID % X, the dialogue's is % X", msg.OriginatingTID, d.gwTID)
		case msg.DialoguePortion != nil:
			p.violate("USSD CONTINUE carries a dialogue portion after the dialogue was established")
		}
	}
	if d.mo && d.gwTID == nil && msg.MessageType == tcap.MsgTypeEnd && msg.DialoguePortion == nil {
		p.violate("the gateway ends the peer's USSD dialogue without an AARE")
	}
	if msg.MessageType == tcap.MsgTypeAbort {
		p.ussdEnded(d, USSDEnd{Session: d.peerTID, Aborted: true})
		return
	}
	end := p.ussdComponents(d, msg)
	if msg.MessageType == tcap.MsgTypeEnd {
		p.ussdEnded(d, end)
	}
}

// ussdComponents answers the gateway's invokes and reads the outcome of
// the peer's processUnstructuredSS-Request.
func (p *Peer) ussdComponents(d *ussdDialogue, msg *tcap.Message) USSDEnd {
	end := USSDEnd{Session: d.peerTID}
	for _, c := range msg.Components {
		if c.InvokeID == nil {
			p.violate("USSD component without an invoke ID")
			continue
		}
		id := *c.InvokeID
		switch c.Type {
		case tcap.CompTypeInvoke:
			if msg.MessageType == tcap.MsgTypeEnd {
				p.violate("USSD invoke %d in an END: nothing can answer it", id)
				continue
			}
			p.ussdInvoke(d, msg, c)
		case tcap.CompTypeReturnResult:
			if !d.mo || id != d.begin {
				p.violate("USSD result for invoke %d, which the peer did not send", id)
				continue
			}
			if c.OperationCode == nil || *c.OperationCode != int32(mapprotocol.OpProcessUnstructuredSSReq) {
				p.violate("processUnstructuredSS-Request's result without its operation code")
			}
			res, err := mapprotocol.DecodeUSSDArg(c.Parameter)
			if err != nil {
				p.violate("processUnstructuredSS-Request's result: %v", err)
				continue
			}
			end.Result = res
		case tcap.CompTypeReturnError:
			if !d.mo || id != d.begin || c.ErrorCode == nil {
				p.violate("USSD error for invoke %d, which the peer did not send", id)
				continue
			}
			code := mapprotocol.ErrorCode(*c.ErrorCode)
			switch code {
			case mapprotocol.ErrSystemFailure, mapprotocol.ErrDataMissing, mapprotocol.ErrUnexpectedDataValue,
				mapprotocol.ErrUnknownAlphabet, mapprotocol.ErrCallBarred:
			default:
				p.violate("processUnstructuredSS-Request answered with %s, not one of its errors (TS 29.002 §11.9.3)", code)
			}
			end.Error = code
		default:
			p.violate("USSD %v component from the gateway", c.Type)
		}
	}
	return end
}

func (p *Peer) ussdInvoke(d *ussdDialogue, msg *tcap.Message, c *tcap.Component) {
	id := *c.InvokeID
	op := mapprotocol.OperationCode(-1)
	if c.OperationCode != nil {
		op = mapprotocol.OperationCode(*c.OperationCode)
	}
	if op != mapprotocol.OpUnstructuredSSRequest && op != mapprotocol.OpUnstructuredSSNotify {
		p.violate("USSD invoke of op %d from the gsmSCF", op)
		return
	}
	// Each end numbers its own invokes (Q.773): only the gateway's own
	// outstanding IDs are off limits, not the peer's.
	if _, busy := d.outstanding[id]; busy {
		p.violate("USSD invoke ID %d reused while outstanding", id)
	}
	arg, err := mapprotocol.DecodeUSSDArg(c.Parameter)
	if err != nil {
		p.violate("USSD-Arg: %v", err)
		return
	}
	if arg.MSISDN != nil {
		p.violate("USSD-Arg from the gsmSCF carries an msisdn (TS 29.002 §11.10.2)")
	}
	inv := &USSDInvoke{Session: d.peerTID, Op: op, InvokeID: id, Arg: arg}
	if msg.MessageType == tcap.MsgTypeBegin {
		inv.Open = d.open
	}
	reply := USSDReply{Drop: true}
	if p.cfg.OnUSSD != nil {
		reply = p.cfg.OnUSSD(inv)
	}
	if reply.Drop {
		d.outstanding[id] = op
		return
	}
	p.answerUSSDLocked(d, id, op, reply)
}

// answerUSSDLocked sends the peer's answer to the gateway's invoke id (of
// op). d.mu is held.
func (p *Peer) answerUSSDLocked(d *ussdDialogue, id int32, op mapprotocol.OperationCode, reply USSDReply) {
	if reply.Abort {
		p.sendUSSD(d, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.gwTID})
		p.ussdDialogues.Delete(d.peerTID)
		return
	}
	comp := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: i32(id)}
	switch {
	case reply.Error != 0:
		comp = &tcap.Component{Type: tcap.CompTypeReturnError, InvokeID: i32(id), ErrorCode: i32(int32(reply.Error))}
	case op == mapprotocol.OpUnstructuredSSRequest && reply.Result != nil:
		res, err := mapprotocol.EncodeUSSDRes(reply.Result.DCS, reply.Result.String)
		if err != nil {
			p.violate("peer USSD-Res: %v", err)
			return
		}
		comp.OperationCode, comp.Parameter = i32(int32(op)), res
	}
	answer := &tcap.Message{MessageType: tcap.MsgTypeContinue, OriginatingTID: tidBytes(d.peerTID), DestinationTID: d.gwTID,
		DialoguePortion: d.aarePending, Components: []*tcap.Component{comp}}
	if reply.End {
		answer.MessageType, answer.OriginatingTID = tcap.MsgTypeEnd, nil
		p.ussdDialogues.Delete(d.peerTID)
	}
	d.aarePending = nil
	p.sendUSSD(d, answer)
}

func (p *Peer) ussdEnded(d *ussdDialogue, end USSDEnd) {
	p.ussdDialogues.Delete(d.peerTID)
	if p.cfg.OnUSSDEnd != nil {
		p.cfg.OnUSSDEnd(end)
	}
}

func (p *Peer) sendUSSD(d *ussdDialogue, msg *tcap.Message) {
	p.sendTCAP(d.conn, d.pd, d.sccpReq, msg)
}

// StartUSSD starts a subscriber-initiated session as the subscriber's HLR:
// processUnstructuredSS-Request with what they dialled, to the gateway's
// gsmSCF (SSN 147) at gatewayGT, with a MAP-OpenInfo naming the subscriber
// by IMSI and the HLR. msisdn, when set, goes in the USSD-Arg. It returns
// the session (the peer's TID).
func (p *Peer) StartUSSD(dialled, imsi, msisdn, gatewayGT string) (uint32, error) {
	conns := p.AcceptedConns()
	if len(conns) == 0 {
		return 0, errors.New("ss7peer: no association")
	}
	dcs, str, err := mapprotocol.EncodeUSSDText(dialled)
	if err != nil {
		return 0, err
	}
	arg := &mapprotocol.USSDArg{DCS: dcs, String: str}
	if msisdn != "" {
		arg.MSISDN = &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: msisdn}
	}
	param, err := mapprotocol.EncodeUSSDArg(arg)
	if err != nil {
		return 0, err
	}
	open, err := mapprotocol.EncodeMAPOpenInfo(&mapprotocol.MAPOpenInfo{
		DestinationReference: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanLandMobile, Digits: imsi},
		OriginationReference: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: USSDHLRNumber},
	})
	if err != nil {
		return 0, err
	}
	gt := func(digits string) *sccp.GlobalTitle {
		return &sccp.GlobalTitle{GTI: sccp.GTI0100, NumberingPlan: sccp.NPISDN, EncodingScheme: sccp.ESBCD,
			NAI: sccp.NAIInternational, Address: digits}
	}
	ai := sccp.AISSNPresent | sccp.AIPCPresent | sccp.AIRouteOnSSN
	// The addresses of a message "from the gateway", so that the replies
	// sendTCAP builds by swapping them go to the gateway.
	d := &ussdDialogue{
		peerTID: p.originateTID.Add(1), mo: true, begin: 1, conn: conns[0],
		pd: &wire.ProtocolData{OPC: p.cfg.GatewayPointCode, SI: siSCCP, NI: p.cfg.NetworkIndicator, SLS: 5},
		sccpReq: &sccp.SccpMessage{
			ProtocolClass:  sccp.ClassBasicConnectionless,
			CallingAddress: &sccp.SccpAddress{AddressIndicator: ai, PointCode: p.cfg.GatewayPointCode, SubsystemNumber: ssnGSMSCF, GlobalTitle: gt(gatewayGT)},
			CalledAddress:  &sccp.SccpAddress{AddressIndicator: ai, PointCode: p.cfg.PointCode, SubsystemNumber: ssnHLR, GlobalTitle: gt(USSDHLRNumber)},
		},
		outstanding: map[int32]mapprotocol.OperationCode{},
	}
	p.ussdCount.Add(1)
	p.ussdDialogues.Store(d.peerTID, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	op := int32(mapprotocol.OpProcessUnstructuredSSReq)
	p.sendUSSD(d, &tcap.Message{
		MessageType: tcap.MsgTypeBegin, OriginatingTID: tidBytes(d.peerTID),
		DialoguePortion: &tcap.DialoguePortion{ApplicationContext: &tcap.ApplicationContext{OID: acOIDOf(acUSSDv2)}, UserInformation: open},
		Components:      []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32(d.begin), OperationCode: &op, Parameter: param}},
	})
	return d.peerTID, nil
}

// AnswerUSSD answers an invoke the peer's OnUSSD dropped, later — as a
// subscriber who takes their time.
func (p *Peer) AnswerUSSD(session uint32, invokeID int32, reply USSDReply) error {
	v, ok := p.ussdDialogues.Load(session)
	if !ok {
		return fmt.Errorf("ss7peer: no USSD session %d", session)
	}
	d := v.(*ussdDialogue)
	d.mu.Lock()
	defer d.mu.Unlock()
	op, ok := d.outstanding[invokeID]
	if !ok {
		return fmt.Errorf("ss7peer: USSD session %d has no invoke %d outstanding", session, invokeID)
	}
	delete(d.outstanding, invokeID)
	p.answerUSSDLocked(d, invokeID, op, reply)
	return nil
}
