// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

// Package ss7peer provides an in-process SS7 peer that the gateway under
// test connects to over real SCTP/M3UA. It speaks SCCP/TCAP/MAP with the
// gateway's own codecs (`internal/sccp`, `internal/tcap`,
// `internal/mapprotocol`). A shared codec cannot show a defect both sides
// share — a TCAP library that wrapped every parameter in an extra SEQUENCE
// went unnoticed that way — so each codec is also checked against frames
// built by hand from the standard (ber_test.go, location_test.go,
// routing_test.go); the peer checks what the frames carry.
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
//	                NetworkNodeNumber: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: "905304445566"},
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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/ishidawataru/sctp"
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

	// OnSRI is the callback invoked on every SRI-for-SM Invoke. Required.
	OnSRI SRICallback

	// OnMtForwardSM is the callback invoked on every MT-Forward-SM Invoke.
	// Optional — when nil, the peer drops op 44 messages silently and the
	// gateway dialogue eventually times out (P-Abort upstream). Tests that
	// exercise the SMS delivery path MUST set this.
	OnMtForwardSM MtForwardSMCallback

	// OnATI and OnPSI answer AnyTimeInterrogation (op 71) and
	// ProvideSubscriberInfo (op 70). Optional: when nil, the peer counts and
	// drops them.
	OnATI ATICallback
	OnPSI PSICallback
	// OnRouting answers SendRoutingInfo (op 22) as an HLR; nil drops it.
	OnRouting RoutingCallback

	// OnSRIPreDrop is consulted before OnSRI is invoked. When it returns
	// true, the peer drops the SRI Invoke silently (no ReturnResult, no
	// ReturnError, no Abort) — exclusively for exercising the TCAP TIMEOUT
	// failure path in cluster outage tests. Optional; nil disables.
	OnSRIPreDrop PreDropHook

	// OnMtForwardSMPreDrop — same semantic as OnSRIPreDrop, for op 44.
	OnMtForwardSMPreDrop PreDropHook

	// RefuseContexts makes the peer behave like a node that supports only
	// version 2 of every context: each BEGIN is answered with a TC-U-ABORT
	// whose AARE refuses the proposed context and proposes version 2 of it
	// (TS 29.002 §12.1). Exercises the gateway's handling of a refused
	// dialogue.
	RefuseContexts bool

	// ReturnUDTS makes the peer behave like an SCCP node that cannot deliver
	// anything: every UDT the gateway sends comes back as a UDTS with
	// UDTSCause (Q.714 §4.2), carrying the gateway's own message.
	ReturnUDTS bool
	UDTSCause  sccp.ReturnCause

	// OnlyVersion2 makes the peer behave like a node that supports version 2
	// of the short-message contexts only: a version 3 BEGIN is refused with
	// an AARE proposing version 2 (TS 29.002 §5.2.1); a version 2 BEGIN is
	// served — SRI-for-SM under shortMsgGateway v2, and MT delivery as
	// forwardSM (op 46) under shortMsgMT-Relay v2, answered without a result
	// parameter.
	OnlyVersion2 bool

	// SegmentAnswers > 1 makes the peer send each answer as that many XUDT
	// segments (Q.714 §4.1.1.1.2), as a node with a long answer may.
	SegmentAnswers int

	// Logger is optional; defaults to zap.NewNop().
	Logger *zap.Logger
}

// Peer is the running in-process HLR / SG-equivalent harness.
type Peer struct {
	cfg       Config
	listener  *wire.Listener
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
	conns   []*wire.Conn

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
	atiRequests     atomic.Uint64
	psiRequests     atomic.Uint64
	routingRequests atomic.Uint64
	connsAccepted   atomic.Uint64

	violationsMu sync.Mutex
	violations   []string

	// Dialogues opened by an empty Begin and accepted, keyed by the
	// gateway's OTID, waiting for the CONTINUE with the components.
	openDialogues sync.Map // uint32 → emptyBeginDialogue
	emptyBegins   atomic.Uint64
	segmentRef    atomic.Uint32
	v2Dialogues   atomic.Uint64
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
	// The SG side answers the gateway's ASP Up and ASP Active with the
	// gateway's own M3UA codec (internal/m3ua/wire), which is checked
	// against frames built from RFC 4666.
	listener, err := wire.Listen(sctpAddr, wire.Config{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("ss7peer: M3UA listen: %w", err)
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
// acceptLoop takes each association and serves it in a goroutine of its
// own, the ASP handshake included, so an ASP that never completes it holds
// back no other.
func (p *Peer) acceptLoop(ctx context.Context) {
	defer p.wg.Done()

	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Debug("Accept ended", zap.Error(err))
			}
			return
		}
		p.connsAccepted.Add(1)
		p.wg.Add(1)
		go p.connHandler(ctx, conn)
	}
}

