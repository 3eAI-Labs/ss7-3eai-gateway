// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"go.uber.org/zap"
)

var (
	ErrInvalidMessage       = errors.New("TCAP: invalid message")
	ErrDialogueNotFound     = errors.New("TCAP: dialogue not found")
	ErrDialogueLimitReached = errors.New("TCAP: max dialogues reached")
	ErrMessageTooLong       = errors.New("TCAP: message too long for one SCCP unitdata")
	ErrInvokeNotFound       = errors.New("TCAP: invoke not found")
	ErrInvalidTID           = errors.New("TCAP: invalid transaction ID")
	ErrDialogueExpired      = errors.New("TCAP: dialogue expired")
	ErrShutdown             = errors.New("TCAP: handler is shutting down")
)

const (
	ChannelBufferSize      = 100
	DefaultWorkerPoolSize  = 4
	DefaultTimerResolution = 1 * time.Second
)

// NewHandler creates a new TCAP handler instance.
//
// sccpIncoming and sccpOutgoing wire TCAP to the SCCP layer below; obtain
// these from sccp.Handler.TcapIncomingChannel() and TcapOutgoingChannel().
// dispatcher routes messages to application-context-specific handlers
// (MAP, CAP, INAP). All four wired dependencies are required: nil values
// fail fast per carrier-grade strict mode (GSMA FS.11).
func NewHandler(
	config *Config,
	sccpIncoming <-chan *sccp.SccpMessage,
	sccpOutgoing chan<- *sccp.SccpMessage,
	dispatcher *Dispatcher,
	logger *zap.Logger,
) (*Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if sccpIncoming == nil {
		return nil, fmt.Errorf("sccpIncoming channel cannot be nil")
	}
	if sccpOutgoing == nil {
		return nil, fmt.Errorf("sccpOutgoing channel cannot be nil")
	}
	if dispatcher == nil {
		return nil, fmt.Errorf("dispatcher cannot be nil")
	}
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:         config,
		nodeID:         config.NodeID,
		dialogues:      make(map[uint32]*Dialogue),
		sccpIncomingCh: sccpIncoming,
		sccpOutgoingCh: sccpOutgoing,
		dispatcher:     dispatcher,
		appOutgoingCh:  make(chan *Message, config.ChannelBufferSize),
		errorCh:        make(chan error, 10),
		shutdownCh:     make(chan struct{}),
		metrics:        &Metrics{},
		logger:         logger,
	}

	// Initialize timer manager
	handler.timerManager = &TimerManager{
		handler:    handler,
		shutdownCh: make(chan struct{}),
	}

	return handler, nil
}

// SetFailureSink installs the callback fired when an originated dialogue
// (one we opened via Originate) ends without a ReturnResult — i.e.
// DialogueTimeout or inbound P/U-ABORT. nil disables; useful in tests
// that exercise codecs without wiring NATS.
//
// The sink runs synchronously inside the timer-manager goroutine for
// timeouts and inside the SCCP-incoming worker for inbound ABORTs.
// Implementations must not block (publish to a queue if needed).
func (h *Handler) SetFailureSink(sink FailureSink) {
	h.failureSink = sink
}

// Start initializes TCAP handler and worker pool.
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("Starting TCAP handler",
		zap.Uint8("node_id", h.nodeID),
		zap.Int("max_dialogues", h.config.MaxDialogues),
		zap.Duration("dialogue_timeout", h.config.DialogueTimeout))

	// Track workers + timer manager so Stop can wait for graceful drain.
	h.wg.Add(h.config.WorkerPoolSize + 1)

	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}
	go h.timerManager.run(ctx)

	h.logger.Info("TCAP handler started successfully",
		zap.Int("workers", h.config.WorkerPoolSize))

	return nil
}

// Stop gracefully stops the TCAP handler. Waits for all workers and the timer
// manager to return before closing log emission, preventing -race "Log in
// goroutine after test completed" panics observed under TestTCAPConcurrentMessages.
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping TCAP handler")

	close(h.shutdownCh)
	close(h.timerManager.shutdownCh)

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		h.logger.Info("TCAP handler stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("TCAP handler stop timeout")
	}

	return nil
}

// ApplicationOutbound returns the write side of the application→TCAP channel.
// MAP/CAP/INAP handlers push outgoing TCAP responses here; the TCAP worker
// drains, encodes, and forwards via SCCP.
func (h *Handler) ApplicationOutbound() chan<- *Message {
	return h.appOutgoingCh
}

// Originate opens a new outbound TCAP dialogue with a BEGIN message carrying
// a single Invoke component. Used by the NATS bridge to initiate MAP/CAP
// transactions toward HLR/MSC/gsmSSF peers (e.g. SRI-for-SM, MtForwardSM).
//
// Wire effects:
//   - Allocates a fresh local TID via the node-id-prefixed generator.
//   - Registers a Dialogue in StateInitWait with addresses pinned, op code
//     and NATS correlation ID stored for the eventual ReturnResult/Error path.
//   - Pushes a *Message of MsgTypeBegin onto the outbound channel; the
//     worker encodes it (including AC OID in the dialogue portion) and
//     forwards via SCCP UDT to `called`.
//
// `called` is the destination peer (HLR GT for SRI-for-SM, VMSC for MT-Fwd-SM)
// and `calling` is our own SCCP address (so the peer's response can come back).
// Both must be non-nil — Q.713 §3.4 requires both addresses on connectionless
// SCCP.
//
// Returns the registered Dialogue. Caller does not need to retain it; the
// dispatcher's response path looks it up by DTID on the incoming END/CONTINUE.
// On channel-full, returns ErrShutdown-shaped error and removes the dialogue.
func (h *Handler) Originate(
	ac *ApplicationContext,
	called, calling *sccp.SccpAddress,
	opCode int32,
	parameter []byte,
	natsCorrelationID string,
) (*Dialogue, error) {
	return h.originate(ac, called, calling, opCode, opCode, parameter, natsCorrelationID, false)
}

