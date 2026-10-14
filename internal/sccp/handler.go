// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"go.uber.org/zap"
)

var (
	ErrInvalidAddress   = errors.New("SCCP: invalid address")
	ErrInvalidMessage   = errors.New("SCCP: invalid message")
	ErrGTTFailed        = errors.New("SCCP: GTT translation failed")
	ErrHopCountExceeded = errors.New("SCCP: hop count exceeded")
	ErrSSNUnavailable   = errors.New("SCCP: subsystem unavailable")
	ErrMessageTooLarge  = errors.New("SCCP: message exceeds max size")
	ErrShutdown         = errors.New("SCCP: handler is shutting down")
	ErrNoRoute          = errors.New("SCCP: no route to destination")
)

const (
	MaxSCCPMessageSize    = 2048
	ChannelBufferSize     = 100
	DefaultWorkerPoolSize = 4
)

// NewHandler creates a new SCCP handler instance.
//
// m3uaIncoming and m3uaOutgoing wire SCCP to the M3UA layer below; obtain
// these from m3ua.Handler.IncomingMessages() and m3ua.Handler.OutgoingMessages()
// respectively. Both must be non-nil for SCCP to start; nil channels would
// silently drop traffic at the protocol boundary.
func NewHandler(
	config *Config,
	m3uaIncoming <-chan *m3ua.MTP3Message,
	m3uaOutgoing chan<- *m3ua.MTP3Message,
	logger *zap.Logger,
) (*Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if m3uaIncoming == nil {
		return nil, fmt.Errorf("m3uaIncoming channel cannot be nil")
	}
	if m3uaOutgoing == nil {
		return nil, fmt.Errorf("m3uaOutgoing channel cannot be nil")
	}
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	handler := &Handler{
		config:         config,
		gttEngine:      NewGTTEngine(logger),
		m3uaIncomingCh: m3uaIncoming,
		m3uaOutgoingCh: m3uaOutgoing,
		tcapIncomingCh: make(chan *SccpMessage, config.ChannelBufferSize),
		tcapOutgoingCh: make(chan *SccpMessage, config.ChannelBufferSize),
		errorCh:        make(chan error, 10),
		shutdownCh:     make(chan struct{}),
		subsystems:     make(map[SubsystemNumber]*SubsystemStatus),
		reassembler:    newReassembler(),
		metrics:        &Metrics{},
		logger:         logger,
	}

	if err := handler.gttEngine.LoadRules(gttRules(config.GTTRules)); err != nil {
		return nil, fmt.Errorf("invalid sccp.gtt_rules: %w", err)
	}

	// Initialize local subsystems
	for _, ssn := range config.LocalSubsystems {
		handler.subsystems[ssn] = &SubsystemStatus{
			SSN:              ssn,
			Available:        true,
			PointCode:        config.LocalPointCode,
			LastStatusChange: time.Now(),
			Congested:        false,
		}
	}

	return handler, nil
}

// Start initializes SCCP handler and worker pool
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("Starting SCCP handler",
		zap.Uint32("local_pc", h.config.LocalPointCode),
		zap.Int("subsystems", len(h.config.LocalSubsystems)),
		zap.Bool("gtt_enabled", h.config.GTTEnabled))

	// Start worker pool
	for i := 0; i < h.config.WorkerPoolSize; i++ {
		go h.worker(ctx, i)
	}

	h.logger.Info("SCCP handler started successfully",
		zap.Int("workers", h.config.WorkerPoolSize))

	return nil
}

// Stop gracefully stops the SCCP handler
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping SCCP handler")

	// Signal shutdown
	close(h.shutdownCh)

	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(done)
	}()

	select {
	case <-done:
		h.logger.Info("SCCP handler stopped gracefully")
	case <-ctx.Done():
		h.logger.Warn("SCCP handler stop timeout")
	}

	return nil
}

