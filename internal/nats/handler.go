// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package natsbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

var (
	ErrNotConnected    = errors.New("NATS: not connected")
	ErrInvalidMessage  = errors.New("NATS: invalid message")
	ErrPublishTimeout  = errors.New("NATS: publish timeout")
	ErrSubscribeFailed = errors.New("NATS: subscribe failed")
	ErrShutdown        = errors.New("NATS: handler is shutting down")
)

const (
	DefaultChannelBufferSize = 1000
	DefaultWorkerPoolSize    = 10
	DefaultMessageTimeout    = 30 * time.Second
)

// NewHandler creates a new NATS bridge handler
func NewHandler(config *Config, logger *zap.Logger) (*Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:     config,
		logger:     logger,
		incomingCh: make(chan *NatsMessage, config.ChannelBufferSize),
		outgoingCh: make(chan outgoing, config.ChannelBufferSize),
		errorCh:    make(chan error, 10),
		shutdownCh: make(chan struct{}),
		metrics:    &Metrics{},
	}

	return handler, nil
}

// Start connects to NATS and starts message processing
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("Starting NATS bridge",
		zap.String("server_url", h.config.ServerURL),
		zap.String("node_id", h.config.NodeID))

	// Connect to NATS
	if err := h.connect(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	// Subscribe to inbound subjects
	if err := h.subscribeAll(); err != nil {
		return fmt.Errorf("failed to subscribe to subjects: %w", err)
	}

	// Start worker pool for message processing
	h.workerWg.Add(h.config.WorkerPoolSize + 1)
	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}

	// Start publish worker
	go h.publishWorker(ctx)

	h.logger.Info("NATS bridge started successfully",
		zap.Int("workers", h.config.WorkerPoolSize))

	return nil
}

// Stop gracefully stops the NATS bridge
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping NATS bridge")

	// Signal shutdown
	close(h.shutdownCh)

	// Unsubscribe from all subjects
	for _, sub := range append(h.subscriptions, h.sessionSubs...) {
		if err := sub.Unsubscribe(); err != nil {
			h.logger.Warn("Failed to unsubscribe", zap.Error(err))
		}
	}

	// Drain connection (wait for pending messages)
	if h.nc != nil {
		if err := h.nc.Drain(); err != nil {
			h.logger.Warn("Failed to drain NATS connection", zap.Error(err))
		}
	}

	// Wait for workers to drain. Caller-supplied ctx caps the wait.
	done := make(chan struct{})
	go func() {
		h.workerWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		h.logger.Info("NATS bridge stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("NATS bridge stop timeout — workers still running",
			zap.Error(ctx.Err()))
	}

	// Close connection
	if h.nc != nil {
		h.nc.Close()
	}

	return nil
}

// connect establishes connection to NATS server
func (h *Handler) connect() error {
	opts := []nats.Option{
		nats.Name(fmt.Sprintf("ss7-3eai-gateway-%s", h.config.NodeID)),
		nats.MaxReconnects(h.config.MaxReconnects),
		nats.ReconnectWait(h.config.ReconnectWait),
		nats.Timeout(h.config.ConnectionTimeout),
		nats.PingInterval(h.config.PingInterval),
		nats.ReconnectHandler(h.handleReconnect),
		nats.DisconnectErrHandler(h.handleDisconnect),
		nats.ErrorHandler(h.handleError),
	}

	// Add authentication if configured
	if h.config.Username != "" && h.config.Password != "" {
		opts = append(opts, nats.UserInfo(h.config.Username, h.config.Password))
	} else if h.config.Token != "" {
		opts = append(opts, nats.Token(h.config.Token))
	} else if h.config.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(h.config.CredentialsFile))
	}

	// Add TLS if enabled
	if h.config.TLSEnabled {
		tlsConfig, err := h.loadTLSConfig()
		if err != nil {
			return fmt.Errorf("failed to load TLS config: %w", err)
		}
		opts = append(opts, nats.Secure(tlsConfig))
		h.logger.Info("TLS enabled for NATS connection")
	}

	// Connect
	nc, err := nats.Connect(h.config.ServerURL, opts...)
	if err != nil {
		return fmt.Errorf("NATS connection failed: %w", err)
	}

	h.nc = nc
	h.metrics.ConnectionStatus.Store(1)

	h.logger.Info("Connected to NATS successfully",
		zap.String("server", h.nc.ConnectedUrl()))

	return nil
}

