// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
)

// stubChannels returns a pair of buffered SCCP channels suitable for wiring a
// Handler in tests. The channels are buffered (size 16) so tests that exercise
// the outgoing path do not block on a missing drainer; tests that explicitly
// observe sccpOut should keep an extra reference and drain.
func stubChannels() (chan *sccp.SccpMessage, chan *sccp.SccpMessage) {
	in := make(chan *sccp.SccpMessage, 16)
	out := make(chan *sccp.SccpMessage, 16)
	return in, out
}

// Messages a peer would send, for tests that need them on the wire.
func tid32(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

func beginInvoke(otid uint32, invID, op int, param []byte) *Message {
	id, code := int32(invID), int32(op)
	return &Message{MessageType: MsgTypeBegin, OriginatingTID: tid32(otid),
		Components: []*Component{{Type: CompTypeInvoke, InvokeID: &id, OperationCode: &code, Parameter: param}}}
}

func endReturnResult(dtid uint32, invID, op int, param []byte) *Message {
	id, code := int32(invID), int32(op)
	return &Message{MessageType: MsgTypeEnd, DestinationTID: tid32(dtid),
		Components: []*Component{{Type: CompTypeReturnResult, InvokeID: &id, OperationCode: &code, Parameter: param}}}
}

func continueInvoke(otid, dtid uint32, invID, op int, param []byte) *Message {
	m := beginInvoke(otid, invID, op, param)
	m.MessageType, m.DestinationTID = MsgTypeContinue, tid32(dtid)
	return m
}