// originate opens a dialogue. logicalOp is the operation the caller asked
// for (it selects the answer's decoder and the reply subject); wireOp is the
// code sent, which differs only when a lower version names the operation
// differently (MT-ForwardSM is forwardSM in version 2).
func (h *Handler) originate(
	ac *ApplicationContext,
	called, calling *sccp.SccpAddress,
	logicalOp, wireOp int32,
	parameter []byte,
	natsCorrelationID string,
	isFallback bool,
) (*Dialogue, error) {
	opCode := wireOp
	if ac == nil {
		return nil, errors.New("Originate: application context required")
	}
	if called == nil || calling == nil {
		return nil, errors.New("Originate: both SCCP addresses required (Q.713 §3.4)")
	}

	if h.getDialogueCount() >= h.config.MaxDialogues {
		return nil, ErrDialogueLimitReached
	}

	localTID := h.allocateTID()
	invokeID := int32(1)

	dialogue := &Dialogue{
		LocalTID:            localTID,
		State:               StateInitWait,
		ApplicationContext:  ac,
		LocalAddress:        calling,
		RemoteAddress:       called,
		NATSCorrelationID:   natsCorrelationID,
		OriginatingOpCode:   logicalOp,
		initiatedLocally:    true,
		versionFallbackUsed: isFallback,
		CreatedAt:           time.Now(),
		LastActivity:        time.Now(),
		ExpiresAt:           time.Now().Add(h.config.DialogueTimeout),
		PendingInvokes: map[int32]*PendingInvoke{
			invokeID: {
				InvokeID:      invokeID,
				OperationCode: opCode,
				SentAt:        time.Now(),
				ExpiresAt:     time.Now().Add(h.config.InvokeTimeout),
				Parameter:     parameter,
			},
		},
	}

	h.dialoguesMu.Lock()
	h.dialogues[localTID] = dialogue
	h.dialoguesMu.Unlock()
	h.metrics.ActiveDialogues.Add(1)
	h.metrics.TotalDialogues.Add(1)

	beginMsg := &Message{
		MessageType:    MsgTypeBegin,
		OriginatingTID: h.encodeTID(localTID),
		DialoguePortion: &DialoguePortion{
			ApplicationContext: ac,
		},
		Components: []*Component{{
			Type:          CompTypeInvoke,
			InvokeID:      &invokeID,
			OperationCode: &opCode,
			Parameter:     parameter,
		}},
		Dialogue: dialogue,
	}

	if err := h.deferComponentsIfTooLong(dialogue, beginMsg); err != nil {
		h.removeDialogue(localTID)
		return nil, err
	}

	select {
	case h.appOutgoingCh <- beginMsg:
		h.logger.Info("TCAP dialogue originated",
			zap.Uint32("local_tid", localTID),
			zap.Int32("op_code", opCode),
			zap.String("nats_correlation_id", natsCorrelationID),
			zap.String("ac", ac.Name))
		return dialogue, nil
	default:
		// Roll back the registration so we don't leak a dialogue for a BEGIN
		// that never went out. The peer will never know it existed.
		h.removeDialogue(localTID)
		h.metrics.Errors.Add(1)
		return nil, errors.New("TCAP outbound channel full")
	}
}

// worker processes incoming SCCP-routed TCAP messages and outbound
// application replies. Per-iteration select gives no priority — fairness is
// the runtime's responsibility.
func (h *Handler) worker(ctx context.Context, id int) {
	defer h.wg.Done()
	h.logger.Info("TCAP worker started", zap.Int("worker_id", id))
	defer h.logger.Info("TCAP worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case sccpMsg := <-h.sccpIncomingCh:
			h.handleIncomingMessage(sccpMsg)
		case appMsg := <-h.appOutgoingCh:
			h.handleOutgoingMessage(appMsg)
		}
	}
}

// handleIncomingMessage processes a TCAP-bearing SCCP UDT/XUDT delivered by
// the SCCP layer. The SCCP wrapper carries the calling/called addresses
// needed to pin dialogue context (Q.713 §3.4) and to construct outbound
// replies in the same dialogue.
func (h *Handler) handleIncomingMessage(sccpMsg *sccp.SccpMessage) {
	if sccpMsg == nil {
		h.metrics.Errors.Add(1)
		return
	}
	h.metrics.MessagesReceived.Add(1)

	tcapMsg, err := h.parseTCAPMessage(sccpMsg.Data)
	if err != nil {
		h.logger.Error("Failed to parse TCAP message", zap.Error(err))
		h.metrics.ParseErrors.Add(1)
		h.metrics.Errors.Add(1)
		return
	}

	h.logger.Debug("Received TCAP message",
		zap.String("type", tcapMsg.MessageType.String()),
		zap.String("otid", hex.EncodeToString(tcapMsg.OriginatingTID)),
		zap.Int("components", len(tcapMsg.Components)))

	switch tcapMsg.MessageType {
	case MsgTypeBegin:
		h.handleBegin(sccpMsg, tcapMsg)
	case MsgTypeContinue:
		h.handleContinue(sccpMsg, tcapMsg)
	case MsgTypeEnd:
		h.handleEnd(sccpMsg, tcapMsg)
	case MsgTypeAbort:
		h.handleAbort(sccpMsg, tcapMsg)
	case MsgTypeUnidirectional:
		h.handleUnidirectional(sccpMsg, tcapMsg)
	default:
		h.logger.Warn("Unknown TCAP message type",
			zap.Uint8("type", uint8(tcapMsg.MessageType)))
		h.metrics.Errors.Add(1)
	}
}

