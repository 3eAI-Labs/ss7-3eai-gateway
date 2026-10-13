// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package m3ua

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ishidawataru/sctp"
	"github.com/wmnsk/go-m3ua"
	"go.uber.org/zap"
)

var (
	ErrNotConnected    = errors.New("M3UA: not connected to SGW")
	ErrNotActive       = errors.New("M3UA: link not in ACTIVE state")
	ErrInvalidConfig   = errors.New("M3UA: invalid configuration")
	ErrTimeout         = errors.New("M3UA: operation timeout")
	ErrSCTPFailure     = errors.New("M3UA: SCTP connection failure")
	ErrMessageTooLarge = errors.New("M3UA: message exceeds MTU")
	ErrShutdown        = errors.New("M3UA: handler is shutting down")

	ErrInvalidRoutingLabel = errors.New("M3UA: invalid routing label")
)

const (
	MaxM3UAMessageSize = 2048 // MTU for SCTP
	ChannelBufferSize  = 100

	// DefaultReconnectInterval applies when reconnect_interval is unset.
	DefaultReconnectInterval = 5 * time.Second
)

// NewHandler creates a new M3UA handler instance
func NewHandler(config *Config, logger *zap.Logger) (*Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config cannot be nil", ErrInvalidConfig)
	}
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	if config.ReconnectInterval <= 0 {
		config.ReconnectInterval = DefaultReconnectInterval
	}

	handler := &Handler{
		config:         config,
		state:          ASPStateDown,
		incomingCh:     make(chan *MTP3Message, ChannelBufferSize),
		outgoingCh:     make(chan *MTP3Message, ChannelBufferSize),
		errorCh:        make(chan error, 10),
		shutdownCh:     make(chan struct{}),
		supervisorDone: make(chan struct{}),
		metrics:        &Metrics{},
		logger:         logger,
	}

	return handler, nil
}

// Start launches the association supervisor and returns at once. The
// supervisor dials the SGW, keeps the association up, and re-dials after
// every loss until Stop or ctx ends. An unreachable SGW at boot is therefore
// not an error: the link comes up when the SGW does, and IsActive reports
// it meanwhile (the readiness probe follows it).
func (h *Handler) Start(ctx context.Context) error {
	if !h.started.CompareAndSwap(false, true) {
		return errors.New("handler already started")
	}

	h.logger.Info("Starting M3UA handler",
		zap.String("sgw", h.config.SGWAddress),
		zap.Int("port", h.config.SGWPort),
		zap.Uint32("asp_id", h.config.ASPIdentifier))

	go h.writeLoop(ctx)
	go h.stateManager(ctx)
	go h.supervise(ctx)
	return nil
}

// Stop gracefully stops the M3UA handler
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping M3UA handler")

	// Signal shutdown
	h.stopOnce.Do(func() { close(h.shutdownCh) })
	h.closeConnection()

	select {
	case <-h.supervisorDone:
		h.logger.Info("M3UA handler stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("M3UA handler stop timeout")
	}
	return nil
}

// connectSCTP establishes SCTP connection with multi-homing (BR-M3UA-003)
func (h *Handler) connectSCTP(ctx context.Context) error {
	// Parse local IPs for multi-homing
	var localAddrs []net.IPAddr
	for _, ipStr := range h.config.LocalIPs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return fmt.Errorf("invalid local IP: %s", ipStr)
		}
		localAddrs = append(localAddrs, net.IPAddr{IP: ip})
	}

	// Parse remote address
	remoteAddr := &sctp.SCTPAddr{
		IPAddrs: []net.IPAddr{{IP: net.ParseIP(h.config.SGWAddress)}},
		Port:    h.config.SGWPort,
	}

	// Local address for binding
	var localAddr *sctp.SCTPAddr
	if len(localAddrs) > 0 {
		localAddr = &sctp.SCTPAddr{IPAddrs: localAddrs}
	}

	h.logger.Info("Connecting to SGW via M3UA",
		zap.Strings("local_ips", h.config.LocalIPs),
		zap.String("remote", remoteAddr.String()))

	// Create M3UA config. The routing-label arguments are unused: every
	// DATA is written with its own label (sendMTP3 → WritePDToStream), so
	// they are zero rather than plausible-looking placeholders.
	m3uaConfig := m3ua.NewConfig(0, 0, 0, 0, 0, 0).SetAspIdentifier(h.config.ASPIdentifier).
		SetTrafficModeType(h.config.TrafficMode).
		SetRoutingContexts(h.config.RoutingContext).
		SetNetworkAppearance(h.config.NetworkAppearance).
		EnableHeartbeat(h.config.HeartbeatInterval, 30*time.Second)

	// The context handed to Dial is the association's lifetime: go-m3ua's
	// monitor closes the connection when it ends. It is cancelled only by
	// closeConnection, never by the dial's own completion.
	connCtx, connCancel := context.WithCancel(ctx)
	conn, err := m3ua.Dial(connCtx, "m3ua", localAddr, remoteAddr, m3uaConfig)
	if err != nil {
		connCancel()
		h.metrics.Errors.Add(1)
		return fmt.Errorf("M3UA dial failed: %w", err)
	}

	h.connMu.Lock()
	h.conn = conn
	h.connCancel = connCancel
	h.connMu.Unlock()

	h.logger.Info("M3UA connection established",
		zap.String("local", conn.LocalAddr().String()),
		zap.String("remote", conn.RemoteAddr().String()))

	h.metrics.ConnectionAttempts.Add(1)
	h.metrics.mu.Lock()
	if len(h.config.LocalIPs) > 0 {
		h.metrics.ActivePath = h.config.LocalIPs[0] // Primary path
	}
	h.metrics.mu.Unlock()

	return nil
}

