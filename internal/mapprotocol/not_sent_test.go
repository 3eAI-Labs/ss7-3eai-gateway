// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
)

// A message the outbound channel cannot take is reported to the not-sent
// sink with its dialogue — the dispatcher's own answers and a session's
// alike — which TCAP then ends, instead of the message vanishing with the
// dialogue left open until its timer.
func TestNotSentSink(t *testing.T) {
	h := newTestHandler(t)
	h.SetTCAPOutbound(make(chan *tcap.Message)) // nobody reads it: always full
	var got []*tcap.Dialogue
	h.SetNotSentSink(func(d *tcap.Dialogue, reason string) {
		got = append(got, d)
		assert.Equal(t, "not sent: MAP outbound channel full", reason)
	})

	rejected := &tcap.Dialogue{LocalTID: 3, RemoteTID: []byte{1, 2, 3, 4}}
	require.NoError(t, h.HandleTCAPMessage(rejected, invokeMsg(99, tcap.ACMapV3.OID))) // a Reject, by the dispatcher
	purged := hrDialogue(tcap.ACMSPurgingV3)
	require.Error(t, h.PurgeMSAccept(purged, 1, &mapprotocol.PurgeMSRes{})) // a session's answer
	assert.Equal(t, []*tcap.Dialogue{rejected, purged}, got)
}