// handleBegin processes TCAP BEGIN message. sccpCtx supplies the addresses
// needed for any P-Abort response and for pinning dialogue addresses.
func (h *Handler) handleBegin(sccpCtx *sccp.SccpMessage, msg *Message) {
	h.metrics.BeginReceived.Add(1)

	if h.getDialogueCount() >= h.config.MaxDialogues {
		h.logger.Error("Max dialogues reached, rejecting BEGIN")
		h.sendAbort(sccpCtx, msg, AbortResourceLimitation, true)
		return
	}

	localTID := h.allocateTID()

	// AC may be absent if BEGIN lacks dialogue portion; dispatch will P-Abort.
	var appCtx *ApplicationContext
	if msg.DialoguePortion != nil {
		appCtx = msg.DialoguePortion.ApplicationContext
	}

	dialogue := &Dialogue{
		LocalTID:           localTID,
		RemoteTID:          msg.OriginatingTID,
		State:              StateActive,
		ApplicationContext: appCtx,
		LocalAddress:       sccpCtx.CalledAddress,
		RemoteAddress:      sccpCtx.CallingAddress,
		CreatedAt:          time.Now(),
		LastActivity:       time.Now(),
		ExpiresAt:          time.Now().Add(h.config.DialogueTimeout),
		PendingInvokes:     make(map[int32]*PendingInvoke),
	}

	dialogue.dialogueResponsePending.Store(appCtx != nil)

	h.dialoguesMu.Lock()
	h.dialogues[localTID] = dialogue
	h.dialoguesMu.Unlock()

	h.metrics.ActiveDialogues.Add(1)
	h.metrics.TotalDialogues.Add(1)

	h.logger.Info("Created new dialogue",
		zap.Uint32("local_tid", localTID),
		zap.String("remote_tid", hex.EncodeToString(msg.OriginatingTID)))

	// An empty Begin (TS 29.002 §C.2.1): the peer opens the dialogue with
	// the AARQ alone and will send its components in a CONTINUE once we
	// accept. A context we implement is accepted with a CONTINUE carrying
	// the AARE; anything else goes through dispatch, which refuses it.
	if len(msg.Components) == 0 && appCtx != nil && h.dispatcher.LookupHandler(appCtx.OID) != nil {
		// An accepted empty Begin holds state for a peer that has sent no
		// operation yet: bound how many may wait, and how long. The
		// continuation follows our CONTINUE after one round trip.
		if h.pendingEmptyBegins.Load() >= int64(max(1, h.config.MaxDialogues/10)) {
			h.logger.Warn("Too many dialogues waiting after an empty Begin, refusing")
			h.removeDialogue(localTID)
			h.sendAbort(sccpCtx, msg, AbortResourceLimitation, true)
			return
		}
		dialogue.awaitingContinuation.Store(true)
		h.pendingEmptyBegins.Add(1)
		dialogue.ExpiresAt = time.Now().Add(min(h.config.DialogueTimeout, emptyBeginWait))

		h.handleOutgoingMessage(&Message{
			MessageType:    MsgTypeContinue,
			OriginatingTID: h.encodeTID(localTID),
			DestinationTID: msg.OriginatingTID,
			Dialogue:       dialogue,
		})
		return
	}

	h.processComponents(dialogue, msg.Components)

	if h.dispatchToApplication(sccpCtx, dialogue, msg) {
		h.removeDialogue(localTID)
	}
}

// handleContinue processes TCAP CONTINUE message.
func (h *Handler) handleContinue(sccpCtx *sccp.SccpMessage, msg *Message) {
	h.metrics.ContinueReceived.Add(1)

	localTID := h.decodeTID(msg.DestinationTID)
	if localTID == 0 {
		h.logger.Warn("Invalid DTID in CONTINUE", zap.String("dtid", hex.EncodeToString(msg.DestinationTID)))
		return
	}

	dialogue := h.getDialogue(localTID)
	if dialogue == nil {
		h.logger.Warn("Dialogue not found for CONTINUE", zap.Uint32("local_tid", localTID))
		h.sendAbort(sccpCtx, msg, AbortUnrecognizedTransactionID, true)
		return
	}

	dialogue.LastActivity = time.Now()
	dialogue.ExpiresAt = time.Now().Add(h.config.DialogueTimeout)
	dialogue.MessagesReceived.Add(1)

	if len(msg.OriginatingTID) > 0 {
		dialogue.RemoteTID = msg.OriginatingTID
	}

	h.logger.Debug("Updated dialogue",
		zap.Uint32("local_tid", localTID),
		zap.String("remote_tid", hex.EncodeToString(dialogue.RemoteTID)))

	if dialogue.awaitingContinuation.Swap(false) {
		h.pendingEmptyBegins.Add(-1)
	}
	h.sendDeferredComponents(dialogue)

	h.processComponents(dialogue, msg.Components)

	if h.dispatchToApplication(sccpCtx, dialogue, msg) {
		h.removeDialogue(localTID)
	}
}

// handleEnd processes TCAP END message.
func (h *Handler) handleEnd(sccpCtx *sccp.SccpMessage, msg *Message) {
	h.metrics.EndReceived.Add(1)

	localTID := h.decodeTID(msg.DestinationTID)
	if localTID == 0 {
		h.logger.Warn("Invalid DTID in END")
		return
	}

	dialogue := h.getDialogue(localTID)
	if dialogue == nil {
		h.logger.Warn("Dialogue not found for END", zap.Uint32("local_tid", localTID))
		return
	}

	dialogue.MessagesReceived.Add(1)

	h.logger.Info("Dialogue ended",
		zap.Uint32("local_tid", localTID),
		zap.Duration("duration", time.Since(dialogue.CreatedAt)))

	h.processComponents(dialogue, msg.Components)

	// END is terminal regardless of dispatch outcome.
	h.dispatchToApplication(sccpCtx, dialogue, msg)
	h.removeDialogue(localTID)

	// An END that answered nothing — no result, no error, only the segments
	// of one — leaves the originator without an outcome; tell it.
	if dialogue.initiatedLocally {
		h.reportFailure(dialogue, FailureRemoteError, "dialogue ended without a result")
	}
}

