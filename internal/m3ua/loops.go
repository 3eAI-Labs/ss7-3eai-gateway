// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package m3ua

import (
	"context"
	"fmt"
	"time"

	gom3ua "github.com/wmnsk/go-m3ua"
	"github.com/wmnsk/go-m3ua/messages/params"
	"go.uber.org/zap"
)

// stateManager monitors connection state (go-m3ua library handles state transitions automatically)
func (h *Handler) stateManager(ctx context.Context) {
	h.logger.Info("State manager started")
	defer h.logger.Info("State manager stopped")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Monitor connection health
			if h.currentConn() != nil {
				// Log current state periodically
				h.logger.Debug("M3UA connection status",
					zap.String("state", h.GetState().String()),
					zap.Uint64("messages_sent", h.metrics.MessagesSent.Load()),
					zap.Uint64("messages_received", h.metrics.MessagesReceived.Load()))
			}
		}
	}
}

// supervise owns the association: dial, serve until it is lost, wait
// ReconnectInterval, dial again. It ends on Stop or ctx, or after
// ReconnectMaxAttempts consecutive failed dials when that is non-zero.
func (h *Handler) supervise(ctx context.Context) {
	defer close(h.supervisorDone)

	failures := 0
	for {
		if h.stopping(ctx) {
			return
		}
		if err := h.connectSCTP(ctx); err != nil {
			failures++
			h.logger.Warn("M3UA association not established",
				zap.Error(err), zap.Int("consecutive_failures", failures))
			if max := h.config.ReconnectMaxAttempts; max > 0 && failures >= max {
				h.logger.Error("M3UA reconnect attempts exhausted, giving up",
					zap.Int("attempts", failures))
				return
			}
			if !h.wait(ctx, h.config.ReconnectInterval) {
				return
			}
			continue
		}
		failures = 0

		h.linkUp()
		h.readLoop(ctx, h.currentConn())
		h.linkDown()
		h.closeConnection()

		if !h.wait(ctx, h.config.ReconnectInterval) {
			return
		}
	}
}

// stopping reports whether Stop was called or ctx ended.
func (h *Handler) stopping(ctx context.Context) bool {
	select {
	case <-h.shutdownCh:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// wait sleeps for d unless shutdown comes first; it reports whether to go on.
func (h *Handler) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-h.shutdownCh:
		return false
	case <-ctx.Done():
		return false
	}
}

func (h *Handler) linkUp() {
	old := h.GetState()
	h.setState(ASPStateActive)
	h.metrics.LinkStatus.Store(1)
	h.transitionState(old, ASPStateActive)
	if h.onLinkUp != nil {
		h.onLinkUp()
	}
	h.logger.Info("M3UA association active")
}

func (h *Handler) linkDown() {
	old := h.GetState()
	h.setState(ASPStateDown)
	h.metrics.LinkStatus.Store(0)
	h.transitionState(old, ASPStateDown)
	if h.onLinkDown != nil {
		h.onLinkDown()
	}
}

