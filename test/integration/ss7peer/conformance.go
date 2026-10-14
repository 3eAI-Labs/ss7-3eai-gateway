// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"bytes"
	"fmt"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/wmnsk/go-m3ua"
	"github.com/wmnsk/go-m3ua/messages/params"
	gotcap "github.com/wmnsk/go-tcap"
	"go.uber.org/zap"
)

// The peer judges the gateway's frames the way a real STP and HLR/MSC
// would, instead of answering whatever arrives. A peer that mirrors the
// request (OPC/DPC swapped, application context echoed) passes a gateway
// that is wrong in a self-consistent way — which is how a fixed DPC of 0 and
// the wrong SMS application context once went unnoticed.
//
// Every violation is recorded (Violations) so a test can fail on it even
// when the dialogue happens to time out instead of failing loudly.

// siSCCP is the MTP3 service indicator for SCCP (Q.704 §14.2.1).
const siSCCP = 3

// Q.713 §3.4.1 address indicator, bit 7: 0 = route on GT, 1 = route on
// DPC/SSN. Encoded independently of internal/sccp so a wrong constant there
// cannot hide here.
const aiRouteOnSSN = 0x40

// MAP application context names for the operations the peer serves, as the
// BER OBJECT IDENTIFIER the dialogue portion carries (TS 29.002 §17.3.3:
// map-ac = {0 4 0 0 1 0}, then the context arc and the version).
var expectedACByOp = map[int32][]byte{
	int32(mapprotocol.OpSendRoutingInfoForSM):  acOID(20, 3), // shortMsgGatewayContext-v3
	int32(mapprotocol.OpMtForwardSM):           acOID(25, 3), // shortMsgMT-RelayContext-v3
	int32(mapprotocol.OpProvideSubscriberInfo): acOID(28, 3), // subscriberInfoEnquiryContext-v3
	int32(mapprotocol.OpAnyTimeInterrogation):  acOID(29, 3), // anyTimeInfoEnquiryContext-v3
}

// Subsystem numbers (TS 23.003 §8): the called address names the node the
// operation is for, and for the location enquiries the calling address names
// the role the gateway plays — the HLR or the gsmSCF answers there.
const (
	ssnHLR    = 6
	ssnVLR    = 7
	ssnMSC    = 8
	ssnGSMSCF = 147
)

var expectedCalledSSNByOp = map[int32]uint8{
	int32(mapprotocol.OpSendRoutingInfoForSM):  ssnHLR,
	int32(mapprotocol.OpMtForwardSM):           ssnMSC,
	int32(mapprotocol.OpAnyTimeInterrogation):  ssnHLR,
	int32(mapprotocol.OpProvideSubscriberInfo): ssnVLR,
}

var expectedCallingSSNByOp = map[int32]uint8{
	int32(mapprotocol.OpAnyTimeInterrogation):  ssnGSMSCF,
	int32(mapprotocol.OpProvideSubscriberInfo): ssnHLR,
}

// checkSubsystems validates the SSNs of an operation's SCCP addresses.
func (p *Peer) checkSubsystems(opCode int32, called, calling *sccp.SccpAddress) {
	if want, ok := expectedCalledSSNByOp[opCode]; ok && uint8(called.SubsystemNumber) != want {
		p.violate("op %d called SSN %d, want %d", opCode, called.SubsystemNumber, want)
	}
	if want, ok := expectedCallingSSNByOp[opCode]; ok && uint8(calling.SubsystemNumber) != want {
		p.violate("op %d calling SSN %d, want %d", opCode, calling.SubsystemNumber, want)
	}
}

// Version 2 equivalents (TS 29.002 §17.3.2.21, §17.3.2.26; forwardSM is
// op 46 in version 2's shortMsgRelayPackage-v2).
var expectedV2ACByOp = map[int32][]byte{
	int32(mapprotocol.OpSendRoutingInfoForSM): acOID(20, 2),
	int32(mapprotocol.OpMoForwardSM):          acOID(25, 2),
}

func acOID(context, version byte) []byte {
	return []byte{0x06, 0x07, 0x04, 0x00, 0x00, 0x01, 0x00, context, version}
}

// Violations returns every conformance violation the peer has seen, in
// arrival order. A conforming gateway leaves it empty.
func (p *Peer) Violations() []string {
	p.violationsMu.Lock()
	defer p.violationsMu.Unlock()
	return append([]string(nil), p.violations...)
}