// worker processes both directions:
//   - m3uaIncomingCh: MTP3-wrapped SCCP messages from M3UA → parse → route → tcapIncomingCh / m3uaOutgoingCh
//   - tcapOutgoingCh: SCCP messages from TCAP → encode → m3uaOutgoingCh
func (h *Handler) worker(ctx context.Context, id int) {
	h.logger.Info("SCCP worker started", zap.Int("worker_id", id))
	defer h.logger.Info("SCCP worker stopped", zap.Int("worker_id", id))

	for {
		select {
		case <-h.shutdownCh:
			return
		case <-ctx.Done():
			return
		case mtp3Msg := <-h.m3uaIncomingCh:
			h.handleIncomingMessage(mtp3Msg)
		case outMsg := <-h.tcapOutgoingCh:
			if err := h.SendMessage(outMsg); err != nil {
				h.logger.Error("Failed to forward TCAP outgoing message",
					zap.Error(err))
				h.metrics.Errors.Add(1)
				h.reportNotDelivered(outMsg, "not sent: "+err.Error())
			}
		}
	}
}

// handleIncomingMessage processes an MTP3-wrapped SCCP message from M3UA
// (Q.713 §4). This node is an SCCP end node, not a relay: a message is
// delivered to a local subsystem or refused, never forwarded — forwarding a
// GT-routed message back towards the STP that sent it would loop.
func (h *Handler) handleIncomingMessage(mtp3Msg *m3ua.MTP3Message) {
	h.metrics.MessagesReceived.Add(1)

	if mtp3Msg == nil || len(mtp3Msg.PayloadData) == 0 {
		h.logger.Warn("Empty MTP3 message from M3UA layer")
		h.metrics.Errors.Add(1)
		return
	}

	// Parse SCCP message
	sccpMsg, err := ParseSCCP(mtp3Msg.PayloadData, h.config.NetworkVariant)
	if err != nil {
		h.logger.Error("Failed to parse SCCP message", zap.Error(err))
		h.metrics.Errors.Add(1)
		return
	}

	h.logger.Debug("Received SCCP message",
		zap.String("type", sccpMsg.MessageType.String()),
		zap.Uint8("protocol_class", uint8(sccpMsg.ProtocolClass)),
		zap.Int("data_len", len(sccpMsg.Data)))

	if sccpMsg.MessageType == MsgTypeUDTS || sccpMsg.MessageType == MsgTypeXUDTS {
		// One of our messages could not be delivered; the UDTS returns it.
		h.metrics.Errors.Add(1)
		h.logger.Warn("SCCP UDTS: a message from this node was not delivered",
			zap.Stringer("return_cause", sccpMsg.ReturnCause),
			zap.Uint32("from_pc", mtp3Msg.OPC))
		h.reportNotDelivered(sccpMsg, "returned by the network: UDTS "+sccpMsg.ReturnCause.String())
		return
	}

	if sccpMsg.CalledAddress == nil {
		h.metrics.Errors.Add(1)
		h.sendUDTS(sccpMsg, CauseUnqualified, mtp3Msg.OPC)
		return
	}
	if !h.addressedToUs(sccpMsg.CalledAddress) {
		h.logger.Warn("SCCP message not addressed to this node, refusing (not a relay)",
			zap.Uint32("cdpa_pc", sccpMsg.CalledAddress.PointCode),
			zap.String("cdpa_gt", gtDigits(sccpMsg.CalledAddress)))
		h.metrics.Errors.Add(1)
		h.sendUDTS(sccpMsg, CauseNoTranslationForAddress, mtp3Msg.OPC)
		return
	}
	if sccpMsg.MessageType == MsgTypeXUDT && sccpMsg.Segmentation != nil {
		whole, failure := h.reassemble(sccpMsg, mtp3Msg.OPC)
		if failure != nil {
			h.metrics.Errors.Add(1)
			h.logger.Warn("SCCP reassembly failed", zap.Stringer("cause", failure.cause))
			if failure.msg != nil {
				failure.msg.ReturnOnError = true
				h.sendUDTS(failure.msg, failure.cause, mtp3Msg.OPC)
			}
			return
		}
		if whole == nil {
			return // more segments to come
		}
		sccpMsg = whole
	}
	if err := h.deliverLocal(sccpMsg); err != nil {
		h.logger.Error("Failed to deliver message", zap.Error(err))
		h.metrics.Errors.Add(1)
		cause := CauseSubsystemFailure
		if errors.Is(err, ErrSSNUnavailable) {
			if _, known := h.GetSubsystemStatus(sccpMsg.CalledAddress.SubsystemNumber); !known {
				cause = CauseUnequippedUser
			}
		}
		h.sendUDTS(sccpMsg, cause, mtp3Msg.OPC)
	}
}