// readLoop reads M3UA DATA from one association and forwards it to SCCP
// over h.incomingCh. It returns when the association is lost or closed, or
// on shutdown; the supervisor decides what happens next.
//
// go-m3ua's Conn.ReadPD blocks on an internal channel (not the SCTP socket),
// so read deadlines have no effect; closing the connection makes ReadPD
// return ErrNotEstablished. Backpressure into SCCP is bounded by
// shutdownCh/ctx so a wedged consumer cannot block shutdown.
func (h *Handler) readLoop(ctx context.Context, conn *gom3ua.Conn) {
	if conn == nil {
		return
	}
	for {
		pd, err := conn.ReadPD()
		if err != nil {
			if !h.stopping(ctx) {
				h.logger.Error("M3UA association lost", zap.Error(err))
				h.metrics.Errors.Add(1)
			}
			return
		}

		if pd == nil {
			// Defensive: library should never deliver a nil payload, but
			// don't crash the read loop on a malformed frame.
			h.logger.Warn("ReadPD returned nil ProtocolDataPayload")
			continue
		}

		msg := &MTP3Message{
			OPC:         pd.OriginatingPointCode,
			DPC:         pd.DestinationPointCode,
			SI:          pd.ServiceIndicator,
			NI:          pd.NetworkIndicator,
			MP:          pd.MessagePriority,
			SLS:         pd.SignalingLinkSelection,
			PayloadData: pd.Data,
		}

		h.metrics.MessagesReceived.Add(1)
		h.metrics.BytesReceived.Add(uint64(len(pd.Data)))

		h.logger.Debug("Received MTP3 message",
			zap.Uint32("opc", msg.OPC),
			zap.Uint32("dpc", msg.DPC),
			zap.Uint8("si", msg.SI),
			zap.Uint8("sls", msg.SLS),
			zap.Int("size", len(msg.PayloadData)))

		select {
		case h.incomingCh <- msg:
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// writeLoop sends outgoing M3UA DATA messages to SGW
func (h *Handler) writeLoop(ctx context.Context) {
	h.logger.Info("Write loop started")
	defer h.logger.Info("Write loop stopped")

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return

		case mtp3Msg := <-h.outgoingCh:
			if err := h.sendMTP3(mtp3Msg); err != nil {
				h.logger.Error("Failed to send MTP3", zap.Error(err))
				if h.onSendFailure != nil {
					h.onSendFailure(mtp3Msg, err)
				}
				h.metrics.Errors.Add(1)

				// Send error to error channel (non-blocking)
				select {
				case h.errorCh <- err:
				default:
					h.logger.Warn("Error channel full, dropping error")
				}
			}
		}
	}
}

// sendMTP3 sends one MTP3 user message to the SGW as an M3UA DATA whose
// Protocol Data carries the message's own routing label. go-m3ua's
// Conn.Write would instead stamp the label fixed in the connection config at
// dial time, which cannot name a per-message DPC.
//
// The SCTP stream is chosen from the SLS so that messages SCCP wants kept in
// sequence (same SLS) share a stream (RFC 4666 §1.4.7); stream 0 is reserved
// for management.
func (h *Handler) sendMTP3(mtp3 *MTP3Message) error {
	// Check if link is active
	if h.GetState() != ASPStateActive {
		return ErrNotActive
	}

	// Validate message size
	if len(mtp3.PayloadData) > MaxM3UAMessageSize {
		return ErrMessageTooLarge
	}
	if mtp3.OPC == 0 || mtp3.DPC == 0 {
		return fmt.Errorf("%w: routing label needs OPC and DPC (got OPC=%d DPC=%d)",
			ErrInvalidRoutingLabel, mtp3.OPC, mtp3.DPC)
	}

	conn := h.currentConn()
	if conn == nil {
		return ErrNotConnected
	}
	pd := params.NewProtocolData(
		mtp3.OPC, mtp3.DPC, mtp3.SI, h.config.NetworkIndicator,
		mtp3.MP, mtp3.SLS, mtp3.PayloadData,
	)
	n, err := conn.WritePDToStream(pd, streamForSLS(mtp3.SLS, conn.MaxMessageStreamID()))
	if err != nil {
		h.metrics.Errors.Add(1)
		return fmt.Errorf("failed to send MTP3: %w", err)
	}

	// Update metrics
	h.metrics.MessagesSent.Add(1)
	h.metrics.BytesSent.Add(uint64(n))

	h.logger.Debug("Sent MTP3 message",
		zap.Uint32("opc", mtp3.OPC),
		zap.Uint32("dpc", mtp3.DPC),
		zap.Uint8("si", mtp3.SI),
		zap.Uint8("ni", h.config.NetworkIndicator),
		zap.Uint8("sls", mtp3.SLS),
		zap.Int("size", len(mtp3.PayloadData)))

	return nil
}

// streamForSLS maps an SLS onto the negotiated outbound data streams
// 1..maxStream. A zero maxStream (nothing negotiated) falls back to stream 1.
func streamForSLS(sls uint8, maxStream uint16) uint16 {
	if maxStream == 0 {
		return 1
	}
	return 1 + uint16(sls)%maxStream
}

// WaitForActive waits for link to become active or timeout
func (h *Handler) WaitForActive(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		if h.GetState() == ASPStateActive {
			return nil
		}
		<-ticker.C
	}

	return ErrTimeout
}

// IsActive returns true if link is active
func (h *Handler) IsActive() bool {
	return h.GetState() == ASPStateActive
}