// connHandler completes the ASP handshake, then runs the association's
// read loop: ReadData → SCCP/TCAP/MAP decode → callback → response encode
// → WriteData, in the order the gateway sent. Shutdown: ctx closes the
// association, which ends ReadData.
func (p *Peer) connHandler(ctx context.Context, conn *wire.Conn) {
	defer p.wg.Done()
	defer conn.Close()

	closeOnce := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-closeOnce:
		}
	}()
	defer close(closeOnce)

	if err := conn.Handshake(); err != nil {
		if ctx.Err() == nil {
			p.logger.Debug("ASP handshake failed", zap.Error(err))
		}
		return
	}
	p.registerConn(conn)
	defer p.deregisterConn(conn)
	p.logger.Info("M3UA association active", zap.String("remote", conn.RemoteAddr().String()))

	for {
		pd, err := conn.ReadData()
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Debug("ReadData ended", zap.Error(err))
			}
			return
		}
		p.handleMTP3(conn, pd)
	}
}

// handleMTP3 decodes one inbound MTP3 payload through SCCP → TCAP →
// MAP, dispatches to the user callback, and writes the response back on
// the same association with OPC/DPC swapped.
func (p *Peer) handleMTP3(conn *wire.Conn, pd *wire.ProtocolData) {
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
	if p.cfg.ReturnUDTS {
		if !sccpMsg.ReturnOnError {
			p.violate("UDT without the return option: a UDTS may not be sent for it")
			return
		}
		p.writeSCCP(conn, pd, &sccp.SccpMessage{
			MessageType:    sccp.MsgTypeUDTS,
			ReturnCause:    p.cfg.UDTSCause,
			CalledAddress:  sccpMsg.CallingAddress,
			CallingAddress: sccpMsg.CalledAddress,
			Data:           sccpMsg.Data,
		})
		return
	}

	tcapMsg, err := tcap.ParseMessage(sccpMsg.Data)
	if err != nil {
		p.violate("TCAP message not Q.773: %v", err)
		return
	}

	// The peer serves a BEGIN carrying an Invoke, or — after an empty Begin
	// it accepted (TS 29.002 §C.2.1) — the CONTINUE carrying it. END and
	// ABORT need no answer.
	otidBytes := tcapMsg.OriginatingTID
	if len(otidBytes) == 0 {
		return
	}
	var acValue []byte
	switch tcapMsg.MessageType {
	case tcap.MsgTypeBegin:
		acValue = acValueOf(tcapMsg)
		if len(tcapMsg.Components) == 0 {
			p.acceptEmptyBegin(conn, pd, sccpMsg, otidBytes, acValue)
			return
		}
	case tcap.MsgTypeContinue:
		var ok bool
		if acValue, ok = p.continueAfterEmptyBegin(tcapMsg, otidBytes); !ok {
			return
		}
	default:
		return
	}

	// First Invoke wins (single-component dialogues per gateway today).
	for _, comp := range tcapMsg.Components {
		if comp.Type != tcap.CompTypeInvoke || comp.OperationCode == nil || comp.InvokeID == nil {
			continue
		}
		opCode := uint8(*comp.OperationCode)
		invokeID := *comp.InvokeID
		paramBytes := comp.Parameter

		otid := bytesToUint32(otidBytes)
		if p.cfg.OnlyVersion2 {
			if len(acValue) >= 2 && acValue[len(acValue)-1] == 3 {
				p.sendACNotSupported(conn, pd, sccpMsg, otidBytes, acOID(acValue[len(acValue)-2], 2))
				return
			}
			want, known := expectedV2ACByOp[int32(opCode)]
			if !known || !bytes.Equal(want, acValue) {
				p.violate("op %d sent under % X in a version 2 dialogue", opCode, acValue)
				p.sendACNotSupported(conn, pd, sccpMsg, otidBytes, acOID(20, 2))
				return
			}
			p.v2Dialogues.Add(1)
			if int32(opCode) == int32(mapprotocol.OpMoForwardSM) { // forwardSM: MT delivery in v2
				p.handleMtForwardSMInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, paramBytes, otid)
				return
			}
			p.handleSRIInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, paramBytes, otid)
			return
		}
		if p.cfg.RefuseContexts {
			if want, known := expectedACByOp[int32(opCode)]; known {
				p.sendACNotSupported(conn, pd, sccpMsg, otidBytes, acOID(want[len(want)-2], 2))
				return
			}
		}
		if supported, ok := p.checkApplicationContext(acValue, int32(opCode)); !ok {
			p.sendACNotSupported(conn, pd, sccpMsg, otidBytes, supported)
			return
		}
		p.checkSubsystems(int32(opCode), sccpMsg.CalledAddress, sccpMsg.CallingAddress)
		switch int32(opCode) {
		case int32(mapprotocol.OpAnyTimeInterrogation), int32(mapprotocol.OpProvideSubscriberInfo):
			p.handleLocationInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, int32(opCode), paramBytes, otid)
		case int32(mapprotocol.OpSendRoutingInfo):
			p.handleRoutingInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, paramBytes, otid)
		case int32(mapprotocol.OpSendRoutingInfoForSM):
			if p.cfg.OnSRIPreDrop != nil && p.cfg.OnSRIPreDrop(otid) {
				p.sriRequests.Add(1)
				p.sriErrors.Add(1)
				p.logger.Info("SRI pre-drop hook fired — silently dropping (no response)",
					zap.Uint32("otid", otid),
					zap.Uint8("node_id", uint8(otid>>24)))
				return
			}
			p.handleSRIInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, paramBytes, otid)
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
			p.handleMtForwardSMInvoke(conn, pd, sccpMsg, tcapMsg, otidBytes, invokeID, paramBytes, otid)
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
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
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
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
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
	op := int32(mapprotocol.OpMtForwardSM)
	if p.cfg.OnlyVersion2 {
		op, respParam = int32(mapprotocol.OpMoForwardSM), nil // forwardSM: no result parameter
	}

	p.sendEndReturnResult(conn, pd, sccpReq, tcapReq, otidBytes, invokeID, op, respParam)
	p.mtFwdResponses.Add(1)
}

