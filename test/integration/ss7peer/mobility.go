// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
)

// Location management: the peer plays the VLR toward the gateway as HLR
// (TS 29.002 §8.1, §8.8.1, §10.2).
//
//   - StartLocationUpdate registers a subscriber as a VLR does:
//     updateLocation in a BEGIN under networkLocUpContext-v3, from the VLR
//     (SSN 7) to the HLR by the E.214 global title of the IMSI, routed on GT
//     (SSN 6). OnInsertSubscriberData answers the gateway's
//     insertSubscriberData in the dialogue; OnLocationUpdateEnd is told
//     how the gateway ended it.
//   - The gateway's cancelLocation, provideRoamingNumber and stand-alone
//     insertSubscriberData are answered by OnCancelLocation,
//     OnProvideRoamingNumber and OnInsertSubscriberData.
//
// The peer checks what a VLR relies on: the contexts and their version, the
// subsystems, the AARE of the first answer and the gateway's TID, invoke
// IDs not reused while outstanding, insertSubscriberData's IMSI — absent
// within a location update, present alone — its fields in declaration
// order, and that updateLocation is answered with its own result or errors.

// ISDReply is how the peer answers an insertSubscriberData.
type ISDReply struct {
	Result *mapprotocol.InsertSubscriberDataRes // nil: a result without a parameter
	Error  mapprotocol.ErrorCode                // non-zero: a ReturnError instead
	End    bool                                 // within a location update: answer in an END
	Abort  bool                                 // within a location update: a U-ABORT instead
	Drop   bool                                 // no answer
}

// ISDCallback answers insertSubscriberData; dialogue is the location
// update's (0: a stand-alone one).
type ISDCallback func(dialogue uint32, arg *mapprotocol.InsertSubscriberDataArg) ISDReply

// LocationUpdateEnd is how the gateway ended a location update.
type LocationUpdateEnd struct {
	Dialogue  uint32
	Aborted   bool
	HLRNumber *mapprotocol.AddressString // updateLocation's result
	Error     mapprotocol.ErrorCode      // or its error
	// Inserts are the insertSubscriberData the gateway sent in it.
	Inserts []*mapprotocol.InsertSubscriberDataArg
	// GatewayAddresses are the calling GTs of the gateway's messages in it.
	GatewayAddresses []string
}

type luDialogue struct {
	mu       sync.Mutex
	peerTID  uint32
	gwTID    []byte
	invoke   int32
	sccpReq  *sccp.SccpMessage // the addresses: of the last message from the gateway
	pd       *wire.ProtocolData
	conn     *wire.Conn // the association that message came on
	end      LocationUpdateEnd
	expected map[int32]bool // the gateway's invokes outstanding
}

var (
	acNetworkLocUpV3      = acOID(1, 3)
	acLocationCancelV3    = acOID(2, 3)
	acRoamingNbEnquiryV3  = acOID(3, 3)
	acSubscriberDataMngV3 = acOID(16, 3)
)

func init() {
	expectedACByOp[int32(mapprotocol.OpCancelLocation)] = acLocationCancelV3
	expectedACByOp[int32(mapprotocol.OpProvideRoamingNumber)] = acRoamingNbEnquiryV3
	expectedACByOp[int32(mapprotocol.OpInsertSubscriberData)] = acSubscriberDataMngV3
	for _, op := range []mapprotocol.OperationCode{mapprotocol.OpCancelLocation,
		mapprotocol.OpProvideRoamingNumber, mapprotocol.OpInsertSubscriberData} {
		expectedCalledSSNByOp[int32(op)] = ssnVLR
		expectedCallingSSNByOp[int32(op)] = ssnHLR // the VLR answers the HLR
	}
}

// mobilityState is the peer's part for location management.
type mobilityState struct {
	dialogues sync.Map // peer TID → *luDialogue
	count     atomic.Uint64
	rr        atomic.Uint64 // round robin over the associations
	// gtConn: the association a GT was last seen on as calling address.
	gtMu   sync.Mutex
	gtConn map[string]*wire.Conn
}