// PauseIntake unsubscribes from all inbound request subjects so no NEW
// externally-originated dialogues (MT-SMS, SRI, CAP/INAP InitialDP) start;
// in-flight workers and the publish path continue. Backs the graceful-drain
// leg of the opsTrigger `drain` action. Idempotent — a second call finds no
// subscriptions and is a no-op.
func (h *Handler) PauseIntake() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var firstErr error
	for _, sub := range h.subscriptions {
		if sub == nil {
			continue
		}
		if err := sub.Unsubscribe(); err != nil {
			h.logger.Warn("PauseIntake: unsubscribe failed", zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	h.subscriptions = nil
	h.logger.Warn("NATS intake paused — unsubscribed from inbound request subjects")
	return firstErr
}

// subscribeAll subscribes to all inbound NATS subjects via a per-cluster
// queue group so multi-node deployments load-balance each request to
// exactly one node. Without the queue group, every node in the cluster
// receives every NATS message and originates a duplicate SS7 dialogue —
// a roamer would receive N copies of the same SMS in an N-node
// deployment. Queue group name = config.ClusterName (validated non-empty
// at construction time); single-node deployments still work because a
// queue group of one behaves identically to a plain subscription.
//
// See nats.io docs: queue subscriptions deliver each message to exactly
// one subscriber per group, hash-distributed by NATS.
func (h *Handler) subscribeAll() error {
	subscriptions := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{SubjectMTSmsRequest, h.handleMTSmsRequest},
		{SubjectSRIRequest, h.handleSRIRequest},
		{SubjectATIRequest, h.handleATIRequest},
		{SubjectPSIRequest, h.handlePSIRequest},
		{SubjectRoutingRequest, h.handleRoutingRequest},
		{SubjectUSSDStart, h.handleUSSDStart},
		{SubjectCLRequest, h.handleCLRequest},
		{SubjectPRNRequest, h.handlePRNRequest},
		{SubjectISDRequest, h.handleISDRequest},
		{SubjectCAPInitialDP, h.handleCAPInitialDP},
		{SubjectINAPInitialDP, h.handleINAPInitialDP},
	}

	for _, sub := range subscriptions {
		subscription, err := h.nc.QueueSubscribe(sub.subject, h.config.ClusterName, sub.handler)
		if err != nil {
			return fmt.Errorf("failed to queue-subscribe to %s: %w", sub.subject, err)
		}
		h.subscriptions = append(h.subscriptions, subscription)
		h.logger.Info("Queue-subscribed to subject",
			zap.String("subject", sub.subject),
			zap.String("queue_group", h.config.ClusterName))
	}

	// A USSD session lives on the node holding its dialogue: every node
	// hears every command and only that one acts (a queue group would hand
	// most commands to the wrong node).
	cmd, err := h.nc.Subscribe(SubjectUSSDCommand, h.handleUSSDCommand)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", SubjectUSSDCommand, err)
	}
	h.sessionSubs = append(h.sessionSubs, cmd)
	h.logger.Info("Subscribed to subject", zap.String("subject", SubjectUSSDCommand))

	// QueueSubscribe only queues the SUB on the client side. Flushing waits for the server's
	// PONG, so once Start returns the server routes to this node: a request published right after
	// start-up is not left to the other members of the queue group (or dropped, with no member).
	timeout := h.config.ConnectionTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := h.nc.FlushTimeout(timeout); err != nil {
		return fmt.Errorf("NATS server did not confirm the subscriptions: %w", err)
	}

	return nil
}

// Message handlers for inbound subjects

