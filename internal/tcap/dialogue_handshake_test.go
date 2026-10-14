// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"context"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gotcap "github.com/wmnsk/go-tcap"
	"go.uber.org/zap/zaptest"
)

// The dialogue handshake (Q.773 §4.2.1, TS 29.002 §12.1) as a peer MSC sees
// it: a BEGIN with an AARQ is answered by an END whose dialogue portion is an
// AARE echoing the context, and a context we do not support is refused by a
// TC-U-ABORT whose AARE names one we do.

// recordingAppHandler captures the dialogue the TCAP layer hands up.
type recordingAppHandler struct{ dialogues chan *Dialogue }

func (r *recordingAppHandler) HandleTCAPMessage(d *Dialogue, _ *Message) error {
	r.dialogues <- d
	return nil
}
func (r *recordingAppHandler) Name() string { return "recording" }

var (
	moRelayV3 = []int{0, 4, 0, 0, 1, 0, 21, 3}
	moRelayV2 = []int{0, 4, 0, 0, 1, 0, 21, 2}
)

func startHandshakeHandler(t *testing.T) (*Handler, chan *sccp.SccpMessage, chan *sccp.SccpMessage, *recordingAppHandler) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	app := &recordingAppHandler{dialogues: make(chan *Dialogue, 4)}
	d := NewDispatcher(logger)
	require.NoError(t, d.Register(moRelayV3, app))

	in, out := stubChannels()
	h, err := NewHandler(&Config{
		NodeID: 1, DialogueTimeout: 5 * time.Second, InvokeTimeout: 5 * time.Second,
		IdleTimeout: 5 * time.Second, MaxDialogues: 10, MaxComponentsPerMessage: 10,
		MaxInvokesPerDialogue: 10, ChannelBufferSize: 16, WorkerPoolSize: 1,
		TimerResolution: 100 * time.Millisecond,
	}, in, out, d, logger)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx))
	// Stop waits for the workers and the timer manager, so nothing logs
	// through the test's logger after the test has returned.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
		cancel()
	})
	return h, in, out, app
}

// peerBegin is a BEGIN from a peer, OTID 0x0A0B0C0D, opening a dialogue
// under oid (the last two arcs become go-tcap's context and version).
func peerBegin(t *testing.T, oid []int) *sccp.SccpMessage {
	t.Helper()
	b := gotcap.NewBeginInvokeWithDialogue(0x0A0B0C0D, gotcap.DialogueAsID,
		uint8(oid[len(oid)-2]), uint8(oid[len(oid)-1]), 1, 46, []byte{0x30, 0x00})
	raw, err := b.MarshalBinary()
	require.NoError(t, err)
	return &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		CalledAddress:  &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC},
		CallingAddress: &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC},
		Data:           raw,
	}
}

func nextSent(t *testing.T, out chan *sccp.SccpMessage) *gotcap.TCAP {
	t.Helper()
	select {
	case m := <-out:
		parsed, err := ParseWire(m.Data)
		require.NoError(t, err)
		return parsed
	case <-time.After(2 * time.Second):
		t.Fatal("nothing sent")
		return nil
	}
}

func lastByte(ie *gotcap.IE) byte {
	if ie == nil || len(ie.Value) == 0 {
		return 0xFF
	}
	return ie.Value[len(ie.Value)-1]
}

func TestHandshake_FirstAnswerCarriesAARE(t *testing.T) {
	h, in, out, app := startHandshakeHandler(t)
	in <- peerBegin(t, moRelayV3)
	dlg := <-app.dialogues

	end := &Message{
		MessageType:    MsgTypeEnd,
		DestinationTID: dlg.RemoteTID,
		Dialogue:       dlg,
		Components:     []*Component{{Type: CompTypeReturnResult, InvokeID: ptrInt32(1)}},
	}
	h.handleOutgoingMessage(end)
	got := nextSent(t, out)

	require.NotNil(t, got.Dialogue, "the first answer must carry a dialogue portion")
	pdu := got.Dialogue.DialoguePDU
	require.NotNil(t, pdu)
	assert.Equal(t, int(gotcap.AARE), int(pdu.Type.Code()), "AARE, not AARQ")
	assert.Equal(t, gotcap.Accepted, lastByte(pdu.Result))
	assert.Equal(t, []byte{0x06, 0x07, 0x04, 0x00, 0x00, 0x01, 0x00, 21, 3},
		pdu.ApplicationContextName.Value, "the accepted context is echoed")

	// A later message in the same dialogue carries no dialogue portion.
	again := &Message{MessageType: MsgTypeEnd, DestinationTID: dlg.RemoteTID, Dialogue: dlg,
		Components: []*Component{{Type: CompTypeReturnResult, InvokeID: ptrInt32(2)}}}
	h.handleOutgoingMessage(again)
	assert.Nil(t, nextSent(t, out).Dialogue)
}

