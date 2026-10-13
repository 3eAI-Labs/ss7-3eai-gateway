// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

// Package ss7peer provides an in-process SS7 peer that the gateway under
// test connects to over real SCTP/M3UA. It speaks SCCP/TCAP/MAP using the
// same wire-format codecs the gateway itself uses
// (`internal/sccp`, `wmnsk/go-tcap`, `internal/mapprotocol`) — single
// source of truth, so codec drift between SUT and harness can never hide.
//
// Usage (E2E test):
//
//	peer, err := ss7peer.Start(ctx, ss7peer.Config{
//	    SCTPAddr: "127.0.0.1:2906", PointCode: 200,
//	    GatewayPointCode: 100,
//	    OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, calledGT, callingGT string) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
//	        return &mapprotocol.SendRoutingInfoForSMResponse{
//	            IMSI: "286010000000123",
//	            LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
//	                NetworkNodeNumber: &mapprotocol.AddressString{Nature: 4, NumberPlan: 1, Digits: "905304445566"},
//	            },
//	        }, nil
//	    },
//	})
//	defer peer.Stop()
//
// The gateway then connects with m3ua.sgw_address = peer.SCTPAddr and the
// configured PCs. All MAP responses produced by OnSRI flow back through
// the gateway's MAP dispatcher → response sink → NATS publish.
//
// Scope: SRI-for-SM (op 45, OnSRI) and MT-Forward-SM (op 44, OnMtForwardSM).
// MoForwardSM is not yet wired (peer is HLR/VMSC role; gateway is the SMSC
// or SMSC-front, so it originates MT but receives MO from the network — MO
// peer hooks belong on a separate "MSC peer" role). CAP/INAP not covered
// (INAP is not implemented; CAP InitialDP origination is non-standard
// direction so untested over the peer).
package ss7peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/ishidawataru/sctp"
	"github.com/wmnsk/go-m3ua"
	"github.com/wmnsk/go-m3ua/messages/params"
	gotcap "github.com/wmnsk/go-tcap"
	"go.uber.org/zap"
)

// SRICallback is invoked synchronously when the peer receives a MAP
// SendRoutingInfoForSM Invoke. Returning a non-nil response triggers an
// END ReturnResult; returning a non-nil error triggers an END ReturnError
// carrying the MAP error code of a MAPError in the chain, or systemFailure for
// any other error.
//
// calledGT / callingGT are the GT digits the gateway placed in CdPA / CgPA
// at the SCCP layer, surfaced so tests can assert on address construction
// (e.g. callingGT == own_gt). Either may be "" when the corresponding
// address routes on PC+SSN only.
//
// otid is the gateway's TCAP Originating Transaction ID (uint32, big-endian
// from wire bytes). The upper byte equals the gateway's node_id_numeric per
// `internal/tcap/handler.go::allocateTID`, so cluster tests can attribute
// inbound dialogues to the originating cluster member without out-of-band
// coordination — the protocol already encodes "who sent this".
type SRICallback func(
	req *mapprotocol.SendRoutingInfoForSMRequest,
	calledGT, callingGT string,
	otid uint32,
) (*mapprotocol.SendRoutingInfoForSMResponse, error)

// MtForwardSMCallback is invoked synchronously when the peer receives a
// MAP MT-Forward-SM Invoke (op 44). Returning a non-nil response triggers
// an END ReturnResult; returning a non-nil error or nil response triggers
// an END ReturnError (currently surfaces as ErrSystemFailure to the SUT).
//
// calledGT / callingGT are the GT digits the gateway placed in CdPA / CgPA
// at the SCCP layer, surfaced so tests can assert on address construction
// (e.g. calledGT == VMSC, callingGT == own_gt). Either may be "" when the
// corresponding address routes on PC+SSN only.
//
// otid is the gateway's originating TID — same semantic as SRICallback.
type MtForwardSMCallback func(
	req *mapprotocol.MtForwardSMRequest,
	calledGT, callingGT string,
	otid uint32,
) (*mapprotocol.MtForwardSMResponse, error)