// sendEndReturnResult answers the in-flight transaction with an END
// carrying a Return Result Last — and an AARE accepting the context when the
// request was the BEGIN that opened the dialogue.
func (p *Peer) sendEndReturnResult(
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
	opCode int32,
	respParam []byte,
) {
	p.sendTCAP(conn, pd, sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: otidBytes, DialoguePortion: aareFor(tcapReq),
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnResult, InvokeID: i32(invokeID),
			OperationCode: i32(opCode), Parameter: respParam}},
	})
}

// sendReturnError answers with an END carrying a Return Error, the same
// shape as the success path.
func (p *Peer) sendReturnError(
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	tcapReq *tcap.Message,
	otidBytes []byte,
	invokeID int32,
	errCode int32,
) {
	p.sendTCAP(conn, pd, sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeEnd, DestinationTID: otidBytes, DialoguePortion: aareFor(tcapReq),
		Components: []*tcap.Component{{Type: tcap.CompTypeReturnError, InvokeID: i32(invokeID), ErrorCode: i32(errCode)}},
	})
}

// sendAbort emits a P-ABORT (unrecognizedTransactionID). The gateway
// cleans up the dialogue.
func (p *Peer) sendAbort(
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	otidBytes []byte,
) {
	cause := tcap.AbortUnrecognizedTransactionID
	p.sendTCAP(conn, pd, sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeAbort, DestinationTID: otidBytes, PAbort: true, AbortCause: &cause,
	})
}

// writeSCCPResponse wraps a TCAP payload in SCCP UDT and writes it back
// on the M3UA association. Addresses are swapped from the request:
// CdPA ← request CgPA, CgPA ← request CdPA. OPC/DPC swap is handled by
// constructing a fresh ProtocolData with peer-OPC and the request's OPC
// as DPC.
func (p *Peer) writeSCCPResponse(
	conn *wire.Conn,
	pd *wire.ProtocolData,
	sccpReq *sccp.SccpMessage,
	tcapBytes []byte,
) {
	if n := p.cfg.SegmentAnswers; n > 1 {
		ref := p.segmentRef.Add(1) & 0xFFFFFF
		size := (len(tcapBytes) + n - 1) / n
		for i := 0; i < n; i++ {
			p.writeSCCP(conn, pd, &sccp.SccpMessage{
				MessageType:    sccp.MsgTypeXUDT,
				ProtocolClass:  sccp.ClassInSequenceConnectionless, // segments travel as class 1
				HopCounter:     15,
				CalledAddress:  sccpReq.CallingAddress,
				CallingAddress: sccpReq.CalledAddress,
				Data:           tcapBytes[i*size : min((i+1)*size, len(tcapBytes))],
				Segmentation:   &sccp.Segmentation{First: i == 0, Remaining: uint8(n - 1 - i), LocalRef: ref},
			})
		}
		return
	}
	p.writeSCCP(conn, pd, &sccp.SccpMessage{
		MessageType:    sccp.MsgTypeUDT,
		ProtocolClass:  sccpReq.ProtocolClass,
		CalledAddress:  sccpReq.CallingAddress, // peer → originator
		CallingAddress: sccpReq.CalledAddress,  // peer = HLR (was CdPA)
		Data:           tcapBytes,
	})
}