func (h *Handler) handleMTSmsRequest(msg *nats.Msg) {
	h.metrics.MTSmsRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onMTSmsRequest)
}

func (h *Handler) handleSRIRequest(msg *nats.Msg) {
	h.metrics.SRIRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onSRIRequest)
}

func (h *Handler) handleATIRequest(msg *nats.Msg) {
	h.metrics.ATIRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onATIRequest)
}

func (h *Handler) handlePSIRequest(msg *nats.Msg) {
	h.metrics.PSIRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onPSIRequest)
}

func (h *Handler) handleRoutingRequest(msg *nats.Msg) {
	h.metrics.RoutingRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onRoutingRequest)
}

func (h *Handler) handleUSSDStart(msg *nats.Msg) {
	h.metrics.USSDStartCount.Add(1)
	h.processIncomingMessage(msg, h.onUSSDStart)
}

// handleUSSDCommand: a command for a session on another node is not this
// node's to answer; the bridge's handler returns nil for it.
func (h *Handler) handleUSSDCommand(msg *nats.Msg) {
	h.metrics.USSDCommandCount.Add(1)
	h.processIncomingMessage(msg, h.onUSSDCommand)
}

func (h *Handler) handleCLRequest(msg *nats.Msg) {
	h.metrics.CLRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onCLRequest)
}

func (h *Handler) handlePRNRequest(msg *nats.Msg) {
	h.metrics.PRNRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onPRNRequest)
}

func (h *Handler) handleISDRequest(msg *nats.Msg) {
	h.metrics.ISDRequestCount.Add(1)
	h.processIncomingMessage(msg, h.onISDRequest)
}

func (h *Handler) handleCAPInitialDP(msg *nats.Msg) {
	h.metrics.CAPInitialDPCount.Add(1)
	h.processIncomingMessage(msg, h.onCAPInitialDP)
}

func (h *Handler) handleINAPInitialDP(msg *nats.Msg) {
	h.metrics.INAPInitialDPCount.Add(1)
	h.processIncomingMessage(msg, h.onINAPInitialDP)
}

// processIncomingMessage deserializes and forwards message to handler
func (h *Handler) processIncomingMessage(msg *nats.Msg, handler func(*NatsMessage) error) {
	h.metrics.MessagesReceived.Add(1)
	h.metrics.BytesReceived.Add(uint64(len(msg.Data)))

	// Deserialize message
	var natsMsg NatsMessage
	if err := json.Unmarshal(msg.Data, &natsMsg); err != nil {
		h.logger.Error("Failed to deserialize NATS message",
			zap.Error(err),
			zap.String("subject", msg.Subject))
		h.metrics.ProcessingErrors.Add(1)
		return
	}

	h.logger.Debug("Received NATS message",
		zap.String("message_id", natsMsg.MessageID),
		zap.String("type", string(natsMsg.MessageType)),
		zap.String("subject", msg.Subject))

	// Call handler if registered
	if handler != nil {
		if err := handler(&natsMsg); err != nil {
			h.logger.Error("Handler failed to process message",
				zap.Error(err),
				zap.String("message_id", natsMsg.MessageID))
			h.metrics.ProcessingErrors.Add(1)
			h.publishRejection(msg.Subject, &natsMsg, err)
		}
	} else {
		// No handler registered, queue for application
		select {
		case h.incomingCh <- &natsMsg:
			// Message queued
		default:
			h.logger.Error("Incoming channel full, dropping message",
				zap.String("message_id", natsMsg.MessageID))
			h.metrics.ProcessingErrors.Add(1)
		}
	}
}

// responseTypeBySubject is the response message type of each request subject
// of the contract.
var responseTypeBySubject = map[string]MessageType{
	SubjectSRIRequest:     MsgTypeMAPSRI,
	SubjectMTSmsRequest:   MsgTypeMAPMTSms,
	SubjectATIRequest:     MsgTypeMAPATI,
	SubjectPSIRequest:     MsgTypeMAPPSI,
	SubjectRoutingRequest: MsgTypeMAPRouting,
	SubjectUSSDStart:      MsgTypeMAPUSSD,
	SubjectUSSDCommand:    MsgTypeMAPUSSD,
	SubjectCLRequest:      MsgTypeMAPCL,
	SubjectPRNRequest:     MsgTypeMAPPRN,
	SubjectISDRequest:     MsgTypeMAPISD,
}