func (p *Peer) violate(format string, args ...any) {
	v := fmt.Sprintf(format, args...)
	p.logger.Warn("conformance violation", zap.String("violation", v))
	p.violationsMu.Lock()
	p.violations = append(p.violations, v)
	p.violationsMu.Unlock()
}

// checkRoutingLabel validates the MTP3 routing label of an inbound DATA
// (RFC 4666 §3.3.1, Protocol Data). A real STP discards a message whose DPC
// is not a known destination, so the caller drops on false.
func (p *Peer) checkRoutingLabel(pd *params.ProtocolDataPayload) bool {
	ok := true
	if pd.ServiceIndicator != siSCCP {
		p.violate("MTP3 SI=%d, want %d (SCCP)", pd.ServiceIndicator, siSCCP)
		ok = false
	}
	if pd.DestinationPointCode != p.cfg.PointCode {
		p.violate("MTP3 DPC=%d, want the peer's point code %d", pd.DestinationPointCode, p.cfg.PointCode)
		ok = false
	}
	if pd.OriginatingPointCode != p.cfg.GatewayPointCode {
		p.violate("MTP3 OPC=%d, want the gateway's point code %d", pd.OriginatingPointCode, p.cfg.GatewayPointCode)
		ok = false
	}
	if pd.NetworkIndicator != p.cfg.NetworkIndicator {
		p.violate("MTP3 NI=%d, want %d", pd.NetworkIndicator, p.cfg.NetworkIndicator)
		ok = false
	}
	return ok
}

// checkAddress validates one SCCP address against Q.713 §3.4: a GT-routed
// address must carry a GT, a DPC/SSN-routed one must carry an SSN, and a
// DPC/SSN-routed called address that names a point code must name this peer.
func (p *Peer) checkAddress(role string, addr *sccp.SccpAddress, called bool) bool {
	if addr == nil {
		p.violate("SCCP %s missing", role)
		return false
	}
	ai := byte(addr.AddressIndicator)
	if ai&aiRouteOnSSN == 0 {
		if addr.GlobalTitle == nil || addr.GlobalTitle.Address == "" {
			p.violate("SCCP %s routes on GT (AI=0x%02X) but carries no GT", role, ai)
			return false
		}
		return true
	}
	if addr.SubsystemNumber == 0 {
		p.violate("SCCP %s routes on SSN (AI=0x%02X) but carries no SSN", role, ai)
		return false
	}
	if called && ai&0x01 != 0 && addr.PointCode != p.cfg.PointCode {
		p.violate("SCCP %s routes on SSN to PC %d, not the peer (%d)", role, addr.PointCode, p.cfg.PointCode)
		return false
	}
	return true
}

// checkApplicationContext validates the AARQ application context name of a
// BEGIN against the operation it carries. On a mismatch it returns the
// context the peer supports, which a real HLR/MSC proposes in its AARE.
func (p *Peer) checkApplicationContext(got []byte, opCode int32) (want []byte, ok bool) {
	want, known := expectedACByOp[opCode]
	if !known {
		return nil, true
	}
	if bytes.Equal(got, want) {
		return want, true
	}
	p.violate("op %d sent under application context % X, want % X", opCode, got, want)
	return want, false
}

// sendACNotSupported answers a BEGIN with a TC-U-ABORT whose AARE rejects
// the dialogue (reject-permanent, application-context-name-not-supported)
// and names the context the peer supports (Q.773 §4.2.2; TS 29.002 §15.6).
func (p *Peer) sendACNotSupported(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	otidBytes []byte,
	supported []byte,
) {
	tx := gotcap.NewAbort(bytesToUint32(otidBytes), 0, []byte{})
	tx.PAbortCause = nil
	ctx, ver := supported[len(supported)-2], supported[len(supported)-1]
	dlg := gotcap.NewDialogue(gotcap.DialogueAsID, 1,
		gotcap.NewAARE(1, ctx, ver, gotcap.RejectPerm, gotcap.DialogueServiceUser,
			gotcap.ApplicationContextNameNotSupplied), // value 2: "not supported" in Q.773
		nil)
	msg := &gotcap.TCAP{Transaction: tx, Dialogue: dlg}
	msg.SetLength()
	b, err := msg.MarshalBinary()
	if err != nil {
		p.logger.Warn("U-ABORT encode failed", zap.Error(err))
		return
	}
	p.writeSCCPResponse(conn, pd, sccpReq, b)
}