// handleAbort processes TCAP ABORT message.
func (h *Handler) handleAbort(sccpCtx *sccp.SccpMessage, msg *Message) {
	h.metrics.AbortReceived.Add(1)

	localTID := h.decodeTID(msg.DestinationTID)
	if localTID == 0 {
		h.logger.Warn("Invalid DTID in ABORT")
		return
	}

	dialogue := h.getDialogue(localTID)
	if dialogue == nil {
		h.logger.Warn("Dialogue not found for ABORT", zap.Uint32("local_tid", localTID))
		return
	}

	dialogue.MessagesReceived.Add(1)

	abortType := "U-ABORT"
	if msg.PAbort {
		abortType = "P-ABORT"
	}

	// A P-ABORT carries a cause; a TC-U-ABORT may carry none, its reason
	// being in the dialogue portion (an AARE refusing the context).
	var detail string
	switch {
	case msg.AbortCause != nil:
		detail = fmt.Sprintf("%s cause=0x%02x", abortType, uint8(*msg.AbortCause))
	case msg.DialoguePortion != nil && msg.DialoguePortion.Rejected:
		detail = abortType + " application context refused"
		if ac := msg.DialoguePortion.ApplicationContext; ac != nil {
			detail += fmt.Sprintf(", peer proposes %s", oidString(ac.OID))
		}
	default:
		detail = abortType
	}

	h.logger.Info("Dialogue aborted",
		zap.Uint32("local_tid", localTID),
		zap.String("detail", detail))

	if h.retryUnderLowerVersion(dialogue, msg) {
		h.removeDialogue(localTID)
		return
	}

	// ABORT is terminal regardless of dispatch outcome.
	h.dispatchToApplication(sccpCtx, dialogue, msg)
	h.removeDialogue(localTID)

	h.reportFailure(dialogue, FailureAbortReceived, detail)
}

// handleUnidirectional processes TCAP UNIDIRECTIONAL message.
// UNI has no dialogue; abort flag is meaningless (nothing to tear down).
func (h *Handler) handleUnidirectional(sccpCtx *sccp.SccpMessage, msg *Message) {
	h.logger.Debug("Received UNIDIRECTIONAL message",
		zap.Int("components", len(msg.Components)))
	h.dispatchToApplication(sccpCtx, nil, msg)
}

// handleOutgoingMessage processes outgoing TCAP messages from application
// handlers. SCCP addresses are sourced from msg.Dialogue when set (the
// in-process back-reference applications use for END responses), otherwise
// the legacy wire-TID lookup is attempted (correct only for BEGIN where
// OTID = our LocalTID). Without either, addresses remain nil and the
// receiver will treat the SCCP wrapper as malformed.
// Address rules: CalledAddress=Remote (the peer), CallingAddress=Local (us).
func (h *Handler) handleOutgoingMessage(msg *Message) {
	h.metrics.MessagesSent.Add(1)

	dlg := msg.Dialogue
	if dlg == nil {
		dlg = h.lookupDialogueForOutgoing(msg)
	}
	answerDialogueRequest(dlg, msg)

	payload, err := h.encodeTCAPMessage(msg)
	if err != nil {
		h.logger.Error("Failed to encode TCAP message", zap.Error(err))
		h.metrics.Errors.Add(1)
		return
	}

	var called, calling *sccp.SccpAddress
	if dlg != nil {
		called = dlg.RemoteAddress
		calling = dlg.LocalAddress
	}

	if err := h.sendOnSCCP(payload, called, calling); err != nil {
		h.logger.Error("Failed to forward TCAP message to SCCP", zap.Error(err))
		return
	}
	h.updateOutgoingMetrics(msg)

	// Sending END or ABORT terminates the dialogue at our end (Q.774):
	// nothing more can arrive for it, so its state goes now rather than at
	// its timer — otherwise every answered dialogue would stay counted
	// against MaxDialogues for DialogueTimeout.
	if dlg != nil && (msg.MessageType == MsgTypeEnd || msg.MessageType == MsgTypeAbort) {
		h.removeDialogue(dlg.LocalTID)
	}
}

// lookupDialogueForOutgoing locates the dialogue an outbound TCAP message
// belongs to. CONTINUE/END/ABORT carry DTID = our local TID; BEGIN carries
// only OTID = our local TID for outgoing-initiated dialogues.
func (h *Handler) lookupDialogueForOutgoing(msg *Message) *Dialogue {
	var localTID uint32
	switch msg.MessageType {
	case MsgTypeContinue, MsgTypeEnd, MsgTypeAbort:
		if len(msg.DestinationTID) >= 4 {
			localTID = h.decodeTID(msg.DestinationTID)
		}
	case MsgTypeBegin:
		if len(msg.OriginatingTID) >= 4 {
			localTID = h.decodeTID(msg.OriginatingTID)
		}
	}
	if localTID == 0 {
		return nil
	}
	return h.getDialogue(localTID)
}

// sendOnSCCP wraps a TCAP payload in an SCCP UDT and pushes it to the
// outgoing channel. ProtocolClass 0 (basic connectionless, Q.713 §2.2).
func (h *Handler) sendOnSCCP(payload []byte, called, calling *sccp.SccpAddress) error {
	out := &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		ProtocolClass:  sccp.ClassBasicConnectionless,
		CalledAddress:  called,
		CallingAddress: calling,
		Data:           payload,
	}
	select {
	case h.sccpOutgoingCh <- out:
		return nil
	default:
		h.metrics.Errors.Add(1)
		return errors.New("SCCP channel full")
	}
}

// processComponents processes TCAP components
func (h *Handler) processComponents(dialogue *Dialogue, components []*Component) {
	dialogue.ComponentsReceived.Add(uint64(len(components)))
	h.metrics.ComponentsReceived.Add(uint64(len(components)))

	dialogue.invokesMu.Lock()
	defer dialogue.invokesMu.Unlock()

	for _, comp := range components {
		switch comp.Type {
		case CompTypeInvoke:
			h.metrics.InvokesReceived.Add(1)
			// Store pending invoke
			if comp.InvokeID != nil {
				dialogue.PendingInvokes[*comp.InvokeID] = &PendingInvoke{
					InvokeID:      *comp.InvokeID,
					OperationCode: *comp.OperationCode,
					SentAt:        time.Now(),
					ExpiresAt:     time.Now().Add(h.config.InvokeTimeout),
					Parameter:     comp.Parameter,
				}
			}

		case CompTypeReturnResult:
			h.metrics.ResultsReceived.Add(1)
			// A Last answers the invoke it names; a Not Last does not.
			if comp.InvokeID != nil && !comp.NotLast {
				delete(dialogue.PendingInvokes, *comp.InvokeID)
			}

		case CompTypeReturnError:
			h.metrics.ErrorsReceived.Add(1)
			// Remove pending invoke
			if comp.InvokeID != nil {
				delete(dialogue.PendingInvokes, *comp.InvokeID)
			}

		case CompTypeReject:
			h.metrics.RejectsReceived.Add(1)
			// Remove pending invoke
			if comp.InvokeID != nil {
				delete(dialogue.PendingInvokes, *comp.InvokeID)
			}
		}
	}
}