// publishRejection answers a request refused before a dialogue started —
// invalid, or no capacity for it — with StatusRejected on its response
// subject, so the caller does not wait out its deadline.
func (h *Handler) publishRejection(subject string, req *NatsMessage, reason error) {
	msgType, ok := responseTypeBySubject[subject]
	if !ok {
		return
	}
	corrID := req.CorrelationID
	if corrID == "" {
		corrID = req.MessageID
	}
	if corrID == "" {
		return // nothing the caller could match an answer by
	}
	if err := h.PublishFailure(msgType, corrID, ss7client.StatusRejected, ss7client.ErrCodeRejected, reason.Error()); err != nil {
		h.logger.Error("publish rejection", zap.String("correlation_id", corrID), zap.Error(err))
	}
}

// worker processes messages from channels
func (h *Handler) worker(ctx context.Context, id int) {
	defer h.workerWg.Done()
	h.logger.Debug("NATS worker started", zap.Int("worker_id", id))
	defer h.logger.Debug("NATS worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case msg := <-h.incomingCh:
			// Process message from incoming queue
			h.logger.Debug("Worker processing message",
				zap.Int("worker_id", id),
				zap.String("message_id", msg.MessageID))
		}
	}
}

// publishWorker handles outgoing message publishing
func (h *Handler) publishWorker(ctx context.Context) {
	defer h.workerWg.Done()
	h.logger.Debug("NATS publish worker started")
	defer h.logger.Debug("NATS publish worker stopped")

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case out := <-h.outgoingCh:
			if err := h.publishMessage(out.msg, out.subject); err != nil {
				h.logger.Error("Failed to publish message",
					zap.Error(err),
					zap.String("message_id", out.msg.MessageID))
			}
		}
	}
}

// publishMessage publishes a message to NATS, on subject or, when that is
// empty, on its message type's.
func (h *Handler) publishMessage(msg *NatsMessage, subject string) error {
	if h.nc == nil || !h.nc.IsConnected() {
		return ErrNotConnected
	}

	if subject == "" {
		subject = h.getSubjectForMessage(msg)
	}
	if subject == "" {
		return fmt.Errorf("unknown message type: %s", msg.MessageType)
	}

	// Serialize message
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	// Track latency
	startTime := time.Now()

	// Publish to NATS
	if err := h.nc.Publish(subject, data); err != nil {
		h.metrics.PublishErrors.Add(1)
		return fmt.Errorf("NATS publish failed: %w", err)
	}

	// Update metrics
	latency := time.Since(startTime)
	h.metrics.MessagesPublished.Add(1)
	h.metrics.BytesPublished.Add(uint64(len(data)))
	h.metrics.PublishLatencySum.Add(uint64(latency.Nanoseconds()))
	h.metrics.PublishLatencyCount.Add(1)

	h.logger.Debug("Published message to NATS",
		zap.String("message_id", msg.MessageID),
		zap.String("subject", subject),
		zap.Duration("latency", latency))

	return nil
}

// getSubjectForMessage returns NATS subject for message type
func (h *Handler) getSubjectForMessage(msg *NatsMessage) string {
	switch msg.MessageType {
	case MsgTypeMAPMOSms:
		return SubjectMOSms
	case MsgTypeMAPMTSms:
		return SubjectMTSmsResponse
	case MsgTypeMAPSRI:
		return SubjectSRIResponse
	case MsgTypeMAPATI:
		return SubjectATIResponse
	case MsgTypeMAPPSI:
		return SubjectPSIResponse
	case MsgTypeMAPRouting:
		return SubjectRoutingResponse
	case MsgTypeMAPUSSD:
		return SubjectUSSDResponse
	case MsgTypeMAPCL:
		return SubjectCLResponse
	case MsgTypeMAPPRN:
		return SubjectPRNResponse
	case MsgTypeMAPISD:
		return SubjectISDResponse
	case MsgTypeCAPEventReport:
		return SubjectCAPEventReport
	case MsgTypeINAPEventReport:
		return SubjectINAPEventReport
	default:
		return ""
	}
}