// MaxUserData is how many octets of user data (TCAP) one UDT can carry
// between two addresses on a network whose links may be narrowband: the
// whole SCCP message must fit an MTP3 signalling information field of 272
// octets less the routing label (4 octets ITU, 7 ANSI; Q.703/Q.704), and the
// data parameter itself holds at most 255. A GT-routed called address may
// grow by a point code in a final translation, which is reserved for.
func (h *Handler) MaxUserData(called, calling *SccpAddress) int {
	const sif = 272
	label := 4
	if h.config.NetworkVariant == VariantANSI {
		label = 7
	}
	overhead := 1 + 1 + 3 + 1 // message type, protocol class, three pointers, data length
	overhead += len(encodeAddress(called, h.config.NetworkVariant))
	overhead += len(encodeAddress(calling, h.config.NetworkVariant))
	if called != nil && called.AddressIndicator.RoutesOnGT() {
		overhead += h.config.NetworkVariant.PCWidth()
	}
	if n := sif - label - overhead; n < maxUnitdataData {
		return n
	}
	return maxUnitdataData
}

// SetDeliveryFailureSink registers the callback told about a message this
// node sent that will not reach its destination: SCCP found no route, M3UA
// could not send it, or the network returned it in a UDTS. msg.Data is the
// user data as sent (for TCAP, our own message). Set before Start.
func (h *Handler) SetDeliveryFailureSink(sink func(msg *SccpMessage, reason string)) {
	h.deliveryFailureSink = sink
}

func (h *Handler) reportNotDelivered(msg *SccpMessage, reason string) {
	if h.deliveryFailureSink != nil && msg != nil && len(msg.Data) > 0 {
		h.deliveryFailureSink(msg, reason)
	}
}

// HandleMTP3SendFailure is M3UA's send-failure callback: the SCCP message
// inside could not be written to the SGW.
func (h *Handler) HandleMTP3SendFailure(mtp3Msg *m3ua.MTP3Message, err error) {
	if mtp3Msg == nil {
		return
	}
	msg, perr := ParseSCCP(mtp3Msg.PayloadData, h.config.NetworkVariant)
	if perr != nil || msg.MessageType == MsgTypeUDTS {
		return
	}
	h.reportNotDelivered(msg, "not sent: "+err.Error())
}

// addressedToUs reports whether an inbound called address names this node:
// our point code, or one of our GTs. Subsystem availability is checked on
// delivery, so that its failure maps to its own UDTS cause.
func (h *Handler) addressedToUs(addr *SccpAddress) bool {
	return addr.PointCode == h.config.LocalPointCode || h.matchesOurGT(addr.GlobalTitle) != gtRoleNone
}

// routeOutbound decides where a message this node originates goes:
//   - to a local subsystem, when it is addressed to us;
//   - on DPC/SSN, to the point code in the called address;
//   - on GT, to the point code a GTT rule gives (see GTTRuleConfig).
func (h *Handler) routeOutbound(msg *SccpMessage) error {
	if h.isLocalAddress(msg.CalledAddress) {
		return h.deliverLocal(msg)
	}
	if !msg.CalledAddress.AddressIndicator.RoutesOnGT() {
		if msg.CalledAddress.PointCode == 0 {
			return fmt.Errorf("%w: called address routes on SSN but has no point code", ErrNoRoute)
		}
		return h.forwardToM3UA(msg, msg.CalledAddress.PointCode)
	}
	dpc, err := h.performGTT(msg)
	if err != nil {
		return err
	}
	return h.forwardToM3UA(msg, dpc)
}