// dispatchToApplication routes msg to its registered application-context handler.
// Returns true if the dialogue must be torn down by the caller (P-Abort already
// sent for BEGIN/CONTINUE per Q.773 §3.1.5).
//
// AC is dialogue-scoped: BEGIN's AARQ pins it, but CONTINUE/END/ABORT typically
// arrive without a dialogue portion. We pin dialogue.ApplicationContext onto
// msg before dispatching so the strict OID-router resolves correctly.
//
// sccpCtx is needed to construct any P-Abort response (addresses are reversed
// from the original incoming message).
func (h *Handler) dispatchToApplication(sccpCtx *sccp.SccpMessage, dialogue *Dialogue, msg *Message) (abort bool) {
	pinApplicationContext(dialogue, msg)

	err := h.dispatcher.Dispatch(dialogue, msg)
	if err == nil {
		return false
	}

	abortable := dialogue != nil && (msg.MessageType == MsgTypeBegin || msg.MessageType == MsgTypeContinue)

	switch {
	case errors.Is(err, ErrMissingAppContext):
		// Q.773 §3.1.5: P-Abort, abnormal-dialogue
		h.logger.Warn("TCAP message missing application context",
			zap.Stringer("type", msg.MessageType),
			zap.Error(err))
		h.metrics.Errors.Add(1)
		if abortable {
			h.sendAbort(sccpCtx, msg, AbortAbnormalDialogue, true)
			return true
		}

	case errors.Is(err, ErrUnknownAppContext):
		h.logger.Warn("No application handler for context",
			zap.Stringer("type", msg.MessageType),
			zap.Error(err))
		h.metrics.Errors.Add(1)
		if abortable && msg.MessageType == MsgTypeBegin {
			// TS 29.002 §12.1 / Q.773 §4.2.1: refuse the proposed context with
			// a TC-U-ABORT carrying an AARE (reject-permanent, application-
			// context-name-not-supported) that names a context we do support,
			// so the peer can retry with it.
			h.sendACNotSupported(sccpCtx, msg)
			return true
		}
		if abortable {
			h.sendAbort(sccpCtx, msg, AbortNoCommonDialoguePortion, true)
			return true
		}

	default:
		// Handler returned error; dispatcher already counted it. Leave dialogue
		// intact — the application layer chose to fail this message, not abort
		// the dialogue. Operators can wire HandlerErrors metric to their alerts.
		h.logger.Error("Application handler failed",
			zap.Stringer("type", msg.MessageType),
			zap.Error(err))
		h.metrics.Errors.Add(1)
	}

	return false
}

// pinApplicationContext copies the dialogue's stored AC onto msg if msg lacks
// one. Q.773 §3.1.4: AC travels in BEGIN's AARQ; later messages in the same
// dialogue do not re-carry it. The dispatcher routes by msg AC, so we must
// surface the dialogue's pinned AC here.
func pinApplicationContext(dialogue *Dialogue, msg *Message) {
	if dialogue == nil || dialogue.ApplicationContext == nil {
		return
	}
	if msg.DialoguePortion != nil && msg.DialoguePortion.ApplicationContext != nil {
		return
	}
	if msg.DialoguePortion == nil {
		msg.DialoguePortion = &DialoguePortion{}
	}
	msg.DialoguePortion.ApplicationContext = dialogue.ApplicationContext
}

// HandleDeliveryFailure is SCCP's delivery-failure sink. msg.Data is a TCAP
// message this node sent; if it opened or continued a dialogue that is
// still waiting (our TID is its OTID), the dialogue is ended now and the
// caller told why, instead of waiting out tcap.dialogue_timeout.
func (h *Handler) HandleDeliveryFailure(msg *sccp.SccpMessage, reason string) {
	sent, err := h.parseTCAPMessage(msg.Data)
	if err != nil || len(sent.OriginatingTID) < 4 {
		return // END/ABORT we sent: no dialogue of ours is waiting
	}
	localTID := h.decodeTID(sent.OriginatingTID)
	dialogue := h.getDialogue(localTID)
	if dialogue == nil {
		return
	}
	h.removeDialogue(localTID)
	h.metrics.Errors.Add(1)
	h.logger.Warn("Dialogue ended: message not delivered",
		zap.Uint32("local_tid", localTID), zap.String("reason", reason))

	h.reportFailure(dialogue, FailureNotDelivered, reason)
}

// SetVersionFallback enables retrying an originated dialogue under the
// context the peer proposes when it refuses ours. Set before Start.
func (h *Handler) SetVersionFallback(f VersionFallback) {
	h.versionFallback = f
}

// retryUnderLowerVersion handles a TC-U-ABORT whose AARE refuses the
// context of a dialogue we opened and proposes another: if the fallback
// table maps it, the same request is sent again in a new dialogue under the
// proposed context (TS 29.002 §5.2.1: "the initiating user needs to start a
// new dialogue"). Once per request.
func (h *Handler) retryUnderLowerVersion(dialogue *Dialogue, abort *Message) bool {
	if h.versionFallback == nil || !dialogue.initiatedLocally || dialogue.versionFallbackUsed ||
		abort.DialoguePortion == nil || !abort.DialoguePortion.Rejected ||
		abort.DialoguePortion.ApplicationContext == nil || dialogue.ApplicationContext == nil {
		return false
	}
	ac, wireOp, ok := h.versionFallback(dialogue.ApplicationContext.OID, dialogue.OriginatingOpCode,
		abort.DialoguePortion.ApplicationContext.OID)
	if !ok {
		return false
	}

	var parameter []byte
	dialogue.invokesMu.RLock()
	if inv := dialogue.PendingInvokes[1]; inv != nil {
		parameter = inv.Parameter
	}
	dialogue.invokesMu.RUnlock()

	retry, err := h.originate(ac, dialogue.RemoteAddress, dialogue.LocalAddress,
		dialogue.OriginatingOpCode, wireOp, parameter, dialogue.NATSCorrelationID, true)
	if err != nil {
		h.logger.Warn("Version fallback failed to start", zap.Error(err))
		return false
	}
	h.logger.Info("Peer refused the application context: retrying under the version it proposed",
		zap.String("refused", oidString(dialogue.ApplicationContext.OID)),
		zap.String("retry", ac.Name),
		zap.Uint32("new_local_tid", retry.LocalTID))
	return true
}