// LocationUpdates counts the location updates the peer started.
func (p *Peer) LocationUpdates() uint64 { return p.mobility.count.Load() }

// learnGT notes the association a calling GT came on: the SG routes a
// message to that GT back there (a node's own GT).
func (p *Peer) learnGT(conn *wire.Conn, calling *sccp.SccpAddress) {
	gt := gtDigits(calling)
	if gt == "" {
		return
	}
	p.mobility.gtMu.Lock()
	defer p.mobility.gtMu.Unlock()
	if p.mobility.gtConn == nil {
		p.mobility.gtConn = map[string]*wire.Conn{}
	}
	p.mobility.gtConn[gt] = conn
}

// connFor is the association a message to called goes on, as the SG routes
// it: a GT of Config.SharedGTPrefixes (the cluster GT, the HLR's E.214
// range) is load-shared over the associations; any other GT goes where it
// was seen as a calling address.
func (p *Peer) connFor(called *sccp.SccpAddress) *wire.Conn {
	conns := p.AcceptedConns()
	if len(conns) == 0 {
		return nil
	}
	gt := gtDigits(called)
	for _, prefix := range p.cfg.SharedGTPrefixes {
		if gt != "" && strings.HasPrefix(gt, prefix) {
			return conns[p.mobility.rr.Add(1)%uint64(len(conns))]
		}
	}
	p.mobility.gtMu.Lock()
	defer p.mobility.gtMu.Unlock()
	if c := p.mobility.gtConn[gt]; c != nil {
		return c
	}
	return conns[0]
}

// MGT is the E.214 mobile global title of an IMSI in the peer's network:
// MGTPrefix (the country code and network code of the HLR's range) and the
// MSIN, the IMSI after its 5-digit MCC+MNC (TS 23.003 §3.2).
func MGT(imsi string) string { return MGTPrefix + imsi[5:] }

// MGTPrefix is the CC+NDC the peer's network uses for E.214 GTs.
const MGTPrefix = "90539"

// StartLocationUpdate registers imsi as a VLR does: updateLocation to the
// HLR at the IMSI's E.214 GT. It returns the dialogue (the peer's TID).
func (p *Peer) StartLocationUpdate(imsi string, arg *mapprotocol.UpdateLocationArg) (uint32, error) {
	param, err := mapprotocol.EncodeUpdateLocationArg(arg)
	if err != nil {
		return 0, err
	}
	gt := func(digits string, np sccp.NumberingPlan) *sccp.GlobalTitle {
		return &sccp.GlobalTitle{GTI: sccp.GTI0100, NumberingPlan: np, EncodingScheme: sccp.ESBCD,
			NAI: sccp.NAIInternational, Address: digits}
	}
	d := &luDialogue{
		peerTID: p.originateTID.Add(1), invoke: 1, expected: map[int32]bool{},
		pd: &wire.ProtocolData{OPC: p.cfg.GatewayPointCode, SI: siSCCP, NI: p.cfg.NetworkIndicator, SLS: 7},
		// As if from the gateway, so that sendTCAP's swap addresses it.
		sccpReq: &sccp.SccpMessage{
			ProtocolClass: sccp.ClassBasicConnectionless,
			CallingAddress: &sccp.SccpAddress{AddressIndicator: sccp.AISSNPresent, SubsystemNumber: ssnHLR,
				GlobalTitle: gt(MGT(imsi), sccp.NPISDNMobile)},
			CalledAddress: &sccp.SccpAddress{AddressIndicator: sccp.AISSNPresent, SubsystemNumber: ssnVLR,
				GlobalTitle: gt(arg.VLRNumber.Digits, sccp.NPISDN)},
		},
	}
	d.end.Dialogue = d.peerTID
	if d.conn = p.connFor(d.sccpReq.CallingAddress); d.conn == nil {
		return 0, errors.New("ss7peer: no association")
	}
	p.mobility.count.Add(1)
	p.mobility.dialogues.Store(d.peerTID, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	op := int32(mapprotocol.OpUpdateLocation)
	p.sendTCAP(d.conn, d.pd, d.sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeBegin, OriginatingTID: tidBytes(d.peerTID),
		DialoguePortion: &tcap.DialoguePortion{ApplicationContext: &tcap.ApplicationContext{OID: acOIDOf(acNetworkLocUpV3)}},
		Components:      []*tcap.Component{{Type: tcap.CompTypeInvoke, InvokeID: i32(d.invoke), OperationCode: &op, Parameter: param}},
	})
	return d.peerTID, nil
}

