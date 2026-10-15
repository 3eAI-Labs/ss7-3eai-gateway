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

func startHandshakeHandler(t *testing.T, configure ...func(*Handler)) (*Handler, chan *sccp.SccpMessage, chan *sccp.SccpMessage, *recordingAppHandler) {
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
	for _, c := range configure {
		c(h)
	}
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
// under oid with an MO-ForwardSM Invoke.
func peerBegin(t *testing.T, oid []int) *sccp.SccpMessage {
	t.Helper()
	return peerMessage(t, &Message{MessageType: MsgTypeBegin, OriginatingTID: []byte{0x0A, 0x0B, 0x0C, 0x0D},
		DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: oid}},
		Components:      []*Component{{Type: CompTypeInvoke, InvokeID: ptrInt32(1), OperationCode: ptrInt32(46), Parameter: []byte{0x30, 0x00}}}})
}

// peerMessage is msg as a peer's UDT.
func peerMessage(t *testing.T, msg *Message) *sccp.SccpMessage {
	t.Helper()
	raw, err := EncodeMessage(msg)
	require.NoError(t, err)
	return &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		CalledAddress:  &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC},
		CallingAddress: &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC},
		Data:           raw,
	}
}

func nextSent(t *testing.T, out chan *sccp.SccpMessage) *Message {
	t.Helper()
	select {
	case m := <-out:
		parsed, err := ParseMessage(m.Data)
		require.NoError(t, err)
		return parsed
	case <-time.After(2 * time.Second):
		t.Fatal("nothing sent")
		return nil
	}
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

	pdu := got.DialoguePortion
	require.NotNil(t, pdu, "the first answer must carry a dialogue portion")
	assert.Equal(t, DialoguePDUAARE, pdu.PDU, "AARE, not AARQ")
	assert.False(t, pdu.Rejected, "accepted")
	assert.Equal(t, moRelayV3, pdu.ApplicationContext.OID, "the accepted context is echoed")

	// A later message in the same dialogue carries no dialogue portion.
	again := &Message{MessageType: MsgTypeEnd, DestinationTID: dlg.RemoteTID, Dialogue: dlg,
		Components: []*Component{{Type: CompTypeReturnResult, InvokeID: ptrInt32(2)}}}
	h.handleOutgoingMessage(again)
	assert.Nil(t, nextSent(t, out).DialoguePortion)
}

func TestHandshake_UnsupportedContextRefusedWithAlternative(t *testing.T) {
	_, in, out, _ := startHandshakeHandler(t)
	in <- peerBegin(t, moRelayV2)
	got := nextSent(t, out)

	assert.Equal(t, MsgTypeAbort, got.MessageType)
	assert.False(t, got.PAbort, "a TC-U-ABORT, not a P-ABORT")
	assert.Equal(t, []byte{0x0A, 0x0B, 0x0C, 0x0D}, got.DestinationTID)

	pdu := got.DialoguePortion
	require.NotNil(t, pdu)
	assert.Equal(t, DialoguePDUAARE, pdu.PDU)
	assert.True(t, pdu.Rejected, "reject-permanent, application-context-name-not-supported (ber_test.go)")
	assert.Equal(t, moRelayV3, pdu.ApplicationContext.OID, "proposes the version we support")
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

	in <- emptyBeginFrom(t, 0x0A0B0C0D)

	cont := nextSent(t, out)
	assert.Equal(t, MsgTypeContinue, cont.MessageType)
	assert.Equal(t, []byte{0x0A, 0x0B, 0x0C, 0x0D}, cont.DestinationTID)
	require.NotNil(t, cont.DialoguePortion)
	assert.Equal(t, DialoguePDUAARE, cont.DialoguePortion.PDU)
	select {
	case <-app.dialogues:
		t.Fatal("nothing to dispatch yet")
	default:
	}

	// The peer's CONTINUE with the Invoke.
	ourTID := uint32(cont.OriginatingTID[0])<<24 | uint32(cont.OriginatingTID[1])<<16 |
		uint32(cont.OriginatingTID[2])<<8 | uint32(cont.OriginatingTID[3])
	in <- peerMessage(t, &Message{MessageType: MsgTypeContinue,
		OriginatingTID: []byte{0x0A, 0x0B, 0x0C, 0x0D}, DestinationTID: cont.OriginatingTID,
		Components: []*Component{{Type: CompTypeInvoke, InvokeID: ptrInt32(1), OperationCode: ptrInt32(46), Parameter: []byte{0x30, 0x00}}}})
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
	dlg, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, longParameter(), "corr-long")
	require.NoError(t, err)

	begin := nextSent(t, out)
	assert.Equal(t, MsgTypeBegin, begin.MessageType)
	assert.NotNil(t, begin.DialoguePortion, "the AARQ goes in the empty Begin")
	assert.Empty(t, begin.Components, "the components are held back")

	// Peer accepts: CONTINUE with an AARE.
	acc := peerMessage(t, &Message{MessageType: MsgTypeContinue,
		OriginatingTID: []byte{0x0C, 0x0C, 0x0C, 0x0C}, DestinationTID: h.encodeTID(dlg.LocalTID),
		DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: ACShortMsgMTRelayV3.OID}}})
	acc.CalledAddress, acc.CallingAddress = calling, called
	in <- acc

	cont := nextSent(t, out)
	assert.Equal(t, MsgTypeContinue, cont.MessageType)
	assert.Equal(t, []byte{0x0C, 0x0C, 0x0C, 0x0C}, cont.DestinationTID)
	assert.Nil(t, cont.DialoguePortion, "no dialogue portion in the second forward message")
	require.Len(t, cont.Components, 1)
	assert.Equal(t, CompTypeInvoke, cont.Components[0].Type)
	assert.Equal(t, longParameter(), cont.Components[0].Parameter)
}