// PreDropHook returns true to instruct the peer to silently drop the
// incoming Invoke (no response, no abort). The gateway's TCAP layer will
// then time out the dialogue at `tcap.dialogue_timeout` and fire the
// FailureSink with `Status=TIMEOUT, ErrorCode=-1`.
//
// Use this exclusively to exercise the TIMEOUT failure path — returning
// `nil` from a normal callback triggers ReturnError (which maps to
// REMOTE_ERROR, not TIMEOUT). The peer counts the request via the
// `_requests` counter but increments `_errors` so test code can distinguish
// dropped from served.
//
// otid carries the gateway's originating TID — primary use is per-node
// drop selection in cluster tests (e.g. drop only TIDs whose upper byte
// equals the node_id under outage simulation).
type PreDropHook func(otid uint32) bool

// Config configures the in-process HLR peer.
type Config struct {
	// SCTPAddr is the SCTP listen address ("host:port"). Typically loopback
	// in tests, e.g. "127.0.0.1:2906".
	SCTPAddr string

	// PointCode is the peer's own MTP3 point code (advertised as OPC on
	// outgoing responses). Must match the gateway's M3UA peer DPC.
	PointCode uint32

	// GatewayPointCode is the SUT's point code (advertised as DPC on outgoing
	// responses). Must match the gateway's m3ua.sgw_pc / SCCP local PC.
	GatewayPointCode uint32

	// NetworkIndicator is the MTP3 network indicator the gateway must put in
	// its routing label (0 international, 2 national; Q.704 §14.2.2). The
	// peer rejects any other value, as an STP would.
	NetworkIndicator uint8

	// NetworkVariant selects ITU 14-bit (default) or ANSI 24-bit PC width
	// on the SCCP wire. MUST match the gateway under test — mismatch
	// silently corrupts addresses (Q.713 §3.4.1).
	NetworkVariant sccp.NetworkVariant

	// AspID is the M3UA ASP identifier the peer advertises. Default 1 if 0.
	AspID uint32

	// OnSRI is the callback invoked on every SRI-for-SM Invoke. Required.
	OnSRI SRICallback

	// OnMtForwardSM is the callback invoked on every MT-Forward-SM Invoke.
	// Optional — when nil, the peer drops op 44 messages silently and the
	// gateway dialogue eventually times out (P-Abort upstream). Tests that
	// exercise the SMS delivery path MUST set this.
	OnMtForwardSM MtForwardSMCallback

	// OnSRIPreDrop is consulted before OnSRI is invoked. When it returns
	// true, the peer drops the SRI Invoke silently (no ReturnResult, no
	// ReturnError, no Abort) — exclusively for exercising the TCAP TIMEOUT
	// failure path in cluster outage tests. Optional; nil disables.
	OnSRIPreDrop PreDropHook

	// OnMtForwardSMPreDrop — same semantic as OnSRIPreDrop, for op 44.
	OnMtForwardSMPreDrop PreDropHook

	// AcceptTimeout bounds how long Listen.Accept will wait for an ASP
	// before tearing down. Default 5s if zero.
	AcceptTimeout time.Duration

	// Logger is optional; defaults to zap.NewNop().
	Logger *zap.Logger
}

// Peer is the running in-process HLR / SG-equivalent harness.
type Peer struct {
	cfg       Config
	listener  *m3ua.Listener
	sctpAddr  *sctp.SCTPAddr
	logger    *zap.Logger
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	stoppedCh chan struct{}
	stopOnce  sync.Once

	// Active M3UA associations. Each accepted ASP gets registered at
	// connHandler entry and deregistered on exit. Cluster tests use this
	// list to (a) wait for the expected ASP fan-in (`WaitForConns`) and
	// (b) round-robin outbound dialogues across cluster members
	// (`OriginateMoForwardSM`). The list contains only currently-active
	// conns; closed associations are pruned automatically.
	connsMu sync.RWMutex
	conns   []*m3ua.Conn

	// Atomic counter for peer-allocated TIDs (BEGIN we originate). The
	// upper byte is reserved for the peer (= 0x00 today, distinct from
	// any cluster-node TID upper byte 1..255). Lower 24 bits are the
	// monotonic counter. Wraps after 16M outbound dialogues — fine for
	// test workloads.
	originateTID atomic.Uint32

	// Metrics — exported via getters. Drives test assertions
	// ("did peer actually receive 1 SRI?").
	sriRequests     atomic.Uint64
	sriResponses    atomic.Uint64
	sriErrors       atomic.Uint64
	mtFwdRequests   atomic.Uint64
	mtFwdResponses  atomic.Uint64
	mtFwdErrors     atomic.Uint64
	moFwdOriginated atomic.Uint64
	connsAccepted   atomic.Uint64

	violationsMu sync.Mutex
	violations   []string
}