// handleLocationUpdate takes the gateway's messages in a location update
// the peer holds.
func (p *Peer) handleLocationUpdate(conn *wire.Conn, pd *wire.ProtocolData, sccpMsg *sccp.SccpMessage, msg *tcap.Message) bool {
	if msg.MessageType != tcap.MsgTypeContinue && msg.MessageType != tcap.MsgTypeEnd && msg.MessageType != tcap.MsgTypeAbort {
		return false
	}
	v, ok := p.mobility.dialogues.Load(bytesToUint32(msg.DestinationTID))
	if !ok {
		return false
	}
	d := v.(*luDialogue)
	d.mu.Lock()
	defer d.mu.Unlock()
	if uint8(sccpMsg.CallingAddress.SubsystemNumber) != ssnHLR {
		p.violate("location update: the gateway answers from SSN %d, want the HLR's (%d)", sccpMsg.CallingAddress.SubsystemNumber, ssnHLR)
	}
	d.conn, d.pd, d.sccpReq = conn, pd, sccpMsg
	d.end.GatewayAddresses = append(d.end.GatewayAddresses, gtDigits(sccpMsg.CallingAddress))
	first := d.gwTID == nil
	switch msg.MessageType {
	case tcap.MsgTypeContinue:
		switch {
		case first:
			d.gwTID = msg.OriginatingTID
		case !bytes.Equal(msg.OriginatingTID, d.gwTID):
			p.violate("location update CONTINUE from TID % X, the dialogue's is % X", msg.OriginatingTID, d.gwTID)
		}
	case tcap.MsgTypeAbort:
		d.end.Aborted = true
		p.luEnded(d)
		return true
	}
	if first {
		if dp := msg.DialoguePortion; dp == nil || dp.Rejected || !bytes.Equal(acValueOf(msg), acNetworkLocUpV3) {
			p.violate("the gateway's first location update answer does not accept networkLocUpContext-v3 with an AARE")
		}
	} else if msg.DialoguePortion != nil {
		p.violate("location update message carries a dialogue portion after the dialogue was established")
	}
	for _, c := range msg.Components {
		p.luComponent(d, msg, c)
	}
	if msg.MessageType == tcap.MsgTypeEnd {
		if len(d.expected) > 0 {
			p.violate("the gateway ends the location update with its insertSubscriberData unanswered")
		}
		p.luEnded(d)
	}
	return true
}