func TestHandshake_UnsupportedContextRefusedWithAlternative(t *testing.T) {
	_, in, out, _ := startHandshakeHandler(t)
	in <- peerBegin(t, moRelayV2)
	got := nextSent(t, out)

	require.NotNil(t, got.Transaction)
	assert.Equal(t, int(gotcap.Abort), int(got.Transaction.Type.Code()))
	assert.Nil(t, got.Transaction.PAbortCause, "a TC-U-ABORT, not a P-ABORT")
	assert.Equal(t, []byte{0x0A, 0x0B, 0x0C, 0x0D}, got.Transaction.DestTransactionID.Value)

	require.NotNil(t, got.Dialogue)
	pdu := got.Dialogue.DialoguePDU
	assert.Equal(t, int(gotcap.AARE), int(pdu.Type.Code()))
	assert.Equal(t, gotcap.RejectPerm, lastByte(pdu.Result))
	assert.Equal(t, gotcap.ApplicationContextNameNotSupplied, lastByte(pdu.ResultSourceDiagnostic),
		"diagnostic 2: application-context-name-not-supported")
	assert.Equal(t, []byte{0x06, 0x07, 0x04, 0x00, 0x00, 0x01, 0x00, 21, 3},
		pdu.ApplicationContextName.Value, "proposes the version we support")
}

func TestSupportedAlternative(t *testing.T) {
	d := NewDispatcher(zaptest.NewLogger(t))
	require.NoError(t, d.Register(moRelayV3, newFakeHandler("mo")))
	require.NoError(t, d.Register([]int{0, 4, 0, 0, 1, 0, 21, 2}, newFakeHandler("mo-v2")))

	assert.Equal(t, moRelayV3, d.SupportedAlternative([]int{0, 4, 0, 0, 1, 0, 21, 1}), "highest version of the context")
	unknown := []int{0, 4, 0, 0, 1, 0, 1, 3}
	assert.Equal(t, unknown, d.SupportedAlternative(unknown), "no version supported: echo")
}

func ptrInt32(v int32) *int32 { return &v }

// An empty Begin from the peer (TS 29.002 §C.2.1): accepted with a
// CONTINUE carrying the AARE; the operation arrives in the peer's CONTINUE
// and is dispatched then.
func TestEmptyBegin_InboundAcceptedThenDispatched(t *testing.T) {
	_, in, out, app := startHandshakeHandler(t)

	b := gotcap.NewBeginInvokeWithDialogue(0x0A0B0C0D, gotcap.DialogueAsID, 21, 3, 1, 46, []byte{0x30, 0x00})
	b.Components = nil
	b.SetLength()
	raw, err := b.MarshalBinary()
	require.NoError(t, err)
	in <- &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		CalledAddress:  &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC},
		CallingAddress: &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC},
		Data:           raw,
	}

	cont := nextSent(t, out)
	assert.Equal(t, int(gotcap.Continue), int(cont.Transaction.Type.Code()))
	assert.Equal(t, []byte{0x0A, 0x0B, 0x0C, 0x0D}, cont.Transaction.DestTransactionID.Value)
	require.NotNil(t, cont.Dialogue)
	assert.Equal(t, int(gotcap.AARE), int(cont.Dialogue.DialoguePDU.Type.Code()))
	select {
	case <-app.dialogues:
		t.Fatal("nothing to dispatch yet")
	default:
	}

	// The peer's CONTINUE with the Invoke.
	ourTID := uint32(cont.Transaction.OrigTransactionID.Value[0])<<24 | uint32(cont.Transaction.OrigTransactionID.Value[1])<<16 |
		uint32(cont.Transaction.OrigTransactionID.Value[2])<<8 | uint32(cont.Transaction.OrigTransactionID.Value[3])
	c := gotcap.NewContinueInvoke(0x0A0B0C0D, ourTID, 1, 46, []byte{0x30, 0x00})
	raw, err = c.MarshalBinary()
	require.NoError(t, err)
	in <- &sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, Data: raw,
		CalledAddress:  &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC},
		CallingAddress: &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}}
	select {
	case d := <-app.dialogues:
		assert.Equal(t, ourTID, d.LocalTID)
	case <-time.After(2 * time.Second):
		t.Fatal("the operation in the CONTINUE was not dispatched")
	}
}

