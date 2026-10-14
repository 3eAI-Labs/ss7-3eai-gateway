// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// E2E integration test: NATS request → gateway → in-process HLR peer →
// gateway → NATS response. Exercises the full M3UA/SCCP/TCAP/MAP wire
// path (real SCTP loopback association) against the boot path used in
// production (initializeHandlers + startServices). Build tag keeps it
// out of `go test ./...`; run via `go test -tags integration ./cmd/gateway/`
// or `make test-e2e`.
//
// Why a build tag: SCTP requires kernel support (Linux ✓, macOS ✗) and
// the test takes ~3-5s to set up — too heavy for the unit suite. CI
// gates on a separate `e2e` workflow that runs `make test-e2e` only on
// ubuntu runners.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

const (
	// ANSI 24-bit point codes — exercises the production SCCP codec width
	// (Q.713 §3.4.1 ANSI variant, 3-octet PC) so the E2E proves the same
	// path real ANSI carrier deployments take. Matches config.example.yaml.
	e2eGatewayPC uint32 = 0x010101 // 1-1-1 (ANSI 24-bit, gateway local PC)
	e2ePeerPC    uint32 = 0x020202 // 2-2-2 (ANSI 24-bit, peer is the HLR/SGW)

	// e2eNetworkIndicator is the MTP3 NI both sides agree on (2 = national).
	e2eNetworkIndicator uint8 = 2

	e2eTestIMSI   = "286010000000123"
	e2eTestVMSC   = "905304445566"
	e2eTestMSISDN = "905321112233"
	e2eTestSCA    = "905001112233" // service centre address
	// e2eTestOwnGT is the cluster-shared GT the E2E gateway advertises in
	// CgPA. Exercises the GT-based source addressing path — peers expect
	// to see this on every outbound transaction so HA failover (which
	// swaps GT ownership between cluster nodes) is transparent.
	e2eTestOwnGT = "905900000001"
)

// e2eFakeTPDU is a minimal SMS-DELIVER TPDU placeholder. The gateway is
// transparent to TPDU contents (TS 23.040) — it carries SMRPUI as opaque
// OCTET STRING. We only need enough bytes that codec encode/decode round-
// trip cleanly; correctness of the TPDU itself is the SMSC's concern, not
// the gateway's.
var e2eFakeTPDU = []byte{0x04, 0x0B, 0x91, 0x90, 0x53, 0x21, 0x11, 0x22, 0x33, 0x00, 0x00, 0x05, 0x48, 0x65, 0x6C, 0x6C, 0x6F}