func (p *Peer) luComponent(d *luDialogue, msg *tcap.Message, c *tcap.Component) {
	if c.InvokeID == nil {
		p.violate("location update component without an invoke ID")
		return
	}
	id := *c.InvokeID
	switch c.Type {
	case tcap.CompTypeInvoke:
		if c.OperationCode == nil || *c.OperationCode != int32(mapprotocol.OpInsertSubscriberData) {
			p.violate("location update: the HLR invokes op %v; only insertSubscriberData is expected", c.OperationCode)
			return
		}
		if msg.MessageType == tcap.MsgTypeEnd {
			p.violate("insertSubscriberData in an END: nothing can answer it")
			return
		}
		if d.expected[id] {
			p.violate("insertSubscriberData invoke ID %d reused while outstanding", id)
		}
		arg, err := mapprotocol.DecodeInsertSubscriberDataArg(c.Parameter)
		if err != nil {
			p.violate("InsertSubscriberDataArg: %v", err)
			return
		}
		if arg.IMSI != "" {
			p.violate("insertSubscriberData within a location update carries the IMSI (TS 29.002 §8.8.1.3)")
		}
		d.end.Inserts = append(d.end.Inserts, arg)
		reply := ISDReply{}
		if p.cfg.OnInsertSubscriberData != nil {
			reply = p.cfg.OnInsertSubscriberData(d.peerTID, arg)
		}
		if reply.Drop {
			d.expected[id] = true
			return
		}
		p.answerISDLocked(d, id, reply)
	case tcap.CompTypeReturnResult:
		if id != d.invoke || msg.MessageType != tcap.MsgTypeEnd {
			p.violate("location update result for invoke %d in a %v", id, msg.MessageType)
			return
		}
		if c.OperationCode == nil || *c.OperationCode != int32(mapprotocol.OpUpdateLocation) {
			p.violate("updateLocation's result without its operation code")
		}
		res, err := mapprotocol.DecodeUpdateLocationRes(c.Parameter)
		if err != nil {
			p.violate("UpdateLocationRes: %v", err)
			return
		}
		if res.HLRNumber.Nature != mapprotocol.NatureInternational || res.HLRNumber.NumberPlan != mapprotocol.NumberingPlanISDN {
			p.violate("hlr-Number %+v is not an international E.164 number", res.HLRNumber)
		}
		d.end.HLRNumber = &res.HLRNumber
	case tcap.CompTypeReturnError:
		if id != d.invoke || c.ErrorCode == nil {
			p.violate("location update error for invoke %d", id)
			return
		}
		code := mapprotocol.ErrorCode(*c.ErrorCode)
		if !mapprotocol.UpdateLocationErrors[code] {
			p.violate("updateLocation answered with %s, not one of its errors", code)
		}
		d.end.Error = code
	default:
		p.violate("location update %v component from the gateway", c.Type)
	}
}

// answerISDLocked sends the peer's answer to insertSubscriberData id.
func (p *Peer) answerISDLocked(d *luDialogue, id int32, reply ISDReply) {
	delete(d.expected, id)
	if reply.Abort {
		p.sendTCAP(d.conn, d.pd, d.sccpReq, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.gwTID})
		p.mobility.dialogues.Delete(d.peerTID)
		return
	}
	comp := isdAnswer(id, reply, p)
	if comp == nil {
		return
	}
	answer := &tcap.Message{MessageType: tcap.MsgTypeContinue, OriginatingTID: tidBytes(d.peerTID), DestinationTID: d.gwTID,
		Components: []*tcap.Component{comp}}
	if reply.End {
		answer.MessageType, answer.OriginatingTID = tcap.MsgTypeEnd, nil
		p.mobility.dialogues.Delete(d.peerTID)
	}
	// The peer's answer goes to the gateway's calling address, routed as
	// the SG does (connFor), not back on the association it came on.
	conn := p.connFor(d.sccpReq.CallingAddress)
	p.sendTCAP(conn, d.pd, d.sccpReq, answer)
}

func isdAnswer(id int32, reply ISDReply, p *Peer) *tcap.Component {
	if reply.Error != 0 {
		return &tcap.Component{Type: tcap.CompTypeReturnError, InvokeID: i32(id), ErrorCode: i32(int32(reply.Error))}
	}
	comp := &tcap.Component{Type: tcap.CompTypeReturnResult, InvokeID: i32(id)}
	if reply.Result != nil {
		res, err := mapprotocol.EncodeInsertSubscriberDataRes(reply.Result)
		if err != nil {
			p.violate("peer InsertSubscriberDataRes: %v", err)
			return nil
		}
		comp.OperationCode, comp.Parameter = i32(int32(mapprotocol.OpInsertSubscriberData)), res
	}
	return comp
}