// isLocalAddress checks whether an inbound CdPA targets this node.
// Two distinct paths qualify (ADR-006 GT model):
//
//  1. Classic PC+SSN: CdPA.PointCode equals LocalPointCode and the SSN
//     is a registered local subsystem. SG-side GTT translated the GT
//     into our PC before delivery.
//
//  2. GT-matched: CdPA carries a GT whose digit string equals our
//     local_global_title (responses pinned by peers) or our
//     cluster_global_title (new dialogues from foreign peers loadshared
//     by the SG to this node). The SG delivered the UDT without
//     stripping the GT; we accept it on either configured GT identity.
//
// The SSN-availability check applies to both paths — even GT-matched
// UDTs must address an active subsystem (MAP/CAP) to be delivered.
func (h *Handler) isLocalAddress(addr *SccpAddress) bool {
	if addr == nil {
		return false
	}
	pcMatch := addr.PointCode == h.config.LocalPointCode
	gtMatch := h.matchesOurGT(addr.GlobalTitle) != gtRoleNone

	if !pcMatch && !gtMatch {
		return false
	}

	h.ssnMu.RLock()
	defer h.ssnMu.RUnlock()
	status, exists := h.subsystems[addr.SubsystemNumber]
	return exists && status.Available
}

// gtRole identifies which configured GT (if any) an inbound CdPA's
// GT digit string matches. Used for log/metric attribution: cluster-GT
// inbound = new dialogue from a foreign peer; local-GT inbound = peer
// response to a dialogue we originated. Carries no routing semantic
// beyond what isLocalAddress already returns.
type gtRole uint8

const (
	gtRoleNone    gtRole = iota // No GT or no match
	gtRoleLocal                 // Matches LocalGlobalTitle (sticky per-node CgPA)
	gtRoleCluster               // Matches ClusterGlobalTitle (shared inbound)
)

func (r gtRole) String() string {
	switch r {
	case gtRoleLocal:
		return "local_gt"
	case gtRoleCluster:
		return "cluster_gt"
	}
	return "none"
}

// matchesOurGT returns the role (local / cluster / none) that a CdPA's
// GT digit string matches, by string equality on Address. Returns
// gtRoleNone when the address has no GT, when the GT digits are empty,
// or when neither configured GT is set.
//
// Comparison is digit-string only; encoding-scheme/NAI/NP differences
// between what the peer sent and what we configured are not material —
// digit equality is the GT identity (per Q.713 §3.4.2 GT semantics).
func (h *Handler) matchesOurGT(gt *GlobalTitle) gtRole {
	if gt == nil || gt.Address == "" {
		return gtRoleNone
	}
	if local := h.config.LocalGlobalTitle.Address; local != "" && gt.Address == local {
		return gtRoleLocal
	}
	if cluster := h.config.ClusterGlobalTitle.Address; cluster != "" && gt.Address == cluster {
		return gtRoleCluster
	}
	return gtRoleNone
}

// deliverLocal delivers message to local TCAP layer. Tags the log
// record with the matched GT role (local/cluster/none) so operators
// can grep cluster-GT inbound traffic (new dialogues from foreign
// peers, candidates for SG loadshare distribution analysis) from
// local-GT inbound (responses pinned to this node). The role does
// not affect routing — both are delivered identically; it's
// telemetry only.
func (h *Handler) deliverLocal(msg *SccpMessage) error {
	role := h.matchesOurGT(msg.CalledAddress.GlobalTitle)
	h.logger.Debug("Delivering message to local TCAP",
		zap.Uint8("ssn", uint8(msg.CalledAddress.SubsystemNumber)),
		zap.String("inbound_gt_role", role.String()))

	// Check subsystem availability
	h.ssnMu.RLock()
	status, exists := h.subsystems[msg.CalledAddress.SubsystemNumber]
	h.ssnMu.RUnlock()

	if !exists || !status.Available {
		h.logger.Warn("Subsystem unavailable",
			zap.Uint8("ssn", uint8(msg.CalledAddress.SubsystemNumber)))
		return ErrSSNUnavailable
	}

	// Forward to TCAP
	select {
	case h.tcapIncomingCh <- msg:
		return nil
	default:
		h.logger.Error("TCAP channel full, dropping message")
		h.metrics.Errors.Add(1)
		return errors.New("TCAP channel full")
	}
}