// TestE2E_SRIForSM_RoundTrip drives the full SS7 path:
//
//	NATS map.sri.request → MAP encode → TCAP BEGIN+Invoke →
//	SCCP UDT → M3UA → SCTP → ss7peer (HLR) →
//	OnSRI callback → MAP encode response → TCAP END+ReturnResult →
//	SCCP UDT → M3UA → gateway → MAP decode → response sink →
//	NATS map.sri.response
//
// Asserts the response IMSI and VMSC match what the peer returned.
func TestE2E_SRIForSM_RoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// 1) Embedded NATS — random client port for hermetic parallelism.
	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	// 2) HLR peer — random SCTP port. OnSRI callback fixes the IMSI/VMSC
	//    we'll assert on the NATS response side.
	peerAddr := allocLoopbackSCTPPort(t)
	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         peerAddr,
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, calledGT, callingGT string, otid uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			t.Logf("ss7peer received SRI for MSISDN=%s SCA=%s called_gt=%s calling_gt=%s otid=0x%08x",
				string(req.MSISDN), req.ServiceCentreAddress.Digits, calledGT, callingGT, otid)
			// CgPA must carry the configured cluster-shared own_gt so the
			// HLR can route the response back via GT (HA-friendly path).
			require.Equal(t, e2eTestOwnGT, callingGT,
				"CgPA GT must equal sccp.local_global_title.address — own-GT regression")
			// map.sri_routing default "msisdn": the subscriber is the called GT.
			require.Equal(t, e2eTestMSISDN, calledGT, "SRI-for-SM must be addressed to the MSISDN")
			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{
						Nature:     mapprotocol.NatureInternational,
						NumberPlan: 1, // ISDN
						Digits:     e2eTestVMSC,
					},
				},
			}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	// 3) Gateway — full boot via the same code path main() uses.
	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	// 4) NATS client — subscribe BEFORE publishing so the response can't
	//    race past us. Use a buffered channel so SubscribeChan keeps order.
	nc, err := nats.Connect(natsServer.ClientURL(),
		nats.Name("e2e-test-client"),
		nats.Timeout(5*time.Second))
	require.NoError(t, err, "nats client connect")
	defer nc.Close()

	respCh := make(chan *nats.Msg, 4)
	sub, err := nc.ChanSubscribe(natsbridge.SubjectSRIResponse, respCh)
	require.NoError(t, err, "subscribe SRI response")
	defer sub.Unsubscribe()

	// 5) Publish SRI request. correlation_id is what we'll match on the
	//    response — gateway threads it through the dialogue and republishes
	//    on the response sink.
	const corrID = "e2e-sri-001"
	reqBytes, err := json.Marshal(&natsbridge.NatsMessage{
		MessageID:     corrID,
		CorrelationID: corrID,
		MSISDN:        e2eTestMSISDN,
		Sender:        e2eTestSCA, // bridge falls back to Sender for SCA
		Parameters: map[string]interface{}{
			"service_centre_address": e2eTestSCA,
		},
	})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(natsbridge.SubjectSRIRequest, reqBytes), "publish SRI request")

	// 6) Wait for response. 8s budget covers M3UA association handshake
	//    (~1-2s warm) + TCAP round trip (<100ms) with comfortable margin
	//    for slow CI runners.
	select {
	case msg := <-respCh:
		var resp natsbridge.NatsMessage
		require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode SRI response")
		t.Logf("E2E SRI response: corr=%s imsi=%s vmsc=%s",
			resp.CorrelationID, resp.IMSI,
			paramString(resp.Parameters, "vmsc_address"))

		require.Equal(t, corrID, resp.CorrelationID, "correlation_id round-trip")
		require.Equal(t, e2eTestIMSI, resp.IMSI, "IMSI round-trip")
		require.Equal(t, e2eTestVMSC, paramString(resp.Parameters, "vmsc_address"),
			"VMSC round-trip via parameters.vmsc_address")
	case <-time.After(8 * time.Second):
		t.Fatalf("timeout waiting for SRI response on %s; peer SRI requests=%d responses=%d errors=%d",
			natsbridge.SubjectSRIResponse,
			peer.SRIRequests(), peer.SRIResponses(), peer.SRIErrors())
	}

	// Peer-side counter assertions guard against the response coming
	// from somewhere other than the simulator (e.g. a stub publishing
	// without going through the wire).
	require.Equal(t, uint64(1), peer.SRIRequests(), "peer must have decoded exactly one SRI Invoke from the wire")
	require.Equal(t, uint64(1), peer.SRIResponses(), "peer must have written back exactly one SRI ReturnResult")
	require.Equal(t, uint64(0), peer.SRIErrors(), "no peer-side errors")
}