// SetUserDataLimit tells TCAP how much data one SCCP unitdata can carry
// between two addresses (sccp.Handler.MaxUserData). Set before Start.
func (h *Handler) SetUserDataLimit(limit func(called, calling *sccp.SccpAddress) int) {
	h.maxUserData = limit
}

// deferComponentsIfTooLong turns a BEGIN that would not fit one SCCP
// unitdata into an "empty Begin" (TS 29.002 §C.2.1): it keeps the dialogue
// portion and holds the components back for the first CONTINUE. This is the
// TCAP-level mechanism 3GPP provides for long MAP messages; SCCP
// segmentation (XUDT) is not used because not every transit node carries
// XUDT (§C.1). It fails if even the CONTINUE could not carry them.
func (h *Handler) deferComponentsIfTooLong(dialogue *Dialogue, begin *Message) error {
	limit := maxUnitdataData
	if h.maxUserData != nil {
		limit = h.maxUserData(dialogue.RemoteAddress, dialogue.LocalAddress)
	}
	whole, err := h.encodeTCAPMessage(begin)
	if err != nil {
		return err
	}
	if len(whole) <= limit {
		return nil
	}
	cont, err := h.encodeTCAPMessage(&Message{
		MessageType:    MsgTypeContinue,
		OriginatingTID: begin.OriginatingTID,
		DestinationTID: []byte{0, 0, 0, 0},
		Components:     begin.Components,
	})
	if err != nil {
		return err
	}
	if len(cont) > limit {
		return fmt.Errorf("%w: %d octets of components, one SCCP unitdata carries %d", ErrMessageTooLong, len(cont), limit)
	}
	dialogue.invokesMu.Lock()
	dialogue.deferredComponents = begin.Components
	dialogue.invokesMu.Unlock()
	begin.Components = nil
	h.logger.Info("Message too long for one unitdata: opening with an empty Begin",
		zap.Uint32("local_tid", dialogue.LocalTID), zap.Int("octets", len(whole)), zap.Int("limit", limit))
	return nil
}

// sendDeferredComponents sends what an empty Begin held back, in the first
// CONTINUE after the peer accepted the dialogue.
func (h *Handler) sendDeferredComponents(dialogue *Dialogue) {
	dialogue.invokesMu.Lock()
	comps := dialogue.deferredComponents
	dialogue.deferredComponents = nil
	dialogue.invokesMu.Unlock()
	if len(comps) == 0 {
		return
	}
	dialogue.State = StateActive
	h.handleOutgoingMessage(&Message{
		MessageType:    MsgTypeContinue,
		OriginatingTID: h.encodeTID(dialogue.LocalTID),
		DestinationTID: dialogue.RemoteTID,
		Components:     comps,
		Dialogue:       dialogue,
	})
}

// emptyBeginWait bounds how long a dialogue opened by a peer's empty Begin
// waits for the peer's CONTINUE (one round trip after ours).
const emptyBeginWait = 5 * time.Second

// maxUnitdataData is the most user data one SCCP UDT can carry (the data
// parameter's length indicator is one octet, Q.713 §4.10).
const maxUnitdataData = 255

// answerDialogueRequest attaches the AARE that the first END or CONTINUE of a
// dialogue opened by the peer must carry (Q.773 §4.2.1): the accepted
// application context, echoed. Later messages carry no dialogue portion.
func answerDialogueRequest(dlg *Dialogue, msg *Message) {
	if dlg == nil || msg.DialoguePortion != nil {
		return
	}
	if msg.MessageType != MsgTypeEnd && msg.MessageType != MsgTypeContinue {
		return
	}
	if !dlg.dialogueResponsePending.CompareAndSwap(true, false) {
		return
	}
	msg.DialoguePortion = &DialoguePortion{ApplicationContext: dlg.ApplicationContext}
}

// sendACNotSupported refuses a BEGIN whose application context has no
// handler with a TC-U-ABORT whose AARE names the supported alternative.
func (h *Handler) sendACNotSupported(sccpCtx *sccp.SccpMessage, begin *Message) {
	proposed := begin.DialoguePortion.ApplicationContext.OID
	h.sendAbortMessage(sccpCtx, &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: begin.OriginatingTID,
		DialoguePortion: &DialoguePortion{
			ApplicationContext: &ApplicationContext{OID: h.dispatcher.SupportedAlternative(proposed)},
			Rejected:           true,
		},
	})
}

// sendAbort sends a TCAP ABORT directly back to the originator. Uses the
// incoming SCCP wrapper for addresses (CalledAddress=their CgPA,
// CallingAddress=their CdPA) since a dialogue may not exist (BEGIN rejected,
// unknown DTID). Encodes and pushes synchronously; falls through to error
// metric if the SCCP channel is full.
func (h *Handler) sendAbort(sccpCtx *sccp.SccpMessage, originalMsg *Message, cause AbortCause, pAbort bool) {
	h.sendAbortMessage(sccpCtx, &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: originalMsg.OriginatingTID,
		AbortCause:     &cause,
		PAbort:         pAbort,
	})
}

// sendAbortMessage encodes an ABORT and sends it back along the incoming
// message's addresses (swapped).
func (h *Handler) sendAbortMessage(sccpCtx *sccp.SccpMessage, abortMsg *Message) {
	payload, err := h.encodeTCAPMessage(abortMsg)
	if err != nil {
		h.logger.Error("Failed to encode ABORT", zap.Error(err))
		h.metrics.Errors.Add(1)
		return
	}

	var called, calling *sccp.SccpAddress
	if sccpCtx != nil {
		called = sccpCtx.CallingAddress
		calling = sccpCtx.CalledAddress
	}

	if err := h.sendOnSCCP(payload, called, calling); err != nil {
		h.logger.Error("Failed to send ABORT", zap.Error(err))
		return
	}
	h.metrics.MessagesSent.Add(1)
	h.updateOutgoingMetrics(abortMsg)
}