// Public API methods

// PublishMOSms publishes MO SMS to NATS
func (h *Handler) PublishMOSms(sender, recipient, content string) error {
	msg := &NatsMessage{
		MessageID:   uuid.New().String(),
		MessageType: MsgTypeMAPMOSms,
		Direction:   DirectionOutbound,
		Timestamp:   time.Now(),
		Sender:      sender,
		Recipient:   recipient,
		Content:     content,
		NodeID:      h.config.NodeID,
	}

	if err := h.enqueue(msg); err != nil {
		return err
	}
	h.metrics.MOSmsPublishCount.Add(1)
	return nil
}

// PublishMOForwardSM publishes an inbound MAP MO-Forward-SM (op 46) to
// NATS. Driven by the gateway-side `MoForwardSMHandler` wired in
// `cmd/gateway/nats_bridge.go`. Direction follows the codebase
// convention (Outbound = "from SS7, going out to NATS"; Inbound =
// "from NATS, going in to SS7") — see `internal/nats/types.go`.
// NodeID is the cluster-member
// identifier (h.config.NodeID), critical for cluster-distribution
// attribution in tests AND for SMSC-side per-source-node billing /
// auditing in production.
//
// Field mapping per 3GPP TS 29.002 §17.7.4:
//
//   - Sender    = SM-RP-OA digits (originating MSC GT in MO-Fwd-SM Arg).
//     Foreign network's MSC asserts itself here.
//   - Recipient = SM-RP-DA digits (destination SMSC GT). In v1 cluster
//     deployments this equals our cluster_global_title.address
//     — the foreign network sees one SMSC GT regardless of
//     which cluster node loadshare picked.
//   - IMSI      = optional originator IMSI (post-Rel-99 IMSI extension);
//     may be empty when the originating network omits it.
//   - PayloadData = TPDU bytes (SM-RP-UI), opaque per TS 23.040 — the
//     SMSC consumer parses TP-User-Data, TP-VP, etc.
//
// Returns "outgoing channel full" when the NATS publish-worker queue is
// saturated; caller (MAP handler) treats as transient and reports
// systemFailure to the foreign MSC, which will retry per its own T-R.
func (h *Handler) PublishMOForwardSM(correlationID, mscGT, smscGT, imsi string, tpdu []byte) error {
	msg := &NatsMessage{
		MessageID:     uuid.New().String(),
		CorrelationID: correlationID,
		MessageType:   MsgTypeMAPMOSms,
		Direction:     DirectionOutbound, // SS7 → NATS per codebase convention
		Timestamp:     time.Now(),
		Sender:        mscGT,
		Recipient:     smscGT,
		IMSI:          imsi,
		PayloadData:   tpdu,
		NodeID:        h.config.NodeID,
	}

	if err := h.enqueue(msg); err != nil {
		return err
	}
	h.metrics.MOSmsPublishCount.Add(1)
	return nil
}

// PublishMTSmsResponse publishes MT SMS response to NATS
func (h *Handler) PublishMTSmsResponse(correlationID, status string, errorCode int32) error {
	msg := &NatsMessage{
		MessageID:     uuid.New().String(),
		CorrelationID: correlationID,
		MessageType:   MsgTypeMAPMTSms,
		Direction:     DirectionOutbound,
		Timestamp:     time.Now(),
		Status:        status,
		ErrorCode:     errorCode,
		NodeID:        h.config.NodeID,
	}

	return h.enqueue(msg)
}