// TestE2E_MtForwardSM_RoundTrip drives the SMS delivery path:
//
//	NATS map.mt.sms.request → MAP encode → TCAP BEGIN+Invoke (op 44) →
//	SCCP UDT (CdPA = VMSC GT) → M3UA → SCTP → ss7peer (VMSC) →
//	OnMtForwardSM callback → MAP encode response → TCAP END+ReturnResult →
//	SCCP UDT → M3UA → gateway → MAP decode → response sink →
//	NATS map.mt.sms.response (status=DELIVERED)
//
// Closes TIER-1 by proving the SMS delivery half of the chain works end-
// to-end, not just the location-lookup half (SRI, covered by the test
// above). With both passing, the gateway can claim "we deliver MT-SMS over
// SS7" with test backing.
func TestE2E_MtForwardSM_RoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peerAddr := allocLoopbackSCTPPort(t)
	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         peerAddr,
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		// SRI not exercised in this test, but the peer still requires it.
		// Returning a stub IMSI/VMSC keeps Start happy; the dispatch switch
		// only enters this path when MT-Fwd-SM (op 44) is received.
		OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, _, _ string, _ uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return &mapprotocol.SendRoutingInfoForSMResponse{IMSI: mapprotocol.IMSI(e2eTestIMSI)}, nil
		},
		// MT-Forward-SM callback. calledGT/callingGT come from the SCCP layer
		// (gateway address-construction), not from MAP — this is the whole
		// point of option B: catch CdPA/CgPA regressions that codec-only
		// checks would miss.
		OnMtForwardSM: func(req *mapprotocol.MtForwardSMRequest, calledGT, callingGT string, _ uint32) (*mapprotocol.MtForwardSMResponse, error) {
			imsi := ""
			if req.SMRPDA.IMSI != nil {
				imsi = string(*req.SMRPDA.IMSI)
			}
			t.Logf("ss7peer received MT-Fwd-SM imsi=%s called_gt=%s calling_gt=%s tpdu_len=%d",
				imsi, calledGT, callingGT, len(req.SMRPUI))

			// CdPA must carry the VMSC GT the NATS caller supplied as
			// network_node_number — proves nats_bridge → calledForGT path.
			require.Equal(t, e2eTestVMSC, calledGT,
				"CdPA GT must match network_node_number — gateway address-construction regression")

			// CgPA must carry the configured cluster-shared own_gt — proves
			// the SCCP own-GT plumbing is wired through nats_bridge.
			require.Equal(t, e2eTestOwnGT, callingGT,
				"CgPA GT must equal sccp.local_global_title.address — own-GT regression")

			// TPDU is opaque to the gateway (TS 23.040) — round-trip must
			// be byte-identical. Catches accidental re-encoding inside the
			// MAP codec or NATS bridge.
			require.Equal(t, e2eFakeTPDU, req.SMRPUI,
				"TPDU pass-through — gateway must not mutate SM-RP-UI")

			require.Equal(t, e2eTestIMSI, imsi,
				"IMSI round-trip from NATS request to MAP SMRPDA")

			// Empty response = TS 29.002 §17.7.4 success → bridge maps to
			// status=DELIVERED on the NATS reply.
			return &mapprotocol.MtForwardSMResponse{}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(),
		nats.Name("e2e-test-client"),
		nats.Timeout(5*time.Second))
	require.NoError(t, err, "nats client connect")
	defer nc.Close()

	respCh := make(chan *nats.Msg, 4)
	sub, err := nc.ChanSubscribe(natsbridge.SubjectMTSmsResponse, respCh)
	require.NoError(t, err, "subscribe MT SMS response")
	defer sub.Unsubscribe()

	// Publish MT SMS request. Bridge requires:
	//   - imsi (subscriber on whose VMSC we deliver — from prior SRI in real flow)
	//   - Parameters[network_node_number] (VMSC GT — used to build CdPA)
	//   - Parameters[service_centre_address] (SCA going into SM-RP-OA)
	//   - PayloadData (TPDU bytes — JSON-marshals to base64 automatically)
	const corrID = "e2e-mt-fwd-001"
	reqBytes, err := json.Marshal(&natsbridge.NatsMessage{
		MessageID:     corrID,
		CorrelationID: corrID,
		IMSI:          e2eTestIMSI,
		PayloadData:   e2eFakeTPDU,
		Parameters: map[string]interface{}{
			"network_node_number":    e2eTestVMSC,
			"service_centre_address": e2eTestSCA,
		},
	})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(natsbridge.SubjectMTSmsRequest, reqBytes), "publish MT SMS request")

	select {
	case msg := <-respCh:
		var resp natsbridge.NatsMessage
		require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode MT SMS response")
		t.Logf("E2E MT-Fwd-SM response: corr=%s status=%s", resp.CorrelationID, resp.Status)

		require.Equal(t, corrID, resp.CorrelationID, "correlation_id round-trip")
		require.Equal(t, "DELIVERED", resp.Status, "MT-Fwd-SM ReturnResult → DELIVERED")
	case <-time.After(8 * time.Second):
		t.Fatalf("timeout waiting for MT SMS response on %s; peer mt_fwd requests=%d responses=%d errors=%d",
			natsbridge.SubjectMTSmsResponse,
			peer.MtForwardSMRequests(), peer.MtForwardSMResponses(), peer.MtForwardSMErrors())
	}

	// Peer-side counter assertions guard against the response coming from
	// somewhere other than the simulator (e.g. a stub publishing without
	// going through the wire).
	require.Equal(t, uint64(1), peer.MtForwardSMRequests(), "peer must have decoded exactly one MT-Fwd-SM Invoke from the wire")
	require.Equal(t, uint64(1), peer.MtForwardSMResponses(), "peer must have written back exactly one MT-Fwd-SM ReturnResult")
	require.Equal(t, uint64(0), peer.MtForwardSMErrors(), "no peer-side errors")
}