// Start binds SCTP, builds the M3UA listener, and accepts the first
// association in a background goroutine. Returns once the listener is
// ready (so the caller can immediately point the gateway at it without a
// race).
func Start(ctx context.Context, cfg Config) (*Peer, error) {
	if cfg.OnSRI == nil {
		return nil, errors.New("ss7peer: OnSRI callback required")
	}
	if cfg.SCTPAddr == "" {
		return nil, errors.New("ss7peer: SCTPAddr required")
	}
	if cfg.PointCode == 0 || cfg.GatewayPointCode == 0 {
		return nil, errors.New("ss7peer: PointCode and GatewayPointCode required")
	}
	if cfg.AspID == 0 {
		cfg.AspID = 1
	}
	if cfg.AcceptTimeout == 0 {
		cfg.AcceptTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	host, portStr, err := net.SplitHostPort(cfg.SCTPAddr)
	if err != nil {
		return nil, fmt.Errorf("ss7peer: parse SCTPAddr: %w", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return nil, fmt.Errorf("ss7peer: parse SCTPAddr port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("ss7peer: invalid host %q", host)
	}
	sctpAddr := &sctp.SCTPAddr{
		IPAddrs: []net.IPAddr{{IP: ip}},
		Port:    port,
	}

	// M3UA config — peer is SGW-side, so OPC = peer PC, DPC = gateway PC.
	// NewServerConfig (not NewConfig) is required for SGW-side: it
	// initializes the param wrappers go-m3ua's Listener.Accept handshake
	// expects when answering ASPUP/ASPAC. Routing context / network
	// appearance / traffic mode mirror the gateway's client config
	// (sccp.local_subsystems route on PC+SSN; one routing context suffices here).
	// Heartbeat disabled — tests run in seconds, not minutes; the BEAT
	// machinery just adds noise + a state-change opportunity that races
	// the first ReadPD. Production peers re-enable.
	hb := &m3ua.HeartbeatInfo{Enabled: false}
	m3uaCfg := m3ua.NewServerConfig(
		hb,
		cfg.PointCode,        // OPC
		cfg.GatewayPointCode, // DPC
		cfg.AspID,            // ASP id
		1,                    // TrafficModeType: 1 = Override (mirror gateway)
		0,                    // NetworkAppearance
		0,                    // CorrelationID
		[]uint32{1},          // RoutingContexts (mirror gateway)
		3,                    // SI = SCCP
		0, 0, 0,              // NI / MP / SLS
	)
	// CorrelationID is optional and not required for the handshake; null
	// it out so the param doesn't appear in BEAT frames either.
	m3uaCfg.CorrelationID = nil

	listener, err := m3ua.Listen("m3ua", sctpAddr, m3uaCfg)
	if err != nil {
		return nil, fmt.Errorf("ss7peer: m3ua.Listen: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	p := &Peer{
		cfg:       cfg,
		listener:  listener,
		sctpAddr:  sctpAddr,
		logger:    cfg.Logger.Named("ss7peer"),
		cancel:    cancel,
		stoppedCh: make(chan struct{}),
	}

	p.wg.Add(1)
	go p.acceptLoop(runCtx)

	p.logger.Info("ss7peer started",
		zap.String("listen", cfg.SCTPAddr),
		zap.Uint32("opc", cfg.PointCode),
		zap.Uint32("dpc", cfg.GatewayPointCode))

	return p, nil
}

// SCTPAddr returns the address the peer is listening on. Useful when the
// caller asks for ":0" and needs the OS-allocated port.
func (p *Peer) SCTPAddr() string {
	if a := p.listener.Addr(); a != nil {
		return a.String()
	}
	return p.cfg.SCTPAddr
}

// Stop tears down the listener and waits for in-flight Accept loops to
// drain. Idempotent.
func (p *Peer) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		_ = p.listener.Close()
		p.wg.Wait()
		close(p.stoppedCh)
		p.logger.Info("ss7peer stopped",
			zap.Uint64("sri_requests", p.sriRequests.Load()),
			zap.Uint64("sri_responses", p.sriResponses.Load()),
			zap.Uint64("sri_errors", p.sriErrors.Load()),
			zap.Uint64("mt_fwd_requests", p.mtFwdRequests.Load()),
			zap.Uint64("mt_fwd_responses", p.mtFwdResponses.Load()),
			zap.Uint64("mt_fwd_errors", p.mtFwdErrors.Load()))
	})
}

// SRIRequests returns the total count of SRI-for-SM Invokes the peer
// has received since Start. Atomic; safe for concurrent read.
func (p *Peer) SRIRequests() uint64  { return p.sriRequests.Load() }
func (p *Peer) SRIResponses() uint64 { return p.sriResponses.Load() }
func (p *Peer) SRIErrors() uint64    { return p.sriErrors.Load() }

// MtForwardSMRequests / MtForwardSMResponses / MtForwardSMErrors return
// the per-op counters for MT-Forward-SM. Same semantic as the SRI
// counters: requests = decoded Invokes seen on the wire, responses =
// successful ReturnResults written back, errors = decode/encode/ABORT
// paths or callback errors.
func (p *Peer) MtForwardSMRequests() uint64  { return p.mtFwdRequests.Load() }
func (p *Peer) MtForwardSMResponses() uint64 { return p.mtFwdResponses.Load() }
func (p *Peer) MtForwardSMErrors() uint64    { return p.mtFwdErrors.Load() }

// acceptLoop accepts M3UA associations until the listener is closed.
// Each accepted Conn gets its own connHandler goroutine — multiple
// gateway nodes can connect to the same peer concurrently if a future
// HA-cluster test needs that.
//
// IMPORTANT: the ctx passed into Accept is captured by the library's
// per-conn monitor goroutine (see go-m3ua server.go:79). Cancelling it
// kills the live association's read loop, surfacing as ErrNotEstablished
// on the very next ReadPD. We therefore pass the long-lived runCtx (not
// a per-Accept WithTimeout) so connections live as long as the peer
// itself. AcceptTimeout is enforced via Listener.Close on Stop instead.
func (p *Peer) acceptLoop(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := p.listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.Debug("Accept ended", zap.Error(err))
			// Real SCTP errors (e.g. listener closed) end the loop.
			return
		}

		p.connsAccepted.Add(1)
		p.registerConn(conn)
		p.logger.Info("M3UA association accepted",
			zap.String("remote", conn.RemoteAddr().String()))

		p.wg.Add(1)
		go p.connHandler(ctx, conn)
	}
}

