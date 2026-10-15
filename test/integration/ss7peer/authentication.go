// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
)

// Authentication and MS purging: the peer plays the VLR toward the gateway
// as HLR (TS 29.002 §8.5.2, §8.1.6). SendAuthenticationInfo and PurgeMS
// send the request in a BEGIN, from the VLR (SSN 7) to the HLR by the
// E.214 global title of the IMSI (SSN 6), and return how the gateway
// answered.
//
// The peer checks what a VLR relies on: the context and its version in the
// AARE, the HLR's subsystem, the gateway's TID; for sendAuthenticationInfo
// that each part of the result is a ReturnResultLast of the request being
// answered (TS 29.002 Annex C.3.3: TC-Result-NL and the empty Continue are
// not allowed for it), with the operation code, decoding strictly, no more
// vectors than asked for in all, no part in a CONTINUE when segmentation
// is prohibited; that errors are the operation's own; that purgeMS is
// answered in an END.

// HLRRequestOutcome is how the gateway answered a request.
type HLRRequestOutcome struct {
	// Parts: sendAuthenticationInfo's result, part by part.
	Parts []*mapprotocol.SendAuthenticationInfoRes
	Purge *mapprotocol.PurgeMSRes
	Error mapprotocol.ErrorCode
	// Reject: the problem code of a Reject of the request.
	Reject  *uint8
	Aborted bool
	// GatewayAddresses are the calling GTs of the gateway's messages.
	GatewayAddresses []string
}

type hrDialogue struct {
	mu      sync.Mutex
	peerTID uint32
	gwTID   []byte
	invoke  int32
	ac      []byte
	op      mapprotocol.OperationCode
	name    string // the operation's, for violations
	arg     *mapprotocol.SendAuthenticationInfoArg
	// stopAfter: the VLR ends the dialogue after this many parts instead
	// of asking for the next (0: it asks for all).
	stopAfter int
	vectors   int
	sccpReq   *sccp.SccpMessage
	pd        *wire.ProtocolData
	out       HLRRequestOutcome
	done      chan HLRRequestOutcome
}

var (
	acInfoRetrievalV3 = acOID(14, 3)
	acMSPurgingV3     = acOID(27, 3)
)

// SendAuthenticationInfo asks the HLR of arg.IMSI for vectors as a VLR
// with the global title vlr does, and waits for the answer. With stopAfter
// > 0 the VLR ends the dialogue after that many parts of the result
// instead of asking for the next.
func (p *Peer) SendAuthenticationInfo(ctx context.Context, vlr string, arg *mapprotocol.SendAuthenticationInfoArg, stopAfter int) (HLRRequestOutcome, error) {
	param, err := mapprotocol.EncodeSendAuthenticationInfoArg(arg)
	if err != nil {
		return HLRRequestOutcome{}, err
	}
	d := &hrDialogue{ac: acInfoRetrievalV3, op: mapprotocol.OpSendAuthenticationInfo, name: "sendAuthenticationInfo",
		arg: arg, stopAfter: stopAfter}
	return p.hlrRequest(ctx, string(arg.IMSI), vlr, d, param)
}

// PurgeMS tells the HLR of arg.IMSI that the VLR with the global title vlr
// deleted the subscriber's record, and waits for the answer.
func (p *Peer) PurgeMS(ctx context.Context, vlr string, arg *mapprotocol.PurgeMSArg) (HLRRequestOutcome, error) {
	param, err := mapprotocol.EncodePurgeMSArg(arg)
	if err != nil {
		return HLRRequestOutcome{}, err
	}
	return p.hlrRequest(ctx, string(arg.IMSI), vlr, &hrDialogue{ac: acMSPurgingV3, op: mapprotocol.OpPurgeMS, name: "purgeMS"}, param)
}