// TestE2E_Client_SRIThenMT drives the gateway through the published
// Apache-2.0 client instead of hand-built JSON: SRI-for-SM, then MT-Forward-SM
// to the network node number the HLR returned. Proves the client module and
// the server agree on the wire contract end to end.
func TestE2E_Client_SRIThenMT(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, _, _ string, _ uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			require.Equal(t, e2eTestMSISDN, string(req.MSISDN), "MSISDN from client")
			require.Equal(t, e2eTestSCA, req.ServiceCentreAddress.Digits, "SCA from client")
			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC},
				},
			}, nil
		},
		OnMtForwardSM: func(req *mapprotocol.MtForwardSMRequest, calledGT, _ string, _ uint32) (*mapprotocol.MtForwardSMResponse, error) {
			require.Equal(t, e2eTestVMSC, calledGT, "CdPA = network node number from the SRI result")
			require.NotNil(t, req.SMRPDA.IMSI)
			require.Equal(t, e2eTestIMSI, string(*req.SMRPDA.IMSI), "IMSI from the SRI result")
			require.Equal(t, e2eFakeTPDU, req.SMRPUI, "TPDU pass-through")
			return &mapprotocol.MtForwardSMResponse{}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-ss7client"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer reqCancel()

	sri, err := client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	require.NoError(t, err, "SRI via client")
	require.Equal(t, e2eTestIMSI, sri.IMSI)
	require.Equal(t, e2eTestVMSC, sri.NetworkNodeNumber)

	mt, err := client.MTForwardSM(reqCtx, ss7client.MTRequest{
		IMSI:                 sri.IMSI,
		NetworkNodeNumber:    ss7client.E164(sri.NetworkNodeNumber),
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
		TPDU:                 e2eFakeTPDU,
	})
	require.NoError(t, err, "MT via client")
	require.Equal(t, ss7client.StatusDelivered, mt.Status)
}

// TestE2E_Client_RemoteErrors: the HLR answers SRI-for-SM with
// unknownSubscriber and the MSC answers MT-ForwardSM with absentSubscriberSM.
// Both must reach the caller as REMOTE_ERROR with the MAP error code — well
// before the TCAP dialogue timer (10s here), i.e. not as a timeout.
func TestE2E_Client_RemoteErrors(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return nil, ss7peer.MAPError{Code: mapprotocol.ErrUnknownSubscriber}
		},
		OnMtForwardSM: func(*mapprotocol.MtForwardSMRequest, string, string, uint32) (*mapprotocol.MtForwardSMResponse, error) {
			return nil, ss7peer.MAPError{Code: mapprotocol.ErrAbsentSubscriberSM}
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-ss7client-errors"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	// Well under tcap.dialogue_timeout (10s): a timeout cannot pass this test.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	_, err = client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr, "SRI must fail with a gateway-reported error")
	require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	require.Equal(t, ss7client.MAPErrUnknownSubscriber, gwErr.Code)
	require.Equal(t, "unknownSubscriber", gwErr.Message)

	_, err = client.MTForwardSM(reqCtx, ss7client.MTRequest{
		IMSI:                 e2eTestIMSI,
		NetworkNodeNumber:    ss7client.E164(e2eTestVMSC),
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
		TPDU:                 e2eFakeTPDU,
	})
	require.ErrorAs(t, err, &gwErr, "MT must fail with a gateway-reported error")
	require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	require.Equal(t, ss7client.MAPErrAbsentSubscriberSM, gwErr.Code)
	require.Equal(t, "absentSubscriberSM", gwErr.Message)
}

// TestE2E_ContextRefused: the HLR supports only version 2 and refuses the
// gateway's v3 dialogue with a TC-U-ABORT (no P-Abort cause, an AARE that
// rejects the context and proposes v2). The gateway must parse it and answer
// the caller at once — ABORTED, naming the refused context — instead of
// waiting out the dialogue timer.
func TestE2E_ContextRefused(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		RefuseContexts:   true,
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			t.Error("a refused dialogue must not reach the operation")
			return nil, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-context-refused"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	// Well under tcap.dialogue_timeout (10s): a timeout cannot pass this test.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	_, err = client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusAborted, gwErr.Status)
	require.Contains(t, gwErr.Message, "application context refused")
	require.Contains(t, gwErr.Message, "0.4.0.0.1.0.20.2", "the version the peer proposes")
}