// connHandler runs the per-Conn read loop: ReadPD → SCCP/TCAP/MAP decode
// → callback → response encode → WritePD. One goroutine per association;
// shutdown via ctx (closes conn to unblock ReadPD) or read error.
func (p *Peer) connHandler(ctx context.Context, conn *m3ua.Conn) {
	defer p.wg.Done()
	defer conn.Close()
	defer p.deregisterConn(conn)

	// Watcher: ctx cancel → close conn → ReadPD returns ErrNotEstablished
	// → main loop exits cleanly. Without this, Stop() would block forever
	// on wg.Wait while ReadPD sits inside an unkillable channel receive.
	closeOnce := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-closeOnce:
		}
	}()
	defer close(closeOnce)

	for {
		pd, err := conn.ReadPD()
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Debug("ReadPD ended", zap.Error(err))
			}
			return
		}
		if pd == nil {
			continue
		}

		p.handleMTP3(conn, pd)
	}
}

// handleMTP3 decodes one inbound MTP3 payload through SCCP → TCAP →
// MAP, dispatches to the user callback, and writes the response back on
// the same association with OPC/DPC swapped.
func (p *Peer) handleMTP3(conn *m3ua.Conn, pd *params.ProtocolDataPayload) {
	if !p.checkRoutingLabel(pd) {
		return
	}
	sccpMsg, err := sccp.ParseSCCP(pd.Data, p.cfg.NetworkVariant)
	if err != nil {
		p.violate("SCCP parse failed: %v", err)
		return
	}
	if sccpMsg.MessageType != sccp.MsgTypeUDT && sccpMsg.MessageType != sccp.MsgTypeXUDT {
		// UDTS / errors / unsupported — ignore for now.
		return
	}
	calledOK := p.checkAddress("called party address", sccpMsg.CalledAddress, true)
	callingOK := p.checkAddress("calling party address", sccpMsg.CallingAddress, false)
	if !calledOK || !callingOK {
		return
	}

	tcapMsg, err := gotcap.Parse(sccpMsg.Data)
	if err != nil {
		p.logger.Warn("TCAP parse failed", zap.Error(err))
		return
	}

	// Only BEGIN with at least one Invoke is interesting for the HLR role.
	// Other transaction types (END/CONTINUE/ABORT) reach the peer only on
	// gateway-originated dialogues — silently ignored, the gateway side is
	// responsible for that state machine.
	if tcapMsg.Transaction == nil || tcapMsg.Transaction.Type.Code() != gotcap.Begin {
		return
	}
	if tcapMsg.Components == nil || len(tcapMsg.Components.Component) == 0 {
		p.logger.Warn("BEGIN without components — ignoring")
		return
	}
	otidValue := tcapMsg.Transaction.OrigTransactionID
	if otidValue == nil || len(otidValue.Value) == 0 {
		p.logger.Warn("BEGIN without OTID — ignoring")
		return
	}

	// First Invoke wins (single-component dialogues per gateway today).
	for _, comp := range tcapMsg.Components.Component {
		if comp.Type.Code() != gotcap.Invoke {
			continue
		}
		opCode := uint8(0)
		if comp.OperationCode != nil && len(comp.OperationCode.Value) > 0 {
			opCode = comp.OperationCode.Value[0]
		}
		invokeID := uint8(0)
		if comp.InvokeID != nil && len(comp.InvokeID.Value) > 0 {
			invokeID = comp.InvokeID.Value[0]
		}

		paramBytes := []byte(nil)
		if comp.Parameter != nil {
			paramBytes = comp.Parameter.Value
		}

		otid := bytesToUint32(otidValue.Value)
		if supported, ok := p.checkApplicationContext(tcapMsg, int32(opCode)); !ok {
			p.sendACNotSupported(conn, pd, sccpMsg, otidValue.Value, supported)
			return
		}
		switch int32(opCode) {
		case int32(mapprotocol.OpSendRoutingInfoForSM):
			if p.cfg.OnSRIPreDrop != nil && p.cfg.OnSRIPreDrop(otid) {
				p.sriRequests.Add(1)
				p.sriErrors.Add(1)
				p.logger.Info("SRI pre-drop hook fired — silently dropping (no response)",
					zap.Uint32("otid", otid),
					zap.Uint8("node_id", uint8(otid>>24)))
				return
			}
			p.handleSRIInvoke(conn, pd, sccpMsg, tcapMsg, otidValue.Value, invokeID, paramBytes, otid)
		case int32(mapprotocol.OpMtForwardSM):
			if p.cfg.OnMtForwardSM == nil {
				p.logger.Debug("MT-Forward-SM Invoke received but OnMtForwardSM not configured — dropping",
					zap.Uint8("op_code", opCode))
				return
			}
			if p.cfg.OnMtForwardSMPreDrop != nil && p.cfg.OnMtForwardSMPreDrop(otid) {
				p.mtFwdRequests.Add(1)
				p.mtFwdErrors.Add(1)
				p.logger.Info("MT-Forward-SM pre-drop hook fired — silently dropping (no response)",
					zap.Uint32("otid", otid),
					zap.Uint8("node_id", uint8(otid>>24)))
				return
			}
			p.handleMtForwardSMInvoke(conn, pd, sccpMsg, tcapMsg, otidValue.Value, invokeID, paramBytes, otid)
		default:
			p.logger.Debug("Ignoring unsupported MAP op",
				zap.Uint8("op_code", opCode))
		}
		return
	}
}