// AnswerInsertSubscriberData answers an insertSubscriberData the peer's
// callback dropped.
func (p *Peer) AnswerInsertSubscriberData(dialogue uint32, invokeID int32, reply ISDReply) error {
	v, ok := p.mobility.dialogues.Load(dialogue)
	if !ok {
		return errors.New("ss7peer: no such location update")
	}
	d := v.(*luDialogue)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.expected[invokeID] {
		return errors.New("ss7peer: no such insertSubscriberData outstanding")
	}
	p.answerISDLocked(d, invokeID, reply)
	return nil
}

// AbortLocationUpdate aborts a location update as the VLR (TC-U-ABORT).
func (p *Peer) AbortLocationUpdate(dialogue uint32) error {
	v, ok := p.mobility.dialogues.LoadAndDelete(dialogue)
	if !ok {
		return errors.New("ss7peer: no such location update")
	}
	d := v.(*luDialogue)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gwTID == nil {
		return errors.New("ss7peer: the gateway has not answered yet")
	}
	p.sendTCAP(p.connFor(d.sccpReq.CallingAddress), d.pd, d.sccpReq, &tcap.Message{MessageType: tcap.MsgTypeAbort, DestinationTID: d.gwTID})
	return nil
}

func (p *Peer) luEnded(d *luDialogue) {
	p.mobility.dialogues.Delete(d.peerTID)
	if p.cfg.OnLocationUpdateEnd != nil {
		p.cfg.OnLocationUpdateEnd(d.end)
	}
}

// handleVLRInvoke serves the gateway's cancelLocation, provideRoamingNumber
// and stand-alone insertSubscriberData, each in a BEGIN of its own.
func (p *Peer) handleVLRInvoke(conn *wire.Conn, pd *wire.ProtocolData, sccpReq *sccp.SccpMessage, tcapReq *tcap.Message,
	otidBytes []byte, invokeID, opCode int32, param []byte) {
	switch mapprotocol.OperationCode(opCode) {
	case mapprotocol.OpCancelLocation:
		arg, err := mapprotocol.DecodeCancelLocationArg(param)
		if err != nil {
			p.violate("CancelLocationArg: %v", err)
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
		code := mapprotocol.ErrorCode(0)
		if p.cfg.OnCancelLocation != nil {
			code = p.cfg.OnCancelLocation(arg)
		}
		if code != 0 {
			p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, int32(code))
			return
		}
		p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, opCode, mapprotocol.EncodeCancelLocationRes())

	case mapprotocol.OpProvideRoamingNumber:
		arg, err := mapprotocol.DecodeProvideRoamingNumberArg(param)
		if err != nil {
			p.violate("ProvideRoamingNumberArg: %v", err)
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
		if p.cfg.OnProvideRoamingNumber == nil {
			return
		}
		res, code := p.cfg.OnProvideRoamingNumber(arg)
		if code == 0 && res == nil {
			code = mapprotocol.ErrSystemFailure
		}
		if code != 0 {
			p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, int32(code))
			return
		}
		b, err := mapprotocol.EncodeProvideRoamingNumberRes(res)
		if err != nil {
			p.violate("peer ProvideRoamingNumberRes: %v", err)
			return
		}
		p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, opCode, b)

	case mapprotocol.OpInsertSubscriberData:
		arg, err := mapprotocol.DecodeInsertSubscriberDataArg(param)
		if err != nil {
			p.violate("InsertSubscriberDataArg: %v", err)
			p.sendAbort(conn, pd, sccpReq, otidBytes)
			return
		}
		if arg.IMSI == "" {
			p.violate("stand-alone insertSubscriberData without the IMSI (TS 29.002 §8.8.1.3)")
		}
		reply := ISDReply{}
		if p.cfg.OnInsertSubscriberData != nil {
			reply = p.cfg.OnInsertSubscriberData(0, arg)
		}
		if reply.Drop {
			return
		}
		comp := isdAnswer(invokeID, reply, p)
		if comp == nil {
			return
		}
		p.sendTCAP(conn, pd, sccpReq, &tcap.Message{MessageType: tcap.MsgTypeEnd, DestinationTID: otidBytes,
			DialoguePortion: aareFor(tcapReq), Components: []*tcap.Component{comp}})
	}
}