// TestE2E_NotDelivered: what never reaches the peer is reported at once —
// ABORTED with the reason — instead of after tcap.dialogue_timeout.
//   - SRI-for-SM reaches the "network", which returns it in a UDTS
//     (subsystem failure).
//   - MT-ForwardSM, addressed to a GT no sccp.gtt_rules entry covers, has
//     no route and is never sent.
func TestE2E_NotDelivered(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		ReturnUDTS:       true,
		UDTSCause:        sccp.CauseSubsystemFailure,
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			t.Error("a returned message must not reach the operation")
			return nil, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	// Only UK numbers are routable: the Turkish VMSC GT of the MT request is not.
	// SRI goes to the configured HLR point code (routed on SSN), so it reaches
	// the peer, which returns it.
	cfg.Set("sccp.gtt_rules", []map[string]any{{"pattern": "44*", "point_code": int(e2ePeerPC)}})
	cfg.Set("map.sri_routing", "hlr")
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-not-delivered"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	// Well under tcap.dialogue_timeout (10s): a timeout cannot pass this test.
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	_, err = client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusAborted, gwErr.Status)
	require.Contains(t, gwErr.Message, "UDTS cause 3 (subsystem failure)")

	_, err = client.MTForwardSM(reqCtx, ss7client.MTRequest{
		IMSI:                 e2eTestIMSI,
		NetworkNodeNumber:    ss7client.E164(e2eTestVMSC),
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
		TPDU:                 e2eFakeTPDU,
	})
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusAborted, gwErr.Status)
	require.Contains(t, gwErr.Message, "no route to destination")
}

// TestE2E_LongMT_EmptyBegin: a full-length SMS-DELIVER (164 octets) makes
// MT-ForwardSM too long for one SCCP unitdata. The gateway opens the dialogue
// with an empty Begin (dialogue portion only), and sends the component in a
// CONTINUE once the peer accepts (TS 29.002 §C.2.1) — not as XUDT segments,
// which not every transit node carries (§C.1). The strict peer checks the
// second forward message (its TID, no dialogue portion, the components).
func TestE2E_LongMT_EmptyBegin(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	longTPDU := make([]byte, 164)
	longTPDU[0] = 0x04 // SMS-DELIVER
	for i := 1; i < len(longTPDU); i++ {
		longTPDU[i] = byte(i)
	}

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return nil, nil
		},
		OnMtForwardSM: func(req *mapprotocol.MtForwardSMRequest, _, _ string, _ uint32) (*mapprotocol.MtForwardSMResponse, error) {
			require.Equal(t, longTPDU, req.SMRPUI, "the whole TPDU arrives")
			return &mapprotocol.MtForwardSMResponse{}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-long-mt"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	_, err = client.MTForwardSM(reqCtx, ss7client.MTRequest{
		IMSI:                 e2eTestIMSI,
		NetworkNodeNumber:    ss7client.E164(e2eTestVMSC),
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
		TPDU:                 longTPDU,
	})
	require.NoError(t, err, "a long MT-ForwardSM is delivered")
	require.Equal(t, uint64(1), peer.EmptyBegins(), "opened with an empty Begin")
	require.Equal(t, uint64(1), peer.MtForwardSMResponses())
}

// TestE2E_SegmentedAnswer: the HLR sends its answer as three XUDT segments
// (Q.714 §4.1.1.1.2). The gateway reassembles them (§4.1.1.2) and the caller
// gets the result.
func TestE2E_SegmentedAnswer(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		SegmentAnswers:   3,
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC},
				},
			}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-segmented"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	res, err := client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	require.NoError(t, err, "a segmented answer is reassembled")
	require.Equal(t, e2eTestIMSI, res.IMSI)
	require.Equal(t, e2eTestVMSC, res.NetworkNodeNumber)
}