// handleSRIInvoke decodes the SRI-for-SM parameter, runs OnSRI, and
// writes back END + AARE + ReturnResult on the same association.
func (p *Peer) handleSRIInvoke(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *gotcap.TCAP,
	otidBytes []byte,
	invokeID uint8,
	paramBytes []byte,
	otid uint32,
) {
	p.sriRequests.Add(1)

	req, err := mapprotocol.DecodeSendRoutingInfoForSMArg(paramBytes)
	if err != nil {
		p.logger.Warn("SRI decode failed", zap.Error(err))
		p.sriErrors.Add(1)
		// Send back ABORT so the gateway dialogue doesn't time out.
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}

	calledGT := gtDigits(sccpReq.CalledAddress)
	callingGT := gtDigits(sccpReq.CallingAddress)
	resp, cbErr := p.cfg.OnSRI(req, calledGT, callingGT, otid)
	if cbErr != nil || resp == nil {
		p.logger.Info("OnSRI returned error/nil — sending ReturnError",
			zap.Stringp("err", errStr(cbErr)))
		p.sriErrors.Add(1)
		p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, returnErrorCode(cbErr))
		return
	}

	respParam, err := mapprotocol.EncodeSendRoutingInfoForSMRes(resp)
	if err != nil {
		p.logger.Warn("SRI response encode failed", zap.Error(err))
		p.sriErrors.Add(1)
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}

	p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID,
		int32(mapprotocol.OpSendRoutingInfoForSM), respParam)
	p.sriResponses.Add(1)
}