// allocateTID allocates a new transaction ID (collision-resistant)
func (h *Handler) allocateTID() uint32 {
	// Upper 8 bits: Node ID (0-255)
	// Lower 24 bits: Sequence counter
	seq := h.tidCounter.Add(1) & 0x00FFFFFF
	tid := (uint32(h.nodeID) << 24) | seq

	return tid
}

// decodeTID decodes transaction ID from bytes
func (h *Handler) decodeTID(tidBytes []byte) uint32 {
	if len(tidBytes) == 0 {
		return 0
	}

	// Simple 4-byte decoding (big-endian)
	if len(tidBytes) >= 4 {
		return uint32(tidBytes[0])<<24 |
			uint32(tidBytes[1])<<16 |
			uint32(tidBytes[2])<<8 |
			uint32(tidBytes[3])
	}

	return 0
}

// encodeTID encodes transaction ID to bytes
func (h *Handler) encodeTID(tid uint32) []byte {
	return []byte{
		byte(tid >> 24),
		byte(tid >> 16),
		byte(tid >> 8),
		byte(tid),
	}
}

// getDialogue retrieves dialogue by local TID
func (h *Handler) getDialogue(localTID uint32) *Dialogue {
	h.dialoguesMu.RLock()
	defer h.dialoguesMu.RUnlock()
	return h.dialogues[localTID]
}

// removeDialogue removes dialogue
func (h *Handler) removeDialogue(localTID uint32) {
	h.dialoguesMu.Lock()
	defer h.dialoguesMu.Unlock()

	if dlg, exists := h.dialogues[localTID]; exists {
		delete(h.dialogues, localTID)
		h.metrics.ActiveDialogues.Add(-1)
		if dlg.awaitingContinuation.Swap(false) {
			h.pendingEmptyBegins.Add(-1)
		}
	}
}

// getDialogueCount returns current dialogue count
func (h *Handler) getDialogueCount() int {
	h.dialoguesMu.RLock()
	defer h.dialoguesMu.RUnlock()
	return len(h.dialogues)
}

func oidString(oid []int) string {
	s, err := canonicalOID(oid)
	if err != nil {
		return fmt.Sprint(oid)
	}
	return s
}

// parseTCAPMessage parses a raw TCAP payload (ITU-T Q.773) from the SCCP
// data field (ber.go).
func (h *Handler) parseTCAPMessage(data []byte) (*Message, error) {
	msg, err := ParseMessage(data)
	if err != nil {
		h.logger.Debug("Failed to parse TCAP message", zap.Error(err))
		return nil, fmt.Errorf("TCAP parse error: %w", err)
	}
	return msg, nil
}

// encodeTCAPMessage encodes a TCAP message (ITU-T Q.773, ber.go).
func (h *Handler) encodeTCAPMessage(msg *Message) ([]byte, error) {
	return EncodeMessage(msg)
}

// updateOutgoingMetrics updates metrics for outgoing messages
func (h *Handler) updateOutgoingMetrics(msg *Message) {
	switch msg.MessageType {
	case MsgTypeBegin:
		h.metrics.BeginSent.Add(1)
	case MsgTypeContinue:
		h.metrics.ContinueSent.Add(1)
	case MsgTypeEnd:
		h.metrics.EndSent.Add(1)
	case MsgTypeAbort:
		h.metrics.AbortSent.Add(1)
	}

	h.metrics.ComponentsSent.Add(uint64(len(msg.Components)))
	for _, comp := range msg.Components {
		switch comp.Type {
		case CompTypeInvoke:
			h.metrics.InvokesSent.Add(1)
		case CompTypeReturnResult:
			h.metrics.ResultsSent.Add(1)
		case CompTypeReturnError:
			h.metrics.ErrorsSent.Add(1)
		case CompTypeReject:
			h.metrics.RejectsSent.Add(1)
		}
	}
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	return &HealthStatus{
		ActiveDialogues:    h.metrics.ActiveDialogues.Load(),
		TotalDialogues:     h.metrics.TotalDialogues.Load(),
		MessagesReceived:   h.metrics.MessagesReceived.Load(),
		MessagesSent:       h.metrics.MessagesSent.Load(),
		ComponentsReceived: h.metrics.ComponentsReceived.Load(),
		ComponentsSent:     h.metrics.ComponentsSent.Load(),
		DialogueTimeouts:   h.metrics.DialogueTimeouts.Load(),
		InvokeTimeouts:     h.metrics.InvokeTimeouts.Load(),
		Errors:             h.metrics.Errors.Load(),
		LastUpdate:         time.Now(),
	}
}