func TestEmptyBegin_TooLongEvenForContinue(t *testing.T) {
	h, _, _, _ := startHandshakeHandler(t)
	h.SetUserDataLimit(func(_, _ *sccp.SccpAddress) int { return 60 })
	called := &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}
	calling := &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC}
	_, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, longParameter(), "corr-too-long")
	assert.ErrorIs(t, err, ErrMessageTooLong)
	assert.Zero(t, h.getDialogueCount(), "nothing left registered")
}

func emptyBeginFrom(t *testing.T, otid uint32) *sccp.SccpMessage {
	t.Helper()
	return peerMessage(t, &Message{MessageType: MsgTypeBegin,
		OriginatingTID:  []byte{byte(otid >> 24), byte(otid >> 16), byte(otid >> 8), byte(otid)},
		DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: moRelayV3}}})
}

// longParameter is a 90-octet argument: an OCTET STRING of 88.
func longParameter() []byte {
	return append([]byte{0x04, 88}, make([]byte, 88)...)
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
	require.Equal(t, MsgTypeContinue, first.MessageType)
	assert.Equal(t, int64(1), h.pendingEmptyBegins.Load())
	h.dialoguesMu.RLock()
	for _, d := range h.dialogues {
		assert.WithinDuration(t, time.Now().Add(emptyBeginWait), d.ExpiresAt, time.Second, "waits briefly")
	}
	h.dialoguesMu.RUnlock()

	in <- emptyBeginFrom(t, 0x02020202)
	second := nextSent(t, out)
	assert.Equal(t, MsgTypeAbort, second.MessageType)
	require.NotNil(t, second.AbortCause)
	assert.Equal(t, AbortResourceLimitation, *second.AbortCause)
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

// A peer refusing our context with an AARE proposing another gets the same
// request in a new dialogue under it (TS 29.002 §5.2.1) — once.
func TestVersionFallback_RetriesOnce(t *testing.T) {
	h, in, out, _ := startHandshakeHandler(t)
	rec := &recordingSink{}
	h.SetFailureSink(rec.fn())
	h.SetVersionFallback(func(refused []int, op int32, proposed []int) (*ApplicationContext, int32, bool) {
		if refused[len(refused)-1] == 3 && proposed[len(proposed)-1] == 2 {
			return &ApplicationContext{OID: proposed}, 46, true
		}
		return nil, 0, false
	})

	called := &sccp.SccpAddress{PointCode: 2, SubsystemNumber: sccp.SSNMSC}
	calling := &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC}
	first, err := h.Originate(&ACShortMsgMTRelayV3, called, calling, 44, []byte{0x30, 0x00}, "corr-v2")
	require.NoError(t, err)
	nextSent(t, out) // the v3 BEGIN

	refuse := func(tid uint32, proposed []int) {
		wire, err := h.encodeTCAPMessage(&Message{
			MessageType: MsgTypeAbort, DestinationTID: h.encodeTID(tid),
			DialoguePortion: &DialoguePortion{ApplicationContext: &ApplicationContext{OID: proposed}, Rejected: true},
		})
		require.NoError(t, err)
		in <- &sccp.SccpMessage{MessageType: sccp.MsgTypeUDT, Data: wire, CalledAddress: calling, CallingAddress: called}
	}
	refuse(first.LocalTID, ACShortMsgMTRelayV2.OID)

	retry := nextSent(t, out)
	assert.Equal(t, MsgTypeBegin, retry.MessageType)
	assert.Equal(t, ACShortMsgMTRelayV2.OID, retry.DialoguePortion.ApplicationContext.OID)
	require.Len(t, retry.Components, 1)
	assert.Equal(t, int32(46), *retry.Components[0].OperationCode, "op sent under v2")
	assert.Empty(t, rec.snapshot(), "the caller is not told about the first refusal")

	// The retry is refused too: no third attempt, the caller hears ABORTED.
	var retryTID uint32
	for _, b := range retry.OriginatingTID {
		retryTID = retryTID<<8 | uint32(b)
	}
	refuse(retryTID, ACShortMsgMTRelayV2.OID)
	require.Eventually(t, func() bool { return len(rec.snapshot()) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, FailureAbortReceived, rec.snapshot()[0].reason)
	assert.Equal(t, int32(44), rec.snapshot()[0].opCode, "reported as the operation the caller asked for")
	assert.Equal(t, 0, h.getDialogueCount())
}

// A context registered for our own dialogues only is not accepted from a peer.
func TestDispatcher_InitiatorOnlyRoutes(t *testing.T) {
	d := NewDispatcher(zaptest.NewLogger(t))
	app := newFakeHandler("map")
	require.NoError(t, d.RegisterInitiatorOnly(ACShortMsgMTRelayV2.OID, app))

	peerOpened := &Dialogue{}
	assert.ErrorIs(t, d.Dispatch(peerOpened, dialogueMsgWithAC(ACShortMsgMTRelayV2.OID)), ErrUnknownAppContext)
	ours := &Dialogue{initiatedLocally: true}
	assert.NoError(t, d.Dispatch(ours, dialogueMsgWithAC(ACShortMsgMTRelayV2.OID)))
	assert.Nil(t, d.LookupHandler(ACShortMsgMTRelayV2.OID), "never accepted on an incoming BEGIN")
}

// The peer sends the rest of a dialogue it opened to the calling address of
// our answers: with a responding address installed, that is this node's
// address, not the called address of the BEGIN (a cluster GT, or the E.214
// global title an HLR is addressed by).
func TestHandshake_RespondingAddress(t *testing.T) {
	node := &sccp.SccpAddress{PointCode: 1, SubsystemNumber: sccp.SSNMSC,
		GlobalTitle: &sccp.GlobalTitle{Address: "905900000001"}}
	var called *sccp.SccpAddress
	h, in, out, app := startHandshakeHandler(t, func(h *Handler) {
		h.SetRespondingAddress(func(c *sccp.SccpAddress) *sccp.SccpAddress { called = c; return node })
	})
	begin := peerBegin(t, moRelayV3)
	in <- begin
	dlg := <-app.dialogues
	assert.Equal(t, begin.CalledAddress, called, "given the BEGIN's called address")
	h.handleOutgoingMessage(&Message{MessageType: MsgTypeContinue, OriginatingTID: tid32(dlg.LocalTID),
		DestinationTID: dlg.RemoteTID, Dialogue: dlg})
	select {
	case m := <-out:
		assert.Equal(t, node, m.CallingAddress)
		assert.Equal(t, begin.CallingAddress, m.CalledAddress)
	case <-time.After(2 * time.Second):
		t.Fatal("nothing sent")
	}
}

// Fits counts the AARE a first answer carries, and only that one.
func TestFits_CountsTheFirstAnswersAARE(t *testing.T) {
	h, in, _, app := startHandshakeHandler(t, func(h *Handler) {
		h.SetUserDataLimit(func(_, _ *sccp.SccpAddress) int { return 60 })
	})
	in <- peerBegin(t, moRelayV3)
	dlg := <-app.dialogues
	msg := func(n int) *Message {
		return &Message{MessageType: MsgTypeContinue, OriginatingTID: tid32(dlg.LocalTID), DestinationTID: dlg.RemoteTID,
			Dialogue: dlg, Components: []*Component{{Type: CompTypeInvoke, InvokeID: ptrInt32(1), OperationCode: ptrInt32(7),
				Parameter: append([]byte{0x30, byte(n)}, make([]byte, n)...)}}}
	}
	// A CONTINUE with 22 octets of parameter: 2+6+6 (tag, OTID, DTID),
	// 2 component portion, 2 invoke, 3 invoke ID, 3 operation code, 22.
	fits, err := h.Fits(msg(20))
	require.NoError(t, err)
	assert.False(t, fits, "with the AARE it is over 60")
	assert.True(t, dlg.dialogueResponsePending.Load(), "asking does not answer the dialogue")

	h.handleOutgoingMessage(msg(0)) // the first answer carries the AARE
	fits, err = h.Fits(msg(20))
	require.NoError(t, err)
	assert.True(t, fits, "without it, it fits")
	fits, err = h.Fits(msg(40))
	require.NoError(t, err)
	assert.False(t, fits)
}