// handleMtForwardSMInvoke decodes the MT-Forward-SM parameter, runs the
// user-supplied OnMtForwardSM callback with the typed request plus the
// SCCP-layer GT digits the gateway placed in CdPA / CgPA, and writes back
// END + AARE + ReturnResult on the same association. Mirrors handleSRIInvoke
// in shape — the only meaningful difference is that the callback receives
// CdPA/CgPA GT digits so tests can assert on the gateway's address-
// construction (option B per the slice plan).
func (p *Peer) handleMtForwardSMInvoke(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *gotcap.TCAP,
	otidBytes []byte,
	invokeID uint8,
	paramBytes []byte,
	otid uint32,
) {
	p.mtFwdRequests.Add(1)

	req, err := mapprotocol.DecodeMtForwardSMArg(paramBytes)
	if err != nil {
		p.logger.Warn("MT-Forward-SM decode failed", zap.Error(err))
		p.mtFwdErrors.Add(1)
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}

	calledGT := gtDigits(sccpReq.CalledAddress)
	callingGT := gtDigits(sccpReq.CallingAddress)
	resp, cbErr := p.cfg.OnMtForwardSM(req, calledGT, callingGT, otid)

	if cbErr != nil || resp == nil {
		p.logger.Info("OnMtForwardSM returned error/nil — sending ReturnError",
			zap.Stringp("err", errStr(cbErr)))
		p.mtFwdErrors.Add(1)
		p.sendReturnError(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, returnErrorCode(cbErr))
		return
	}

	respParam, err := mapprotocol.EncodeMtForwardSMRes(resp)
	if err != nil {
		p.logger.Warn("MT-Forward-SM response encode failed", zap.Error(err))
		p.mtFwdErrors.Add(1)
		p.sendAbort(conn, pd, sccpReq, otidBytes)
		return
	}

	p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID,
		int32(mapprotocol.OpMtForwardSM), respParam)
	p.mtFwdResponses.Add(1)
}

// sendEndReturnResult builds an END + AARE dialogue + ReturnResultLast
// component for the in-flight transaction and writes it back on the
// same M3UA association with OPC/DPC swapped.
//
// AARE context/version: parsed from the inbound BEGIN's Dialogue
// ApplicationContextName so we mirror exactly what the gateway sent
// (including any non-spec ctx values produced by the gateway encoder).
func (p *Peer) sendEndReturnResult(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *gotcap.TCAP,
	otidBytes []byte,
	invokeID uint8,
	opCode int32,
	respParam []byte,
) {
	dtid := bytesToUint32(otidBytes)
	ctx, ctxver := dialogueCtxVerFromBegin(tcapReq)

	endTCAP := gotcap.NewEndReturnResultWithDialogue(
		dtid, gotcap.DialogueAsID,
		ctx, ctxver,
		int(invokeID), int(opCode),
		true, // isLast
		respParam,
	)
	// Override the dialogue PDU type from default AARQ to AARE — the
	// constructor uses NewBeginInvokeWithDialogue's AARQ template; for
	// END responses we need AARE per Q.773.
	if endTCAP.Dialogue != nil {
		endTCAP.Dialogue.DialoguePDU = gotcap.NewAARE(1, ctx, ctxver, gotcap.Accepted, gotcap.DialogueServiceUser, gotcap.Null)
		// Reset SingleAsn1Type length to match new PDU.
		if endTCAP.Dialogue.SingleAsn1Type != nil {
			endTCAP.Dialogue.SingleAsn1Type.Length = uint8(endTCAP.Dialogue.DialoguePDU.MarshalLen())
		}
		endTCAP.Dialogue.SetLength()
	}
	endTCAP.SetLength()

	tcapBytes, err := endTCAP.MarshalBinary()
	if err != nil {
		p.logger.Warn("END encode failed", zap.Error(err))
		return
	}

	p.writeSCCPResponse(conn, pd, sccpReq, tcapBytes)
}