// TestE2E_Version2Fallback: the peer supports version 2 of the
// short-message contexts only. It refuses the gateway's version 3 dialogues
// with an AARE proposing version 2; the gateway starts a new dialogue under
// it (TS 29.002 §5.2.1) — MT delivery as forwardSM — and the caller gets the
// result as if nothing happened.
func TestE2E_Version2Fallback(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnlyVersion2:     true,
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC},
				},
			}, nil
		},
		OnMtForwardSM: func(req *mapprotocol.MtForwardSMRequest, _, _ string, _ uint32) (*mapprotocol.MtForwardSMResponse, error) {
			require.Equal(t, e2eFakeTPDU, req.SMRPUI)
			return &mapprotocol.MtForwardSMResponse{}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	cfg := buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL())
	handlers, err := initializeHandlers(cfg, logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-v2-fallback"))
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	sri, err := client.SendRoutingInfoForSM(reqCtx, ss7client.SRIRequest{
		MSISDN:               e2eTestMSISDN,
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
	})
	require.NoError(t, err, "SRI-for-SM succeeds after falling back to version 2")
	require.Equal(t, e2eTestIMSI, sri.IMSI)

	_, err = client.MTForwardSM(reqCtx, ss7client.MTRequest{
		IMSI:                 e2eTestIMSI,
		NetworkNodeNumber:    ss7client.E164(e2eTestVMSC),
		ServiceCentreAddress: ss7client.E164(e2eTestSCA),
		TPDU:                 e2eFakeTPDU,
	})
	require.NoError(t, err, "MT delivery succeeds as forwardSM in version 2")
	require.Equal(t, uint64(2), peer.V2Dialogues())
}

// startEmbeddedNATS boots an in-process NATS server on a random port.
// JetStream not enabled — the gateway uses NATS pub/sub for the SS7
// bridge subjects, and JetStream KV for ConfigStore. ConfigStore can
// run without JetStream as a no-op fallback for tests; if a future test
// asserts ConfigStore behavior, switch to RunBasicJetStreamServer.
func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // random
	opts.JetStream = true
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS server did not become ready")
	}
	return srv
}