// A BEGIN too long for one unitdata goes out empty; the components follow in
// the first CONTINUE, after the peer accepts.
func TestEmptyBegin_OutboundDefersComponents(t *testing.T) {
	h, in, out, _ := startHandshakeHandler(t)
	h.SetUserDataLimit(func(_, _ *sccp.SccpAddress) int { return 120 })

	called := &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}
	calling := &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC}
	dlg, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, make([]byte, 90), "corr-long")
	require.NoError(t, err)

	begin := nextSent(t, out)
	assert.Equal(t, int(gotcap.Begin), int(begin.Transaction.Type.Code()))
	assert.NotNil(t, begin.Dialogue, "the AARQ goes in the empty Begin")
	assert.Nil(t, begin.Components, "the components are held back")

	// Peer accepts: CONTINUE with an AARE.
	acc := &gotcap.TCAP{
		Transaction: gotcap.NewContinue(0x0C0C0C0C, dlg.LocalTID, []byte{}),
		Dialogue: gotcap.NewDialogue(gotcap.DialogueAsID, 1,
			gotcap.NewAARE(1, 25, 3, gotcap.Accepted, gotcap.DialogueServiceUser, gotcap.Null), nil),
	}
	acc.SetLength()
	raw, err := acc.MarshalBinary()
	require.NoError(t, err)
	in <- &sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, Data: raw, CalledAddress: calling, CallingAddress: called}

	cont := nextSent(t, out)
	assert.Equal(t, int(gotcap.Continue), int(cont.Transaction.Type.Code()))
	assert.Equal(t, []byte{0x0C, 0x0C, 0x0C, 0x0C}, cont.Transaction.DestTransactionID.Value)
	assert.Nil(t, cont.Dialogue, "no dialogue portion in the second forward message")
	require.NotNil(t, cont.Components)
	require.Len(t, cont.Components.Component, 1)
	assert.Equal(t, int(gotcap.Invoke), int(cont.Components.Component[0].Type.Code()))
}

func TestEmptyBegin_TooLongEvenForContinue(t *testing.T) {
	h, _, _, _ := startHandshakeHandler(t)
	h.SetUserDataLimit(func(_, _ *sccp.SccpAddress) int { return 60 })
	called := &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}
	calling := &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC}
	_, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, make([]byte, 90), "corr-too-long")
	assert.ErrorIs(t, err, ErrMessageTooLong)
	assert.Zero(t, h.getDialogueCount(), "nothing left registered")
}

func emptyBeginFrom(t *testing.T, otid uint32) *sccp.SccpMessage {
	t.Helper()
	b := gotcap.NewBeginInvokeWithDialogue(otid, gotcap.DialogueAsID, 21, 3, 1, 46, []byte{0x30, 0x00})
	b.Components = nil
	b.SetLength()
	raw, err := b.MarshalBinary()
	require.NoError(t, err)
	return &sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, Data: raw,
		CalledAddress:  &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC},
		CallingAddress: &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}}
}

// Sending END terminates the dialogue (Q.774): its state goes at once, not
// after DialogueTimeout, or every answered dialogue would count against
// MaxDialogues for the whole timeout.
func TestDialogue_RemovedWhenWeEndIt(t *testing.T) {
	h, in, out, app := startHandshakeHandler(t)
	in <- peerBegin(t, moRelayV3)
	dlg := <-app.dialogues
	require.Equal(t, 1, h.getDialogueCount())

	h.handleOutgoingMessage(&Message{MessageType: MsgTypeEnd, DestinationTID: dlg.RemoteTID, Dialogue: dlg,
		Components: []*Component{{Type: CompTypeReturnResult, InvokeID: ptrInt32(1)}}})
	nextSent(t, out)
	assert.Equal(t, 0, h.getDialogueCount())
}

// Dialogues opened by empty Begins hold state for peers that have sent no
// operation: at most MaxDialogues/10 may wait (here 1), each only briefly.
func TestEmptyBegin_Bounded(t *testing.T) {
	h, in, out, _ := startHandshakeHandler(t) // MaxDialogues 10

	in <- emptyBeginFrom(t, 0x01010101)
	first := nextSent(t, out)
	require.Equal(t, int(gotcap.Continue), int(first.Transaction.Type.Code()))
	assert.Equal(t, int64(1), h.pendingEmptyBegins.Load())
	h.dialoguesMu.RLock()
	for _, d := range h.dialogues {
		assert.WithinDuration(t, time.Now().Add(emptyBeginWait), d.ExpiresAt, time.Second, "waits briefly")
	}
	h.dialoguesMu.RUnlock()

	in <- emptyBeginFrom(t, 0x02020202)
	second := nextSent(t, out)
	assert.Equal(t, int(gotcap.Abort), int(second.Transaction.Type.Code()))
	require.NotNil(t, second.Transaction.PAbortCause)
	assert.Equal(t, byte(AbortResourceLimitation), lastByte(second.Transaction.PAbortCause))
	assert.Equal(t, 1, h.getDialogueCount(), "the refused one leaves no state")

	// Once the waiting dialogue goes, the slot is free again.
	h.dialoguesMu.RLock()
	var tid uint32
	for k := range h.dialogues {
		tid = k
	}
	h.dialoguesMu.RUnlock()
	h.removeDialogue(tid)
	assert.Equal(t, int64(0), h.pendingEmptyBegins.Load())
}