// performGTT translates the called GT of an outbound message and returns
// the DPC to send it to. An intermediate translation (rule SSN 0) leaves
// the called address routing on GT for the next relay; a final one rewrites
// it to route on the rule's point code and SSN (Q.714 §2.4).
func (h *Handler) performGTT(msg *SccpMessage) (uint32, error) {
	h.metrics.GTTTranslations.Add(1)

	gt := msg.CalledAddress.GlobalTitle
	if gt == nil || gt.Address == "" {
		h.metrics.GTTFailures.Add(1)
		return 0, fmt.Errorf("%w: called address routes on GT but carries none", ErrGTTFailed)
	}
	if !h.config.GTTEnabled {
		h.metrics.GTTFailures.Add(1)
		return 0, fmt.Errorf("%w: GT %s needs translation but sccp.gtt_enabled is false", ErrNoRoute, gt.Address)
	}

	dest, err := h.gttEngine.Translate(gt)
	if err != nil {
		h.metrics.GTTFailures.Add(1)
		return 0, fmt.Errorf("%w: GT %s: %v", ErrNoRoute, gt.Address, err)
	}

	if dest.SSN != 0 {
		addr := *msg.CalledAddress
		addr.PointCode = dest.PointCode
		addr.SubsystemNumber = dest.SSN
		addr.AddressIndicator |= AIPCPresent | AISSNPresent | AIRouteOnSSN
		msg.CalledAddress = &addr
	}

	h.logger.Debug("GTT",
		zap.String("gt", gt.Address),
		zap.Uint32("dpc", dest.PointCode),
		zap.Bool("final", dest.SSN != 0))
	return dest.PointCode, nil
}

// forwardToM3UA encodes msg and hands it to M3UA with its routing label:
// OPC = our point code, DPC = the next SCCP node, SI = 3 (SCCP). M3UA stamps
// the network indicator. SLS: protocol class 0 rotates it so traffic spreads
// across the STP's links; class 1 (in-sequence) keeps one SLS per address
// pair (Q.714 §1.1.2).
func (h *Handler) forwardToM3UA(msg *SccpMessage, dpc uint32) error {
	h.logger.Debug("Forwarding message to M3UA",
		zap.Uint32("dpc", dpc),
		zap.Uint8("dest_ssn", uint8(msg.CalledAddress.SubsystemNumber)))

	payload, err := EncodeSCCP(msg, h.config.NetworkVariant)
	if err != nil {
		h.logger.Error("Failed to encode SCCP message", zap.Error(err))
		return err
	}

	mtp3Msg := &m3ua.MTP3Message{
		OPC:         h.config.LocalPointCode,
		DPC:         dpc,
		SI:          3,
		SLS:         h.selectSLS(msg),
		PayloadData: payload,
	}

	select {
	case h.m3uaOutgoingCh <- mtp3Msg:
		h.metrics.MessagesSent.Add(1)
		h.metrics.BytesSent.Add(uint64(len(payload)))
		return nil
	default:
		h.logger.Error("M3UA channel full, dropping message")
		h.metrics.Errors.Add(1)
		return errors.New("M3UA channel full")
	}
}

// slsMask keeps SLS values within 4 bits, the ITU width (Q.704 §2.2); the
// same values are valid in ANSI's wider field.
const slsMask = 0x0F

// selectSLS picks the signalling link selection for an outbound message.
func (h *Handler) selectSLS(msg *SccpMessage) uint8 {
	if msg.ProtocolClass == ClassInSequenceConnectionless {
		hash := fnv.New32a()
		for _, a := range []*SccpAddress{msg.CalledAddress, msg.CallingAddress} {
			if a == nil {
				continue
			}
			_, _ = fmt.Fprintf(hash, "%d/%d/", a.PointCode, a.SubsystemNumber)
			if a.GlobalTitle != nil {
				hash.Write([]byte(a.GlobalTitle.Address))
			}
		}
		return uint8(hash.Sum32()) & slsMask
	}
	return uint8(h.slsCounter.Add(1)) & slsMask
}