// PublishSRIResponse publishes SendRoutingInfo response to NATS
func (h *Handler) PublishSRIResponse(correlationID, msisdn, imsi string, vmscAddress string) error {
	msg := &NatsMessage{
		MessageID:     uuid.New().String(),
		CorrelationID: correlationID,
		MessageType:   MsgTypeMAPSRI,
		Direction:     DirectionOutbound,
		Timestamp:     time.Now(),
		MSISDN:        msisdn,
		IMSI:          imsi,
		NodeID:        h.config.NodeID,
		Parameters: map[string]interface{}{
			ss7client.ParamVMSCAddress: vmscAddress,
		},
	}

	return h.enqueue(msg)
}

// PublishFailure publishes the failure of a request on the response subject
// of msgType: a refused request, a timed-out or aborted dialogue, or the
// peer's error.
func (h *Handler) PublishFailure(msgType MessageType, correlationID, status string, errorCode int32, errMsg string) error {
	return h.enqueue(&NatsMessage{
		MessageID:     uuid.New().String(),
		CorrelationID: correlationID,
		MessageType:   msgType,
		Direction:     DirectionOutbound,
		Timestamp:     time.Now(),
		Status:        status,
		ErrorCode:     errorCode,
		ErrorMessage:  errMsg,
		NodeID:        h.config.NodeID,
	})
}

// PublishSubscriberInfo publishes a successful AnyTimeInterrogation
// (MsgTypeMAPATI) or ProvideSubscriberInfo (MsgTypeMAPPSI) answer.
func (h *Handler) PublishSubscriberInfo(msgType MessageType, correlationID string, info *ss7client.SubscriberInfo) error {
	return h.enqueue(&NatsMessage{
		MessageID:      uuid.New().String(),
		CorrelationID:  correlationID,
		MessageType:    msgType,
		Direction:      DirectionOutbound,
		Timestamp:      time.Now(),
		SubscriberInfo: info,
		NodeID:         h.config.NodeID,
	})
}

// PublishRoutingInfo publishes a successful SendRoutingInfo answer: the
// IMSI, the routing information and, when the HLR returned it, the
// subscriber's location and state.
func (h *Handler) PublishRoutingInfo(correlationID, imsi string, routing *ss7client.RoutingInfo, info *ss7client.SubscriberInfo) error {
	return h.enqueue(&NatsMessage{
		MessageID:      uuid.New().String(),
		CorrelationID:  correlationID,
		MessageType:    MsgTypeMAPRouting,
		Direction:      DirectionOutbound,
		Timestamp:      time.Now(),
		IMSI:           imsi,
		RoutingInfo:    routing,
		SubscriberInfo: info,
		NodeID:         h.config.NodeID,
	})
}

// PublishUSSDResponse answers a USSD start or command that succeeded.
func (h *Handler) PublishUSSDResponse(correlationID string, u *ss7client.USSD) error {
	return h.enqueue(&NatsMessage{
		MessageID:     uuid.New().String(),
		CorrelationID: correlationID,
		MessageType:   MsgTypeMAPUSSD,
		Direction:     DirectionOutbound,
		Timestamp:     time.Now(),
		USSD:          u,
		NodeID:        h.config.NodeID,
	})
}

// PublishUSSDEvent publishes a session's begin or end on its route's
// subject. msg carries the subscriber and, for an end, the status; the
// envelope is filled in here.
func (h *Handler) PublishUSSDEvent(route string, msg *NatsMessage) error {
	msg.MessageID = uuid.New().String()
	msg.MessageType = MsgTypeMAPUSSD
	msg.Direction = DirectionOutbound
	msg.Timestamp = time.Now()
	msg.NodeID = h.config.NodeID
	return h.enqueueOn(SubjectUSSDEventPrefix+route, msg)
}

// PublishAnswer publishes a successful answer on its message type's
// response subject; the envelope is filled in here.
func (h *Handler) PublishAnswer(msg *NatsMessage) error {
	msg.MessageID = uuid.New().String()
	msg.Direction = DirectionOutbound
	msg.Timestamp = time.Now()
	msg.NodeID = h.config.NodeID
	return h.enqueue(msg)
}