// setState changes ASP state (thread-safe)
func (h *Handler) setState(newState ASPState) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	h.state = newState
}

// GetState returns current ASP state (thread-safe)
func (h *Handler) GetState() ASPState {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	return h.state
}

// transitionState handles state transition logic
func (h *Handler) transitionState(oldState, newState ASPState) {
	h.logger.Info("State transition",
		zap.String("from", oldState.String()),
		zap.String("to", newState.String()))

	h.metrics.StateTransitions.Add(1)
	h.metrics.mu.Lock()
	h.metrics.LastStateChange = time.Now()
	h.metrics.mu.Unlock()

	// Trigger callback
	if h.onStateChange != nil {
		h.onStateChange(oldState, newState)
	}
}

// ResetAssociation drops the current SCTP/M3UA association so the
// supervisor re-establishes it. Closing the connection makes the active
// readLoop's ReadPD return, and the supervisor re-dials — exactly the path a
// real link flap takes. Backs the
// opsTrigger `resetPeer` action; idempotent (a no-op when the link is not
// active). Operator-gated (x-arinna requiresApproval) so it is not invoked at
// high frequency.
func (h *Handler) ResetAssociation() error {
	if !h.IsActive() {
		h.logger.Info("M3UA ResetAssociation: link not active, no-op")
		return nil
	}
	h.logger.Warn("M3UA ResetAssociation: dropping association to force reconnect")
	h.closeConnection()
	return nil
}

// closeConnection closes the current association, if any, and ends its
// context. Safe to call concurrently and repeatedly.
func (h *Handler) closeConnection() {
	h.connMu.Lock()
	conn, cancel := h.conn, h.connCancel
	h.conn, h.connCancel = nil, nil
	h.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if cancel != nil {
		cancel()
	}
}

// currentConn returns the live association, or nil between associations.
func (h *Handler) currentConn() *m3ua.Conn {
	h.connMu.RLock()
	defer h.connMu.RUnlock()
	return h.conn
}

// validateConfig validates M3UA configuration
func validateConfig(cfg *Config) error {
	if cfg.SGWAddress == "" {
		return errors.New("sgw_address is required")
	}
	if cfg.SGWPort < 1 || cfg.SGWPort > 65535 {
		return errors.New("sgw_port must be between 1 and 65535")
	}
	if cfg.NetworkIndicator > 3 {
		return errors.New("network_indicator must be 0 (international), 1, 2 (national) or 3")
	}
	if cfg.TrafficMode != 1 && cfg.TrafficMode != 2 {
		return errors.New("traffic_mode must be 1 (Override) or 2 (Loadshare)")
	}
	if cfg.MultiHomingEnabled && len(cfg.LocalIPs) < 2 {
		return errors.New("multi-homing requires at least 2 local IPs")
	}
	if cfg.ASPUPACKTimeout < 1*time.Second {
		return errors.New("aspup_ack_timeout must be at least 1 second")
	}
	return nil
}

// IncomingMessages returns channel for incoming MTP3 messages from the SGW.
// Each message carries OPC/DPC/SI/NI/MP/SLS plus the SCCP user data (PayloadData).
func (h *Handler) IncomingMessages() <-chan *MTP3Message {
	return h.incomingCh
}

// OutgoingMessages returns channel for outgoing MTP3 messages
func (h *Handler) OutgoingMessages() chan<- *MTP3Message {
	return h.outgoingCh
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	h.metrics.mu.RLock()
	defer h.metrics.mu.RUnlock()

	linkStatus := "DOWN"
	if h.metrics.LinkStatus.Load() == 1 {
		linkStatus = "UP"
	}

	return &HealthStatus{
		State:        h.GetState().String(),
		LinkStatus:   linkStatus,
		ActivePath:   h.metrics.ActivePath,
		MessagesSent: h.metrics.MessagesSent.Load(),
		MessagesRecv: h.metrics.MessagesReceived.Load(),
		Errors:       h.metrics.Errors.Load(),
		LastStateChg: h.metrics.LastStateChange,
	}
}

// OnStateChange sets callback for state changes
func (h *Handler) OnStateChange(callback func(oldState, newState ASPState)) {
	h.onStateChange = callback
}

// OnLinkUp sets callback for link up events
func (h *Handler) OnLinkUp(callback func()) {
	h.onLinkUp = callback
}

// OnLinkDown sets callback for link down events
func (h *Handler) OnLinkDown(callback func()) {
	h.onLinkDown = callback
}