// sendUDTS returns an undeliverable message to its originator (Q.714
// §4.2), but only when the originator asked for it (return option) and
// never for a UDTS. It goes back to the node that handed it to us (the MTP3
// OPC), which routes it on the swapped called address.
func (h *Handler) sendUDTS(orig *SccpMessage, cause ReturnCause, toPC uint32) {
	if !orig.ReturnOnError || orig.MessageType == MsgTypeUDTS || orig.MessageType == MsgTypeXUDTS ||
		orig.CallingAddress == nil || toPC == 0 {
		return
	}
	returned := MsgTypeUDTS
	if orig.MessageType == MsgTypeXUDT {
		returned = MsgTypeXUDTS // an XUDT is returned in an XUDTS (Q.714 §4.2)
	}
	udts := &SccpMessage{
		MessageType:    returned,
		HopCounter:     h.config.MaxHopCounter,
		ReturnCause:    cause,
		CalledAddress:  orig.CallingAddress,
		CallingAddress: orig.CalledAddress,
		Data:           orig.Data,
	}
	if err := h.forwardToM3UA(udts, toPC); err != nil {
		h.logger.Error("Failed to send UDTS", zap.Error(err))
	}
}

// SendMessage sends an SCCP message this node originates (called by TCAP).
// The return option is set so that a node unable to deliver it reports why
// (logged on receipt), instead of the loss being visible only as a timeout.
func (h *Handler) SendMessage(msg *SccpMessage) error {
	if err := h.validateMessage(msg); err != nil {
		return err
	}
	if msg.MessageType == MsgTypeUDT || msg.MessageType == MsgTypeXUDT {
		msg.ReturnOnError = true
	}
	if msg.HopCounter == 0 {
		msg.HopCounter = h.config.MaxHopCounter
	}
	return h.routeOutbound(msg)
}

// validateMessage validates SCCP message before sending
func (h *Handler) validateMessage(msg *SccpMessage) error {
	if msg == nil {
		return ErrInvalidMessage
	}
	if msg.CalledAddress == nil {
		return ErrInvalidAddress
	}
	if len(msg.Data) > MaxSCCPMessageSize {
		return ErrMessageTooLarge
	}
	return nil
}

// parseSCCPMessage parses raw SCCP payload from M3UA (ITU-T Q.713)
// SetSubsystemStatus updates subsystem availability
func (h *Handler) SetSubsystemStatus(ssn SubsystemNumber, available bool) {
	h.ssnMu.Lock()
	defer h.ssnMu.Unlock()

	if status, exists := h.subsystems[ssn]; exists {
		status.Available = available
		status.LastStatusChange = time.Now()

		h.logger.Info("Subsystem status changed",
			zap.Uint8("ssn", uint8(ssn)),
			zap.Bool("available", available))
	}
}

// GetSubsystemStatus returns subsystem status
func (h *Handler) GetSubsystemStatus(ssn SubsystemNumber) (*SubsystemStatus, bool) {
	h.ssnMu.RLock()
	defer h.ssnMu.RUnlock()

	status, exists := h.subsystems[ssn]
	return status, exists
}

// LoadGTTRules loads GTT rules into the engine
func (h *Handler) LoadGTTRules(rules []*GTTRule) error {
	return h.gttEngine.LoadRules(rules)
}

// AddGTTRule adds a single GTT rule
func (h *Handler) AddGTTRule(rule *GTTRule) error {
	return h.gttEngine.AddRule(rule)
}

// RemoveGTTRule removes a GTT rule by ID
func (h *Handler) RemoveGTTRule(ruleID uint64) error {
	return h.gttEngine.RemoveRule(ruleID)
}

// GetGTTRules returns all GTT rules
func (h *Handler) GetGTTRules() []*GTTRule {
	return h.gttEngine.GetRules()
}

// GetMetrics returns current metrics
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// GetHealthStatus returns health status
func (h *Handler) GetHealthStatus() *HealthStatus {
	h.ssnMu.RLock()
	activeSSNs := make([]SubsystemNumber, 0, len(h.subsystems))
	for ssn, status := range h.subsystems {
		if status.Available {
			activeSSNs = append(activeSSNs, ssn)
		}
	}
	h.ssnMu.RUnlock()

	return &HealthStatus{
		ActiveSubsystems: activeSSNs,
		GTTRulesCount:    len(h.gttEngine.GetRules()),
		MessagesReceived: h.metrics.MessagesReceived.Load(),
		MessagesSent:     h.metrics.MessagesSent.Load(),
		Errors:           h.metrics.Errors.Load(),
		GTTTranslations:  h.metrics.GTTTranslations.Load(),
		LastUpdate:       time.Now(),
	}
}