// ErrNoResponder: no application serves the subject asked.
var ErrNoResponder = errors.New("no application is serving the subject")

// RequestLocationUpdate asks the HLR application about a location update
// and waits for its answer until ctx ends. It blocks: call it off the
// TCAP workers.
func (h *Handler) RequestLocationUpdate(ctx context.Context, msg *NatsMessage) (*NatsMessage, error) {
	h.fillTurn(msg)
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	nc := h.nc
	if nc == nil {
		return nil, ErrNotConnected
	}
	h.metrics.LocationUpdateTurns.Add(1)
	reply, err := nc.RequestWithContext(ctx, SubjectLocationUpdate, data)
	if errors.Is(err, nats.ErrNoResponders) {
		return nil, ErrNoResponder
	}
	if err != nil {
		return nil, err
	}
	var answer NatsMessage
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		return nil, fmt.Errorf("the answer is not a message: %w", err)
	}
	return &answer, nil
}

// PublishLocationUpdateEnd tells the HLR application a location update
// ended without its answer: a publish, not a request — it is not answered.
func (h *Handler) PublishLocationUpdateEnd(msg *NatsMessage) error {
	h.fillTurn(msg)
	return h.enqueueOn(SubjectLocationUpdate, msg)
}

func (h *Handler) fillTurn(msg *NatsMessage) {
	msg.MessageID = uuid.New().String()
	msg.MessageType = MsgTypeMAPUL
	msg.Direction = DirectionOutbound
	msg.Timestamp = time.Now()
	msg.NodeID = h.config.NodeID
}

// NodeID is this node's identity (nats.node_id).
func (h *Handler) NodeID() string { return h.config.NodeID }

func (h *Handler) enqueue(msg *NatsMessage) error {
	return h.enqueueOn("", msg)
}

// enqueueOn queues msg for subject ("": its message type's). One publish
// worker takes the queue in order, so a session's messages reach NATS in
// the order they were queued.
func (h *Handler) enqueueOn(subject string, msg *NatsMessage) error {
	select {
	case h.outgoingCh <- outgoing{msg: msg, subject: subject}:
		return nil
	default:
		return fmt.Errorf("outgoing channel full")
	}
}

// Handler registration methods

func (h *Handler) OnMTSmsRequest(handler func(*NatsMessage) error) {
	h.onMTSmsRequest = handler
}

func (h *Handler) OnSRIRequest(handler func(*NatsMessage) error) {
	h.onSRIRequest = handler
}

func (h *Handler) OnATIRequest(handler func(*NatsMessage) error) {
	h.onATIRequest = handler
}

func (h *Handler) OnPSIRequest(handler func(*NatsMessage) error) {
	h.onPSIRequest = handler
}

func (h *Handler) OnRoutingRequest(handler func(*NatsMessage) error) {
	h.onRoutingRequest = handler
}

func (h *Handler) OnUSSDStart(handler func(*NatsMessage) error) {
	h.onUSSDStart = handler
}

func (h *Handler) OnUSSDCommand(handler func(*NatsMessage) error) {
	h.onUSSDCommand = handler
}

func (h *Handler) OnCLRequest(handler func(*NatsMessage) error)  { h.onCLRequest = handler }
func (h *Handler) OnPRNRequest(handler func(*NatsMessage) error) { h.onPRNRequest = handler }
func (h *Handler) OnISDRequest(handler func(*NatsMessage) error) { h.onISDRequest = handler }

func (h *Handler) OnCAPInitialDP(handler func(*NatsMessage) error) {
	h.onCAPInitialDP = handler
}

func (h *Handler) OnINAPInitialDP(handler func(*NatsMessage) error) {
	h.onINAPInitialDP = handler
}

// Reconnect and error handlers