// allocLoopbackSCTPPort grabs a free TCP port and reuses it for SCTP.
// Not strictly correct (TCP/SCTP port spaces are separate at the
// kernel) but the loopback collision rate is negligible in practice
// and avoids depending on SCTP-specific port allocators.
func allocLoopbackSCTPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// buildE2EConfig produces a viper config tailored for the in-process
// E2E run: m3ua points at the peer, NATS uses the embedded server,
// firewall/TDR/HA disabled, API in dev_mode so boot doesn't reject the
// placeholder secrets.
func buildE2EConfig(t *testing.T, peerSCTPAddr, natsURL string) *viper.Viper {
	t.Helper()
	host, portStr, err := net.SplitHostPort(peerSCTPAddr)
	require.NoError(t, err, "split peer addr")
	var peerPort int
	_, err = fmt.Sscanf(portStr, "%d", &peerPort)
	require.NoError(t, err, "parse peer port")

	v := viper.New()

	// Identity
	v.Set("node_id", "e2e-node-1")
	v.Set("node_id_numeric", 1)
	v.Set("cluster_name", "e2e-cluster")

	// M3UA → peer
	v.Set("m3ua.sgw_address", host)
	v.Set("m3ua.sgw_port", peerPort)
	v.Set("m3ua.asp_id", 1)
	v.Set("m3ua.routing_context", 1)
	v.Set("m3ua.network_appearance", 0)
	v.Set("m3ua.network_indicator", int(e2eNetworkIndicator))
	v.Set("m3ua.traffic_mode", 1)
	v.Set("m3ua.multi_homing_enabled", false)
	v.Set("m3ua.local_ips", []string{"127.0.0.1"})
	v.Set("m3ua.heartbeat_interval", "30s")
	v.Set("m3ua.path_max_retrans", 5)
	v.Set("m3ua.rto_initial", "1s")
	v.Set("m3ua.rto_min", "500ms")
	v.Set("m3ua.rto_max", "5s")
	v.Set("m3ua.assoc_max_retrans", 5)
	v.Set("m3ua.aspup_ack_timeout", "3s")
	v.Set("m3ua.aspac_ack_timeout", "3s")
	v.Set("m3ua.reconnect_interval", "2s")
	v.Set("m3ua.reconnect_max_attempts", 0)

	// SCCP — local PC matches peer's view of the gateway DPC. ANSI variant
	// to exercise 24-bit PC wire encoding (matches config.example.yaml).
	v.Set("sccp.network_variant", "ansi")
	v.Set("sccp.local_point_code", int(e2eGatewayPC))
	v.Set("sccp.local_subsystems", []int{6, 8, 146, 147}) // HLR, MSC, CAP, gsmSCF (MAP)
	// Cluster-shared own GT for CgPA — exercises GT-based source addressing
	// (HA prerequisite). Peer callbacks assert callingGT == e2eTestOwnGT.
	v.Set("sccp.local_global_title.address", e2eTestOwnGT)
	v.Set("sccp.gtt_enabled", true)
	// GT-routed messages (MT-Forward-SM to the VMSC GT) go to the peer, as
	// they would to an STP that translates again.
	v.Set("sccp.gtt_rules", []map[string]any{{"pattern": "*", "point_code": int(e2ePeerPC)}})
	v.Set("sccp.max_hop_counter", 15)
	v.Set("sccp.channel_buffer_size", 1000)
	v.Set("sccp.worker_pool_size", 4)

	// TCAP
	v.Set("tcap.node_id", 1)
	v.Set("tcap.dialogue_timeout", "10s")
	v.Set("tcap.invoke_timeout", "5s")
	v.Set("tcap.idle_timeout", "30s")
	v.Set("tcap.max_dialogues", 1000)
	v.Set("tcap.max_components_per_message", 100)
	v.Set("tcap.max_invokes_per_dialogue", 10)
	v.Set("tcap.channel_buffer_size", 1000)
	v.Set("tcap.worker_pool_size", 4)
	v.Set("tcap.timer_resolution", "1s")

	// MAP — HLR target = peer
	v.Set("map.hlr_address", host)
	v.Set("map.hlr_gt", "") // no GT — route on PC+SSN
	v.Set("map.hlr_pc", int(e2ePeerPC))
	v.Set("map.channel_buffer_size", 1000)
	v.Set("map.worker_pool_size", 4)
	v.Set("map.operation_timeout", "5s")

	// CAP — required by validators even if unused
	v.Set("cap.gsmscf_address", host)
	v.Set("cap.gsmscf_gt", "")
	v.Set("cap.gsmscf_pc", int(e2ePeerPC))
	v.Set("cap.service_keys", map[string]string{"1": "test"})
	v.Set("cap.channel_buffer_size", 1000)
	v.Set("cap.worker_pool_size", 4)
	v.Set("cap.operation_timeout", "5s")

	// NATS → embedded server
	v.Set("nats.url", natsURL)
	v.Set("nats.max_reconnects", 5)
	v.Set("nats.reconnect_wait", "500ms")
	v.Set("nats.ping_interval", "10s")
	v.Set("nats.worker_pool_size", 4)
	v.Set("nats.channel_buffer_size", 1000)
	v.Set("nats.message_timeout", "5s")

	// Firewall — disabled (E2E exercises the protocol stack, not policy)
	v.Set("firewall.enabled", false)

	// TDR — disabled (no S3 in CI)
	v.Set("tdr.enabled", false)

	// API — dev mode so secrets/CORS validators don't hard-fail boot
	v.Set("api.bind_addr", "127.0.0.1:0")
	v.Set("api.dev_mode", true)
	v.Set("api.tls_enabled", false)
	v.Set("api.jwt_secret", "e2e-test-jwt-secret-32-chars-min-required-pad")
	v.Set("api.jwt_expiration", "1h")
	v.Set("api.cors_enabled", false)
	v.Set("api.rate_limit_enabled", false)
	v.Set("api.read_timeout", "5s")
	v.Set("api.write_timeout", "5s")
	v.Set("api.idle_timeout", "10s")
	v.Set("api.default_admin_user", "admin")
	v.Set("api.default_admin_password", "e2e-test-admin-password")

	return v
}

// paramString safely extracts a string from NatsMessage.Parameters.
// Bridge publishes vmsc into Parameters; correlation lives at top level.
func paramString(p map[string]interface{}, key string) string {
	if v, ok := p[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// quietUnused keeps imports referenced when the test file is compiled
// but a subset of branches becomes dead during refactors.
var _ = strings.HasPrefix