// GetDialogues returns all active dialogues
func (h *Handler) GetDialogues() []DialogueInfo {
	h.dialoguesMu.RLock()
	defer h.dialoguesMu.RUnlock()

	dialogues := make([]DialogueInfo, 0, len(h.dialogues))
	for _, d := range h.dialogues {
		d.invokesMu.RLock()
		pendingInvokes := len(d.PendingInvokes)
		d.invokesMu.RUnlock()

		acName := ""
		if d.ApplicationContext != nil {
			acName = d.ApplicationContext.Name
		}

		info := DialogueInfo{
			LocalTID:           d.LocalTID,
			RemoteTID:          hex.EncodeToString(d.RemoteTID),
			State:              d.State.String(),
			ApplicationContext: acName,
			CreatedAt:          d.CreatedAt,
			LastActivity:       d.LastActivity,
			ExpiresAt:          d.ExpiresAt,
			MessagesReceived:   d.MessagesReceived.Load(),
			MessagesSent:       d.MessagesSent.Load(),
			PendingInvokes:     pendingInvokes,
		}
		dialogues = append(dialogues, info)
	}

	return dialogues
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates TCAP configuration
func validateConfig(cfg *Config) error {
	if cfg.DialogueTimeout == 0 {
		cfg.DialogueTimeout = 30 * time.Second
	}
	if cfg.InvokeTimeout == 0 {
		cfg.InvokeTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.MaxDialogues == 0 {
		cfg.MaxDialogues = 10000
	}
	if cfg.MaxComponentsPerMessage == 0 {
		cfg.MaxComponentsPerMessage = 100
	}
	if cfg.MaxInvokesPerDialogue == 0 {
		cfg.MaxInvokesPerDialogue = 10
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = ChannelBufferSize
	}
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if cfg.TimerResolution == 0 {
		cfg.TimerResolution = DefaultTimerResolution
	}
	return nil
}

// String methods for enums
func (mt MessageType) String() string {
	switch mt {
	case MsgTypeUnidirectional:
		return "UNIDIRECTIONAL"
	case MsgTypeBegin:
		return "BEGIN"
	case MsgTypeEnd:
		return "END"
	case MsgTypeContinue:
		return "CONTINUE"
	case MsgTypeAbort:
		return "ABORT"
	default:
		return "UNKNOWN"
	}
}

func (ds DialogueState) String() string {
	switch ds {
	case StateIdle:
		return "IDLE"
	case StateInitWait:
		return "INIT_WAIT"
	case StateActive:
		return "ACTIVE"
	case StateTerminating:
		return "TERMINATING"
	default:
		return "UNKNOWN"
	}
}

// run manages dialogue and invoke timeouts. Owned by Handler.wg so Stop can
// synchronously drain it before logger teardown.
func (tm *TimerManager) run(ctx context.Context) {
	defer tm.handler.wg.Done()
	tm.handler.logger.Info("TCAP timer manager started")
	defer tm.handler.logger.Info("TCAP timer manager stopped")

	ticker := time.NewTicker(tm.handler.config.TimerResolution)
	defer ticker.Stop()

	for {
		select {
		case <-tm.shutdownCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			tm.checkTimeouts()
		}
	}
}

// checkTimeouts checks for expired dialogues and invokes
func (tm *TimerManager) checkTimeouts() {
	now := time.Now()
	h := tm.handler

	h.dialoguesMu.RLock()
	expiredDialogues := make([]uint32, 0)
	expiredInvokes := make(map[uint32][]int32)

	for tid, dlg := range h.dialogues {
		// Check dialogue timeout
		if now.After(dlg.ExpiresAt) {
			expiredDialogues = append(expiredDialogues, tid)
			continue
		}

		// Check invoke timeouts
		dlg.invokesMu.RLock()
		for invID, inv := range dlg.PendingInvokes {
			if now.After(inv.ExpiresAt) {
				if expiredInvokes[tid] == nil {
					expiredInvokes[tid] = make([]int32, 0)
				}
				expiredInvokes[tid] = append(expiredInvokes[tid], invID)
			}
		}
		dlg.invokesMu.RUnlock()
	}
	h.dialoguesMu.RUnlock()

	// Handle expired dialogues
	for _, tid := range expiredDialogues {
		h.logger.Warn("Dialogue timeout",
			zap.Uint32("local_tid", tid))
		h.metrics.DialogueTimeouts.Add(1)
		h.handleDialogueTimeout(tid)
	}

	// Handle expired invokes
	for tid, invIDs := range expiredInvokes {
		for _, invID := range invIDs {
			h.logger.Warn("Invoke timeout",
				zap.Uint32("local_tid", tid),
				zap.Int32("invoke_id", invID))
			h.metrics.InvokeTimeouts.Add(1)
			h.handleInvokeTimeout(tid, invID)
		}
	}
}

// handleDialogueTimeout sends a P-Abort upstream, removes the dialogue, and
// updates metrics. Uses the dialogue's pinned addresses (CalledAddress=Remote,
// CallingAddress=Local) since the dialogue is the only context source here.
//
// For dialogues we originated (NATSCorrelationID set), fires the failure
// sink AFTER the wire ABORT is sent so the upstream NATS waiter unblocks.
// Sink fires post-delete so it can't observe a stale dialogue.
func (h *Handler) handleDialogueTimeout(localTID uint32) {
	h.dialoguesMu.Lock()
	dlg, exists := h.dialogues[localTID]
	if !exists {
		h.dialoguesMu.Unlock()
		return
	}

	cause := AbortResourceLimitation
	abortMsg := &Message{
		MessageType:    MsgTypeAbort,
		DestinationTID: dlg.RemoteTID,
		AbortCause:     &cause,
		PAbort:         true,
	}

	called := dlg.RemoteAddress
	calling := dlg.LocalAddress

	delete(h.dialogues, localTID)
	h.metrics.ActiveDialogues.Add(-1)
	h.dialoguesMu.Unlock()

	if err := h.sendMessage(abortMsg, called, calling); err != nil {
		h.logger.Error("Failed to send timeout Abort",
			zap.Uint32("local_tid", localTID),
			zap.Error(err))
	}

	h.reportFailure(dlg, FailureTimeout, "dialogue timeout")
}

// reportFailure tells the originator of d how its dialogue ended, unless it
// has already had its outcome (MarkAnswered).
func (h *Handler) reportFailure(d *Dialogue, reason FailureReason, detail string) {
	if h.failureSink == nil || d == nil || d.NATSCorrelationID == "" || !d.MarkAnswered() {
		return
	}
	h.failureSink(d.NATSCorrelationID, d.OriginatingOpCode, reason, detail)
}

// handleInvokeTimeout handles invoke timeout
func (h *Handler) handleInvokeTimeout(localTID uint32, invokeID int32) {
	h.dialoguesMu.RLock()
	dlg, exists := h.dialogues[localTID]
	h.dialoguesMu.RUnlock()

	if !exists {
		return
	}

	// Remove pending invoke
	dlg.invokesMu.Lock()
	delete(dlg.PendingInvokes, invokeID)
	dlg.invokesMu.Unlock()

	// Optionally send ReturnError or handle locally
	// For now, just log and update metrics
	h.logger.Debug("Invoke timed out, removed from pending",
		zap.Uint32("local_tid", localTID),
		zap.Int32("invoke_id", invokeID))
}

// sendMessage encodes a TCAP message and pushes it to SCCP with the given
// addresses. Used by the timer manager (timeout aborts) where there is no
// incoming SCCP wrapper to derive addresses from.
func (h *Handler) sendMessage(msg *Message, called, calling *sccp.SccpAddress) error {
	payload, err := h.encodeTCAPMessage(msg)
	if err != nil {
		return err
	}

	if err := h.sendOnSCCP(payload, called, calling); err != nil {
		return err
	}
	h.metrics.MessagesSent.Add(1)
	return nil
}