func (p *Peer) hlrRequest(ctx context.Context, imsi, vlr string, d *hrDialogue, param []byte) (HLRRequestOutcome, error) {
	gt := func(digits string, np sccp.NumberingPlan) *sccp.GlobalTitle {
		return &sccp.GlobalTitle{GTI: sccp.GTI0100, NumberingPlan: np, EncodingScheme: sccp.ESBCD,
			NAI: sccp.NAIInternational, Address: digits}
	}
	d.peerTID, d.invoke, d.done = p.originateTID.Add(1), 1, make(chan HLRRequestOutcome, 1)
	d.pd = &wire.ProtocolData{OPC: p.cfg.GatewayPointCode, SI: siSCCP, NI: p.cfg.NetworkIndicator, SLS: 9}
	// As if from the gateway, so that sendTCAP's swap addresses it.
	d.sccpReq = &sccp.SccpMessage{
		ProtocolClass: sccp.ClassBasicConnectionless,
		CallingAddress: &sccp.SccpAddress{AddressIndicator: sccp.AISSNPresent, SubsystemNumber: ssnHLR,
			GlobalTitle: gt(MGT(imsi), sccp.NPISDNMobile)},
		CalledAddress: &sccp.SccpAddress{AddressIndicator: sccp.AISSNPresent, SubsystemNumber: ssnVLR,
			GlobalTitle: gt(vlr, sccp.NPISDN)},
	}
	conn := p.connFor(d.sccpReq.CallingAddress)
	if conn == nil {
		return HLRRequestOutcome{}, errors.New("ss7peer: no association")
	}
	p.mobility.hlrRequests.Store(d.peerTID, d)
	defer p.mobility.hlrRequests.Delete(d.peerTID)
	op := int32(d.op)
	d.mu.Lock()
	p.sendTCAP(conn, d.pd, d.sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeBegin, OriginatingTID: tidBytes(d.peerTID),
		DialoguePortion: &tcap.DialoguePortion{ApplicationContext: &tcap.ApplicationContext{OID: acOIDOf(d.ac)}},
		Components:      []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32(d.invoke), OperationCode: &op, Parameter: param}},
	})
	d.mu.Unlock()
	select {
	case out := <-d.done:
		return out, nil
	case <-ctx.Done():
		return HLRRequestOutcome{}, ctx.Err()
	}
}

// handleHLRRequest takes the gateway's messages in a request the peer
// sent.
func (p *Peer) handleHLRRequest(conn *wire.Conn, pd *wire.ProtocolData, sccpMsg *sccp.SccpMessage, msg *tcap.Message) bool {
	if msg.MessageType != tcap.MsgTypeContinue && msg.MessageType != tcap.MsgTypeEnd && msg.MessageType != tcap.MsgTypeAbort {
		return false
	}
	v, ok := p.mobility.hlrRequests.Load(bytesToUint32(msg.DestinationTID))
	if !ok {
		return false
	}
	d := v.(*hrDialogue)
	d.mu.Lock()
	defer d.mu.Unlock()
	if uint8(sccpMsg.CallingAddress.SubsystemNumber) != ssnHLR {
		p.violate("%s: the gateway answers from SSN %d, want the HLR's (%d)", d.name, sccpMsg.CallingAddress.SubsystemNumber, ssnHLR)
	}
	d.pd, d.sccpReq = pd, sccpMsg
	d.out.GatewayAddresses = append(d.out.GatewayAddresses, gtDigits(sccpMsg.CallingAddress))
	first := d.gwTID == nil
	if msg.MessageType == tcap.MsgTypeAbort {
		d.out.Aborted = true
		d.finish()
		return true
	}
	if first {
		if dp := msg.DialoguePortion; dp == nil || dp.Rejected || !bytes.Equal(acValueOf(msg), d.ac) {
			p.violate("%s: the gateway's first answer does not accept the context with an AARE", d.name)
		}
	} else if msg.DialoguePortion != nil {
		p.violate("%s: a dialogue portion after the dialogue was established", d.name)
	}
	if msg.MessageType == tcap.MsgTypeContinue {
		switch {
		case d.op == mapprotocol.OpPurgeMS:
			p.violate("purgeMS answered in a CONTINUE")
		case first:
			d.gwTID = msg.OriginatingTID
		case !bytes.Equal(msg.OriginatingTID, d.gwTID):
			p.violate("%s CONTINUE from TID % X, the dialogue's is % X", d.name, msg.OriginatingTID, d.gwTID)
		}
	}
	parts := len(d.out.Parts)
	for _, c := range msg.Components {
		p.hrComponent(d, msg, c)
	}
	if msg.MessageType == tcap.MsgTypeEnd {
		d.finish()
		return true
	}
	if d.op == mapprotocol.OpSendAuthenticationInfo && len(d.out.Parts) == parts && d.out.Reject == nil {
		p.violate("sendAuthenticationInfo: a CONTINUE without a part of the result (an empty Continue is not allowed for it)")
	}
	// A part with more to come: ask for the next, or end, as the VLR. The
	// message goes to the gateway's calling address, routed as the SG does.
	if d.stopAfter > 0 && len(d.out.Parts) >= d.stopAfter {
		p.sendTCAP(p.connFor(sccpMsg.CallingAddress), d.pd, d.sccpReq, &tcap.Message{MessageType: tcap.MsgTypeEnd, DestinationTID: d.gwTID})
		d.finish()
		return true
	}
	d.invoke++
	op := int32(mapprotocol.OpSendAuthenticationInfo)
	p.sendTCAP(p.connFor(sccpMsg.CallingAddress), d.pd, d.sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeContinue, OriginatingTID: tidBytes(d.peerTID), DestinationTID: d.gwTID,
		Components: []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32(d.invoke), OperationCode: &op}},
	})
	return true
}

