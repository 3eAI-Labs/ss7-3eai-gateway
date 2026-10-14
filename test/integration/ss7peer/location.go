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

// ATICallback answers an AnyTimeInterrogation the way an HLR would: a
// response becomes an END ReturnResult, an error an END ReturnError (the
// code of a MAPError in the chain, else systemFailure). The arguments are
// those of SRICallback.
type ATICallback func(
	req *mapprotocol.AnyTimeInterrogationRequest,
	calledGT, callingGT string,
	otid uint32,
) (*mapprotocol.AnyTimeInterrogationResponse, error)

// PSICallback answers a ProvideSubscriberInfo the way a VLR would.
type PSICallback func(
	req *mapprotocol.ProvideSubscriberInfoRequest,
	calledGT, callingGT string,
	otid uint32,
) (*mapprotocol.ProvideSubscriberInfoResponse, error)

// ATIRequests and PSIRequests count the location enquiries the peer received.
func (p *Peer) ATIRequests() uint64 { return p.atiRequests.Load() }
func (p *Peer) PSIRequests() uint64 { return p.psiRequests.Load() }

// handleLocationInvoke serves op 71 or op 70: decode with the gateway's
// codec, ask the callback, answer with the result or the error.
func (p *Peer) handleLocationInvoke(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
	opCode int32,
	paramBytes []byte,
	otid uint32,
) {
	calledGT, callingGT := gtDigits(sccpReq.CalledAddress), gtDigits(sccpReq.CallingAddress)
	var (
		result []byte
		cbErr  error
		err    error
	)
	switch opCode {
	case int32(mapprotocol.OpAnyTimeInterrogation):
		p.atiRequests.Add(1)
		if p.cfg.OnATI == nil {
			return
		}
		req, derr := mapprotocol.DecodeAnyTimeInterrogationArg(paramBytes)
		if derr != nil {
			p.violate("AnyTimeInterrogationArg undecodable: %v", derr)
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
		var resp *mapprotocol.AnyTimeInterrogationResponse
		if resp, cbErr = p.cfg.OnATI(req, calledGT, callingGT, otid); cbErr == nil && resp != nil {
			result, err = mapprotocol.EncodeAnyTimeInterrogationRes(resp)
		}
	case int32(mapprotocol.OpProvideSubscriberInfo):
		p.psiRequests.Add(1)
		if p.cfg.OnPSI == nil {
			return
		}
		req, derr := mapprotocol.DecodeProvideSubscriberInfoArg(paramBytes)
		if derr != nil {
			p.violate("ProvideSubscriberInfoArg undecodable: %v", derr)
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
		var resp *mapprotocol.ProvideSubscriberInfoResponse
		if resp, cbErr = p.cfg.OnPSI(req, calledGT, callingGT, otid); cbErr == nil && resp != nil {
			result, err = mapprotocol.EncodeProvideSubscriberInfoRes(resp)
		}
	}
	if cbErr != nil || result == nil {
		p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, returnErrorCode(cbErr))
		return
	}
	if err != nil {
		p.logger.Warn("location result encode failed", zap.Error(err))
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}
	p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, opCode, result)
}