func (h *Handler) handleReconnect(nc *nats.Conn) {
	h.metrics.ReconnectCount.Add(1)
	h.metrics.mu.Lock()
	h.metrics.LastReconnect = time.Now()
	h.metrics.mu.Unlock()

	h.logger.Warn("NATS reconnected",
		zap.String("server", nc.ConnectedUrl()),
		zap.Uint64("reconnect_count", h.metrics.ReconnectCount.Load()))
}

func (h *Handler) handleDisconnect(nc *nats.Conn, err error) {
	h.metrics.ConnectionStatus.Store(0)

	if err != nil {
		h.logger.Error("NATS disconnected",
			zap.Error(err),
			zap.String("server", nc.ConnectedUrl()))
	} else {
		h.logger.Warn("NATS disconnected gracefully")
	}
}

func (h *Handler) handleError(nc *nats.Conn, sub *nats.Subscription, err error) {
	h.logger.Error("NATS error",
		zap.Error(err),
		zap.String("subject", sub.Subject))
}

// Metrics and health

func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

func (h *Handler) GetHealthStatus() *HealthStatus {
	avgLatency := float64(0)
	if count := h.metrics.PublishLatencyCount.Load(); count > 0 {
		sum := h.metrics.PublishLatencySum.Load()
		avgLatency = float64(sum) / float64(count) / 1e6 // Convert to ms
	}

	return &HealthStatus{
		Connected:         h.nc != nil && h.nc.IsConnected(),
		ServerURL:         h.config.ServerURL,
		NodeID:            h.config.NodeID,
		ReconnectCount:    h.metrics.ReconnectCount.Load(),
		MessagesPublished: h.metrics.MessagesPublished.Load(),
		MessagesReceived:  h.metrics.MessagesReceived.Load(),
		PublishErrors:     h.metrics.PublishErrors.Load(),
		AverageLatencyMs:  avgLatency,
		LastUpdate:        time.Now(),
	}
}

// IncomingMessages returns channel for incoming messages
func (h *Handler) IncomingMessages() <-chan *NatsMessage {
	return h.incomingCh
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates NATS bridge configuration
func validateConfig(cfg *Config) error {
	if cfg.ServerURL == "" {
		return errors.New("server_url is required")
	}
	if cfg.NodeID == "" {
		return errors.New("node_id is required")
	}
	if cfg.ClusterName == "" {
		// Required because subscribeAll uses ClusterName as the NATS
		// queue group. An empty queue group degrades to plain Subscribe,
		// which causes every cluster node to receive every request and
		// originate duplicate SS7 dialogues — operator gets billed N
		// times per SMS. Fail-fast at construction so this can never
		// silently happen in production.
		return errors.New("cluster_name is required (used as NATS queue group for multi-node load-balancing)")
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = DefaultChannelBufferSize
	}
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if cfg.MessageTimeout == 0 {
		cfg.MessageTimeout = DefaultMessageTimeout
	}
	if cfg.MaxReconnects == 0 {
		cfg.MaxReconnects = -1 // Infinite reconnects
	}
	if cfg.ReconnectWait == 0 {
		cfg.ReconnectWait = 1 * time.Second
	}
	if cfg.ConnectionTimeout == 0 {
		cfg.ConnectionTimeout = 2 * time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 2 * time.Minute
	}
	return nil
}

// Connection returns the underlying NATS connection
// Used for JetStream KV access
func (h *Handler) Connection() *nats.Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.nc
}

// loadTLSConfig loads TLS certificates and returns configured tls.Config
func (h *Handler) loadTLSConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load client certificate if provided
	if h.config.TLSCertFile != "" && h.config.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(h.config.TLSCertFile, h.config.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		h.logger.Info("Loaded client TLS certificate",
			zap.String("cert", h.config.TLSCertFile))
	}

	// Load CA certificate if provided
	if h.config.TLSCACertFile != "" {
		caCert, err := os.ReadFile(h.config.TLSCACertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}

		tlsConfig.RootCAs = caCertPool
		h.logger.Info("Loaded CA certificate",
			zap.String("ca_cert", h.config.TLSCACertFile))
	}

	return tlsConfig, nil
}