func (p *Peer) hrComponent(d *hrDialogue, msg *tcap.Message, c *tcap.Component) {
	if c.InvokeID == nil || *c.InvokeID != d.invoke {
		p.violate("%s: a %v component for invoke %v, the request outstanding is %d", d.name, c.Type, c.InvokeID, d.invoke)
		return
	}
	switch c.Type {
	case tcap.CompTypeReturnResult:
		if c.NotLast {
			p.violate("%s: a Return Result Not Last (TS 29.002 Annex C: not allowed)", d.name)
		}
		if c.OperationCode == nil || mapprotocol.OperationCode(*c.OperationCode) != d.op {
			p.violate("%s: its result without its operation code", d.name)
		}
		if d.op == mapprotocol.OpPurgeMS {
			res, err := mapprotocol.DecodePurgeMSRes(c.Parameter)
			if err != nil {
				p.violate("PurgeMS-Res: %v", err)
				return
			}
			d.out.Purge = res
			return
		}
		res, err := mapprotocol.DecodeSendAuthenticationInfoRes(c.Parameter)
		if err != nil {
			p.violate("SendAuthenticationInfoRes: %v", err)
			return
		}
		if msg.MessageType == tcap.MsgTypeContinue && d.arg.SegmentationProhibited {
			p.violate("sendAuthenticationInfo: a part in a CONTINUE, segmentation prohibited")
		}
		if res.UEUsageType != nil && len(d.out.Parts) > 0 {
			p.violate("sendAuthenticationInfo: the UE usage type in a later part")
		}
		d.vectors += len(res.Triplets) + len(res.Quintuplets) + len(res.EPSVectors)
		if max := d.arg.NumberOfRequestedVectors + d.arg.AdditionalVectors; d.vectors > max {
			p.violate("sendAuthenticationInfo: %d vectors, %d asked for", d.vectors, max)
		}
		d.out.Parts = append(d.out.Parts, res)
	case tcap.CompTypeReturnError:
		if c.ErrorCode == nil {
			p.violate("%s: a ReturnError without a code", d.name)
			return
		}
		code := mapprotocol.ErrorCode(*c.ErrorCode)
		allowed := mapprotocol.PurgeMSErrors
		if d.op == mapprotocol.OpSendAuthenticationInfo {
			allowed = mapprotocol.SendAuthenticationInfoErrors
		}
		if !allowed[code] {
			p.violate("%s answered with %s, not one of its errors", d.name, code)
		}
		d.out.Error = code
	case tcap.CompTypeReject:
		if c.ProblemCode != nil {
			d.out.Reject = &c.ProblemCode.Code
		}
	default:
		p.violate("%s: a %v component from the gateway", d.name, c.Type)
	}
}

// finish reports the outcome, once.
func (d *hrDialogue) finish() {
	select {
	case d.done <- d.out:
	default:
	}
}
