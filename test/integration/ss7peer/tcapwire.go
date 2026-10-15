// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package ss7peer

import (
	"encoding/asn1"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// The peer reads and writes TCAP with the gateway's Q.773 codec
// (internal/tcap/ber.go), which is checked against frames built by hand
// from the Recommendation; the peer's own checks are on what the frames
// carry.

// acValueOf is the application context name of a message's dialogue
// portion as a BER OBJECT IDENTIFIER (06 07 …), or nil.
func acValueOf(m *tcap.Message) []byte {
	if m.DialoguePortion == nil || m.DialoguePortion.ApplicationContext == nil {
		return nil
	}
	b, err := asn1.Marshal(asn1.ObjectIdentifier(m.DialoguePortion.ApplicationContext.OID))
	if err != nil {
		return nil
	}
	return b
}

// acOIDOf reads back an acValue.
func acOIDOf(acValue []byte) []int {
	var oid asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(acValue, &oid); err != nil {
		return nil
	}
	return oid
}

// aareFor is the dialogue portion of the first answer to req: an AARE
// accepting req's context, or none when req carried no dialogue portion.
func aareFor(req *tcap.Message) *tcap.DialoguePortion {
	if req.DialoguePortion == nil || req.DialoguePortion.ApplicationContext == nil {
		return nil
	}
	return &tcap.DialoguePortion{ApplicationContext: req.DialoguePortion.ApplicationContext}
}

func i32(v int32) *int32 { return &v }

func tidBytes(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

// sendTCAP encodes msg and writes it back to the gateway.
func (p *Peer) sendTCAP(conn *wire.Conn, pd *wire.ProtocolData, sccpReq *sccp.SccpMessage, msg *tcap.Message) {
	b, err := tcap.EncodeMessage(msg)
	if err == nil && p.cfg.IndefiniteLengths {
		b, err = indefiniteLengths(b)
	}
	if err != nil {
		p.logger.Warn("TCAP encode failed", zap.Error(err))
		return
	}
	p.writeSCCPResponse(conn, pd, sccpReq, b)
}