// writeSCCP encodes an SCCP message and writes it back on the association
// the request arrived on, with the routing label's OPC/DPC swapped.
func (p *Peer) writeSCCP(conn *wire.Conn, pd *wire.ProtocolData, resp *sccp.SccpMessage) {
	sccpBytes, err := sccp.EncodeSCCP(resp, p.cfg.NetworkVariant)
	if err != nil {
		p.logger.Warn("SCCP encode failed", zap.Error(err))
		return
	}

	// OPC/DPC swapped from the request; the SLS, and with it the stream,
	// kept, so the answers of one transaction stay in sequence.
	respPD := &wire.ProtocolData{
		OPC: p.cfg.PointCode, DPC: pd.OPC, SI: pd.SI, NI: pd.NI, MP: pd.MP, SLS: pd.SLS,
		Data: sccpBytes,
	}
	stream := uint16(1)
	if max := conn.MaxStream(); max > 0 {
		stream = 1 + uint16(pd.SLS)%max
	}
	if _, err := conn.WriteData(respPD, stream); err != nil {
		p.logger.Warn("WriteData failed", zap.Error(err))
		return
	}
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
func (p *Peer) registerConn(conn *wire.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	p.conns = append(p.conns, conn)
}

// deregisterConn removes a conn that has exited its connHandler. Pruning
// is done by pointer identity: one *wire.Conn per association, for its
// whole lifetime.
func (p *Peer) deregisterConn(conn *wire.Conn) {
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
func (p *Peer) AcceptedConns() []*wire.Conn {
	p.connsMu.RLock()
	defer p.connsMu.RUnlock()
	out := make([]*wire.Conn, len(p.conns))
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

// EmptyBegins returns how many dialogues the gateway opened with an empty
// Begin (TS 29.002 §C.2.1) that the peer accepted.
func (p *Peer) EmptyBegins() uint64 { return p.emptyBegins.Load() }

type emptyBeginDialogue struct {
	peerTID uint32
	acValue []byte
}

// acceptEmptyBegin answers a BEGIN that carries the dialogue portion alone
// with a CONTINUE whose AARE accepts the context, as an HLR or MSC does.
func (p *Peer) acceptEmptyBegin(conn *wire.Conn, pd *wire.ProtocolData, sccpReq *sccp.SccpMessage, otid, acValue []byte) {
	if len(acValue) < 2 {
		p.violate("empty BEGIN without an application context")
		return
	}
	known := false
	for _, want := range expectedACByOp {
		known = known || bytes.Equal(want, acValue)
	}
	if !known || p.cfg.RefuseContexts {
		if known {
			acValue = acOID(acValue[len(acValue)-2], 2)
		}
		p.sendACNotSupported(conn, pd, sccpReq, otid, acValue)
		return
	}
	peerTID := p.originateTID.Add(1)
	p.openDialogues.Store(bytesToUint32(otid), emptyBeginDialogue{peerTID: peerTID, acValue: acValue})
	p.emptyBegins.Add(1)

	p.sendTCAP(conn, pd, sccpReq, &tcap.Message{
		MessageType: tcap.MsgTypeContinue, OriginatingTID: tidBytes(peerTID), DestinationTID: otid,
		DialoguePortion: &tcap.DialoguePortion{ApplicationContext: &tcap.ApplicationContext{OID: acOIDOf(acValue)}},
	})
}

// continueAfterEmptyBegin checks the gateway's second forward message: it
// must address the peer's TID, carry the components, and no dialogue
// portion (that was settled by the AARE). It returns the dialogue's context.
func (p *Peer) continueAfterEmptyBegin(tcapMsg *tcap.Message, otid []byte) ([]byte, bool) {
	v, ok := p.openDialogues.LoadAndDelete(bytesToUint32(otid))
	if !ok {
		p.violate("CONTINUE for a dialogue the peer never accepted (OTID % X)", otid)
		return nil, false
	}
	d := v.(emptyBeginDialogue)
	if bytesToUint32(tcapMsg.DestinationTID) != d.peerTID {
		p.violate("CONTINUE does not address the peer's TID")
		return nil, false
	}
	if tcapMsg.DialoguePortion != nil {
		p.violate("second forward message carries a dialogue portion")
	}
	if len(tcapMsg.Components) == 0 {
		p.violate("CONTINUE after an empty Begin carries no components")
		return nil, false
	}
	return d.acValue, true
}

// V2Dialogues returns how many version 2 dialogues the peer served
// (OnlyVersion2).
func (p *Peer) V2Dialogues() uint64 { return p.v2Dialogues.Load() }