// sendReturnError emits an END + AARE + ReturnError. Same wire shape as
// the success path, just a different component type.
func (p *Peer) sendReturnError(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapReq *gotcap.TCAP,
	otidBytes []byte,
	invokeID uint8,
	errCode int32,
) {
	dtid := bytesToUint32(otidBytes)
	ctx, ctxver := dialogueCtxVerFromBegin(tcapReq)

	// go-tcap doesn't have a NewEndReturnError helper — assemble manually.
	tx := gotcap.NewEnd(dtid, []byte{})
	dlg := gotcap.NewDialogue(gotcap.DialogueAsID, 1,
		gotcap.NewAARE(1, ctx, ctxver, gotcap.Accepted, gotcap.DialogueServiceUser, gotcap.Null),
		nil)
	errComp := gotcap.NewReturnError(int(invokeID), int(errCode), true, nil)

	tcapMsg := &gotcap.TCAP{
		Transaction: tx,
		Dialogue:    dlg,
		Components:  gotcap.NewComponents(errComp),
	}
	tcapMsg.SetLength()

	tcapBytes, err := tcapMsg.MarshalBinary()
	if err != nil {
		p.logger.Warn("ReturnError encode failed", zap.Error(err))
		return
	}

	p.writeSCCPResponse(conn, pd, sccpReq, tcapBytes)
}

// sendAbort emits a TCAP P-Abort. Gateway should clean up the dialogue.
func (p *Peer) sendAbort(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	otidBytes []byte,
) {
	dtid := bytesToUint32(otidBytes)
	tx := gotcap.NewAbort(dtid, 0 /* unrecognizedTransaction */, []byte{})
	tcapMsg := &gotcap.TCAP{Transaction: tx}
	tcapMsg.SetLength()

	tcapBytes, err := tcapMsg.MarshalBinary()
	if err != nil {
		p.logger.Warn("Abort encode failed", zap.Error(err))
		return
	}
	p.writeSCCPResponse(conn, pd, sccpReq, tcapBytes)
}

// writeSCCPResponse wraps a TCAP payload in SCCP UDT and writes it back
// on the M3UA association. Addresses are swapped from the request:
// CdPA ← request CgPA, CgPA ← request CdPA. OPC/DPC swap is handled by
// constructing a fresh ProtocolData with peer-OPC and the request's OPC
// as DPC.
func (p *Peer) writeSCCPResponse(
	conn *m3ua.Conn,
	pd *params.ProtocolDataPayload,
	sccpReq *sccp.SccpMessage,
	tcapBytes []byte,
) {
	resp := &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		ProtocolClass:  sccpReq.ProtocolClass,
		CalledAddress:  sccpReq.CallingAddress, // peer → originator
		CallingAddress: sccpReq.CalledAddress,  // peer = HLR (was CdPA)
		Data:           tcapBytes,
	}
	sccpBytes, err := sccp.EncodeSCCP(resp, p.cfg.NetworkVariant)
	if err != nil {
		p.logger.Warn("SCCP encode failed", zap.Error(err))
		return
	}

	// Build MTP3 ProtocolData with OPC/DPC swapped from the request.
	// SLS preserved so SCTP stream selection on the response side stays
	// consistent (wmnsk/go-m3ua hashes SLS into stream choice).
	respPD := params.NewProtocolData(
		p.cfg.PointCode,         // OPC = peer
		pd.OriginatingPointCode, // DPC = gateway (was OPC of request)
		pd.ServiceIndicator,
		pd.NetworkIndicator,
		pd.MessagePriority,
		pd.SignalingLinkSelection,
		sccpBytes,
	)
	if _, err := conn.WritePD(respPD); err != nil {
		p.logger.Warn("WritePD failed", zap.Error(err))
		return
	}
}

