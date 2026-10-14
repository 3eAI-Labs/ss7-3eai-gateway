// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/wmnsk/go-m3ua"
	"github.com/wmnsk/go-m3ua/messages/params"
	"go.uber.org/zap"
)

// RoutingCallback answers a SendRoutingInfo the way an HLR would. One
// result is sent as an END ReturnResult (Last). Several are sent the way an
// HLR segments a long result (TS 29.002 §C.2.3): each but the last as a
// Return Result Not Last in its own CONTINUE — the first with the AARE —
// and the last as a Return Result Last in the END. An error becomes an END
// ReturnError, as for SRICallback.
type RoutingCallback func(
	req *mapprotocol.SendRoutingInfoRequest,
	calledGT, callingGT string,
	otid uint32,
) ([]*mapprotocol.SendRoutingInfoResponse, error)

// RoutingRequests counts the SendRoutingInfo invokes the peer received.
func (p *Peer) RoutingRequests() uint64 { return p.routingRequests.Load() }

func (p *Peer) handleRoutingInvoke(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
	paramBytes []byte,
	otid uint32,
) {
	p.routingRequests.Add(1)
	if p.cfg.OnRouting == nil {
		return
	}
	req, err := mapprotocol.DecodeSendRoutingInfoArg(paramBytes)
	if err != nil {
		p.violate("SendRoutingInfoArg undecodable: %v", err)
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}
	segments, cbErr := p.cfg.OnRouting(req, gtDigits(sccpReq.CalledAddress), gtDigits(sccpReq.CallingAddress), otid)
	if cbErr != nil || len(segments) == 0 {
		p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, returnErrorCode(cbErr))
		return
	}
	encoded := make([][]byte, len(segments))
	for i, s := range segments {
		if encoded[i], err = mapprotocol.EncodeSendRoutingInfoRes(s); err != nil {
			p.logger.Warn("SendRoutingInfoRes encode failed", zap.Error(err))
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
	}
	op := int32(mapprotocol.OpSendRoutingInfo)
	if len(encoded) == 1 {
		p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, op, encoded[0])
		return
	}
	// Each segment but the last as a Return Result Not Last in its own
	// CONTINUE, the first with the AARE; the last as a Return Result Last
	// in the END.
	peerTID := tidBytes(p.originateTID.Add(1))
	for i, seg := range encoded[:len(encoded)-1] {
		msg := &tcap.Message{
			MessageType: tcap.MsgTypeContinue, OriginatingTID: peerTID, DestinationTID: otidBytes,
			Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, NotLast: true,
				InvokeID: i32(invokeID), OperationCode: i32(op), Parameter: seg}},
		}
		if i == 0 {
			msg.DialoguePortion = aareFor(tcapReq)
		}
		p.sendTCAP(conn, pd, sccpReq, msg)
	}
	p.sendTCAP(conn, pd, sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: otidBytes,
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult,
			InvokeID: i32(invokeID), OperationCode: i32(op), Parameter: encoded[len(encoded)-1]}},
	})
}