// TcapIncomingChannel returns the read side of the SCCP→TCAP channel; TCAP
// drains messages routed to its local subsystems from here.
func (h *Handler) TcapIncomingChannel() <-chan *SccpMessage {
	return h.tcapIncomingCh
}

// TcapOutgoingChannel returns the write side of the TCAP→SCCP channel; TCAP
// pushes outbound TCAP responses (already wrapped in *SccpMessage with proper
// addresses) here for SCCP to forward via M3UA.
func (h *Handler) TcapOutgoingChannel() chan<- *SccpMessage {
	return h.tcapOutgoingCh
}

// Errors returns channel for errors
func (h *Handler) Errors() <-chan error {
	return h.errorCh
}

// validateConfig validates SCCP configuration
func validateConfig(cfg *Config) error {
	if cfg.LocalPointCode == 0 {
		return errors.New("local_point_code is required")
	}
	if !cfg.NetworkVariant.ValidPointCode(cfg.LocalPointCode) {
		return fmt.Errorf("local_point_code %d does not fit a %s point code", cfg.LocalPointCode, cfg.NetworkVariant)
	}
	if len(cfg.LocalSubsystems) == 0 {
		return errors.New("at least one local subsystem is required")
	}
	if cfg.MaxHopCounter == 0 {
		cfg.MaxHopCounter = 15 // Default per ITU-T Q.713
	}
	if cfg.ChannelBufferSize == 0 {
		cfg.ChannelBufferSize = ChannelBufferSize
	}
	if cfg.WorkerPoolSize == 0 {
		cfg.WorkerPoolSize = DefaultWorkerPoolSize
	}
	if err := validateGTConfig("local_global_title", cfg.LocalGlobalTitle); err != nil {
		return err
	}
	if err := validateGTConfig("cluster_global_title", cfg.ClusterGlobalTitle); err != nil {
		return err
	}
	for i, r := range cfg.GTTRules {
		if err := validateGTTRule(r, cfg.NetworkVariant); err != nil {
			return fmt.Errorf("gtt_rules[%d]: %w", i, err)
		}
	}
	// Same digits in both roles defeats sticky response routing — peers
	// pinning CgPA=local_gt would round-trip to the cluster GT instead.
	// HA topology (ADR-006) requires distinct addresses.
	if cfg.LocalGlobalTitle.Address != "" &&
		cfg.LocalGlobalTitle.Address == cfg.ClusterGlobalTitle.Address {
		return fmt.Errorf("local_global_title.address and cluster_global_title.address must differ when both are set (got %q for both)",
			cfg.LocalGlobalTitle.Address)
	}
	return nil
}

// validateGTConfig is the shared predicate for any GTConfig instance,
// used by both the local-sticky and cluster-shared roles. role is the
// YAML key prefix (e.g. "local_global_title") used in the error
// message so operators get an unambiguous pointer to the bad field.
func validateGTConfig(role string, gt GTConfig) error {
	if gt.Address == "" {
		return nil // disabled — fields below are ignored
	}
	if !isE164Digits(gt.Address) {
		return fmt.Errorf("%s.address must be E.164 digits (0-9), got %q",
			role, gt.Address)
	}
	if g := gt.GTI; g != 0 && (g < 1 || g > 4) {
		return fmt.Errorf("%s.gti must be 0,1,2,3, or 4 — got %d", role, g)
	}
	return nil
}

// validateGTTRule checks one operator translation rule.
func validateGTTRule(r GTTRuleConfig, variant NetworkVariant) error {
	digits := strings.TrimSuffix(r.Pattern, "*")
	if r.Pattern == "" || (digits != "" && !isE164Digits(digits)) || strings.Contains(digits, "*") {
		return fmt.Errorf("pattern %q must be digits, a digit prefix followed by \"*\", or \"*\"", r.Pattern)
	}
	if r.PointCode == 0 || !variant.ValidPointCode(r.PointCode) {
		return fmt.Errorf("point_code %d is not a valid %s point code", r.PointCode, variant)
	}
	return nil
}