// dialogueCtxVerFromBegin parses the AARQ ApplicationContextName from the
// inbound BEGIN and returns its last two arcs as ctx,ctxver. The peer
// must mirror exactly the AC short-ID and version the SUT sent, otherwise
// the AARE we emit on the END will not match any registered handler in
// the gateway's TCAP dispatcher.
//
// IMPORTANT: read from DialoguePDU.ApplicationContextName.Value (the AC
// OID, encoded as [0x06, 0x07, 4, 0, 0, 1, 0, ctx, ver]), NOT from
// Dialogue.ObjectIdentifier (which is the Dialogue-As-ID, always
// [0,17,134,5,1,1,1] — using it would always yield ctx=1, ver=1).
func dialogueCtxVerFromBegin(tcapMsg *gotcap.TCAP) (uint8, uint8) {
	const defaultCtx, defaultVer uint8 = 1, 3
	if tcapMsg.Dialogue == nil || tcapMsg.Dialogue.DialoguePDU == nil {
		return defaultCtx, defaultVer
	}
	acn := tcapMsg.Dialogue.DialoguePDU.ApplicationContextName
	if acn == nil {
		return defaultCtx, defaultVer
	}
	v := acn.Value
	if len(v) < 2 {
		return defaultCtx, defaultVer
	}
	return v[len(v)-2], v[len(v)-1]
}

func bytesToUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// MAPError makes a peer callback answer with a specific MAP error code
// (3GPP TS 29.002 MAP-ErrorCodes) instead of systemFailure.
type MAPError struct{ Code mapprotocol.ErrorCode }

func (e MAPError) Error() string { return "MAP error " + e.Code.String() }

func returnErrorCode(err error) int32 {
	var me MAPError
	if errors.As(err, &me) {
		return int32(me.Code)
	}
	return int32(mapprotocol.ErrSystemFailure)
}

func errStr(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

// gtDigits returns the GT address digits from an SCCP address, or "" when
// the address routes on PC+SSN only (no GT in AddressIndicator). Used to
// surface gateway address-construction to MT-Forward-SM test callbacks.
func gtDigits(addr *sccp.SccpAddress) string {
	if addr == nil || addr.GlobalTitle == nil {
		return ""
	}
	return addr.GlobalTitle.Address
}

// --- conn registry ----------------------------------------------------

// registerConn appends a freshly-accepted association to the active list.
// Called at acceptLoop entry (after Accept returns successfully).
func (p *Peer) registerConn(conn *m3ua.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	p.conns = append(p.conns, conn)
}

// deregisterConn removes a conn that has exited its connHandler. Pruning
// is done by pointer identity — the wmnsk M3UA Conn is reused by reference
// throughout the connection's lifetime, so this is reliable.
func (p *Peer) deregisterConn(conn *m3ua.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	for i, c := range p.conns {
		if c == conn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			return
		}
	}
}

// AcceptedConns returns a snapshot of currently-active M3UA associations.
// Safe under concurrent registration / deregistration. The slice is a
// fresh allocation — caller may iterate without holding a lock.
//
// Cluster tests use this for explicit conn selection (e.g. round-robin
// MO-Fwd origination across cluster members). Returned conns are valid
// only at the moment of the call; a node restart would mutate the list.
func (p *Peer) AcceptedConns() []*m3ua.Conn {
	p.connsMu.RLock()
	defer p.connsMu.RUnlock()
	out := make([]*m3ua.Conn, len(p.conns))
	copy(out, p.conns)
	return out
}

// WaitForConns blocks until at least `n` active associations are
// registered or `ctx` cancels. Polls every 50ms — the M3UA ASPAC
// handshake completes in ~50-100ms on loopback so this resolves
// quickly under normal conditions and the polling cost is negligible.
//
// Returns ctx.Err() on timeout (caller decides whether that's fatal).
// Returning nil only guarantees the count was met at the moment of
// observation; a node death immediately after could drop the count
// below n. Real cluster tests pair this with explicit kill-test
// asserts that already account for transient state.
func (p *Peer) WaitForConns(ctx context.Context, n int) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		p.connsMu.RLock()
		got := len(p.conns)
		p.connsMu.RUnlock()
		if got >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ss7peer: waiting for %d conns, got %d: %w", n, got, ctx.Err())
		case <-tick.C:
		}
	}
}

// MOForwardSMOriginated returns the count of MO-Forward-SM dialogues
// the peer has SUCCESSFULLY originated (BEGIN written to the wire).
// Failed origination attempts (encode error, conn closed, write error)
// do not increment.
func (p *Peer) MOForwardSMOriginated() uint64 { return p.moFwdOriginated.Load() }