// gttRules turns the operator's rules into engine rules, one destination
// each, numbered in configuration order.
func gttRules(cfg []GTTRuleConfig) []*GTTRule {
	rules := make([]*GTTRule, 0, len(cfg))
	for i, r := range cfg {
		rule := &GTTRule{
			ID:           uint64(i + 1),
			Priority:     r.Priority,
			Pattern:      r.Pattern,
			Description:  r.Description,
			Enabled:      true,
			Destinations: make([]Destination, 1),
		}
		rule.Destinations[0].PointCode = r.PointCode
		rule.Destinations[0].SSN = SubsystemNumber(r.SSN)
		rule.Destinations[0].Weight = 1
		rule.Destinations[0].Active = true
		rules = append(rules, rule)
	}
	return rules
}

// gtDigits returns the GT digits of an address, or "" when it has none.
func gtDigits(addr *SccpAddress) string {
	if addr == nil || addr.GlobalTitle == nil {
		return ""
	}
	return addr.GlobalTitle.Address
}

// isE164Digits returns true when s contains only ASCII digits 0-9.
// Used by LocalGlobalTitle validation; same predicate is enforceable
// elsewhere if other GT-bearing fields land.
func isE164Digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// BuildLocalGT returns the wire-level *GlobalTitle for the per-node
// sticky CgPA, or nil when LocalGlobalTitle.Address is empty (disabled).
// See buildGT for default behaviour.
func (cfg *Config) BuildLocalGT() *GlobalTitle {
	return buildGT(cfg.LocalGlobalTitle)
}

// BuildClusterGT returns the wire-level *GlobalTitle for the
// cluster-shared inbound GT used by SG loadshare matching, or nil when
// ClusterGlobalTitle.Address is empty (disabled — single-node mode).
func (cfg *Config) BuildClusterGT() *GlobalTitle {
	return buildGT(cfg.ClusterGlobalTitle)
}

// buildGT projects a GTConfig (operator YAML) onto the wire-level
// *GlobalTitle, applying carrier-grade defaults to zero fields:
// GTI0100, TT=0, NP=ISDN(1), NAI=International(4). EncodingScheme
// auto-selects ESBCD vs ESBCDEven from address parity per
// Q.713 §3.4.2.3.4 (odd digits → BCD; even → BCD even-padded).
// Returns nil when Address is empty (disabled).
func buildGT(c GTConfig) *GlobalTitle {
	if c.Address == "" {
		return nil
	}
	gti := GlobalTitleIndicator(c.GTI)
	if gti == 0 {
		gti = GTI0100
	}
	np := NumberingPlan(c.NumberingPlan)
	if np == 0 {
		np = NPISDN
	}
	nai := NatureOfAddressIndicator(c.NatureOfAddress)
	if nai == 0 {
		nai = NAIInternational
	}
	es := ESBCD
	if len(c.Address)%2 == 0 {
		es = ESBCDEven
	}
	return &GlobalTitle{
		GTI:             gti,
		TranslationType: c.TranslationType,
		NumberingPlan:   np,
		EncodingScheme:  es,
		NAI:             nai,
		Address:         c.Address,
	}
}

// MessageType.String returns string representation
func (mt MessageType) String() string {
	switch mt {
	case MsgTypeCR:
		return "CR"
	case MsgTypeCC:
		return "CC"
	case MsgTypeCREF:
		return "CREF"
	case MsgTypeRLSD:
		return "RLSD"
	case MsgTypeRLC:
		return "RLC"
	case MsgTypeDT1:
		return "DT1"
	case MsgTypeDT2:
		return "DT2"
	case MsgTypeAK:
		return "AK"
	case MsgTypeUDT:
		return "UDT"
	case MsgTypeUDTS:
		return "UDTS"
	case MsgTypeED:
		return "ED"
	case MsgTypeEA:
		return "EA"
	case MsgTypeRSR:
		return "RSR"
	case MsgTypeRSC:
		return "RSC"
	case MsgTypeERR:
		return "ERR"
	case MsgTypeIT:
		return "IT"
	case MsgTypeXUDT:
		return "XUDT"
	case MsgTypeXUDTS:
		return "XUDTS"
	case MsgTypeLUDT:
		return "LUDT"
	case MsgTypeLUDTS:
		return "LUDTS"
	default:
		return "UNKNOWN"
	}
}
