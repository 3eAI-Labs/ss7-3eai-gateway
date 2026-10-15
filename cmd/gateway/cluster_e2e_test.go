// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// Cluster E2E integration tests — 3-node Active-Active loadshare per
// ADR-006, slice H3. Each test boots THREE concurrent `cmd/gateway`
// instances against ONE in-process HLR peer (acting as the SG-equivalent)
// and ONE shared NATS server. The cluster nodes share `cluster_name`
// (= NATS queue group) and `cluster_global_title.address`, but have
// distinct `node_id_numeric` and `local_global_title.address` per
// ADR-006 §2 Model B.
//
// Two tests:
//
//  1. TestE2E_Cluster_3Node_NATSLoadShare — proves end-to-end that NATS
//     QueueSubscribe distributes inbound requests across cluster nodes
//     AND that responses route back only to the originating node
//     (sticky-by-TID). Attribution uses the TCAP TID upper-byte
//     (== node_id_numeric per `internal/tcap/handler.go::allocateTID`),
//     which the peer surfaces via the SRICallback's `otid` parameter.
//     No log scraping or out-of-band coordination — the protocol
//     itself encodes "who originated this dialogue", and the peer is
//     the wire-truthful oracle.
//
//  2. TestE2E_Cluster_NodeOutage_FailureSinkTimeout — drives the
//     ADR-006 §3 caller-retry semantic: peer silently drops requests
//     whose OTID upper byte == 1 (simulating an SS7 outage on
//     node-1's path), the gateway's TCAP `dialogue_timeout` fires,
//     FailureSink publishes `Status=TIMEOUT, ErrorCode=-1` on the
//     same reply subject. After confirming the failure path, the
//     test stops node-1 entirely and verifies the surviving two
//     nodes continue serving traffic — "zero impact on the other
//     two nodes" per the ADR §H3 acceptance criteria.
//
// Out of scope (per ADR-006 §H3 Out-of-scope):
//   - Peer-originated MO-Forward to cluster_gt — would test the SG-side
//     loadshare across cluster nodes for inbound new dialogues. Cluster
//     GT CdPA matching is already proven by the 18 unit tests in
//     `internal/sccp/cluster_gt_test.go`; an end-to-end peer-origination
//     path requires extending the peer with a TCAP/MAP outbound encoder
//     (significant work) and a MAP MO-Forward inbound handler on the
//     gateway side (separate slice). Deferred without loss of generality.
//   - SIGKILL of the gateway process. We use `stopServices` (graceful)
//     to simulate node death. The TIMEOUT FailureSink fires BEFORE Stop
//     is called — Stop only proves the survivors-keep-serving half of
//     the assertion. True SIGKILL would require os.Exec'ing a child
//     gateway, which adds infrastructure cost without strengthening
//     the assertion (the failure-sink → NATS publish path is identical
//     under ungraceful exit).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

const (
	// Cluster-shared identifiers. cluster_name is the NATS queue group
	// (per ADR-006 §4); all three nodes share it. cluster_global_title
	// is the inbound shared GT (per §2 Model B); peer addresses NEW
	// dialogues to this GT and the SG distributes via M3UA loadshare.
	clusterE2EClusterName = "cluster-e2e"
	clusterE2EClusterGT   = "905900000099" // shared inbound GT

	// Per-node sticky GTs (CgPA on outbound — peer pins, SG routes
	// responses back to the originating node).
	clusterE2ENode1GT = "905900000001"
	clusterE2ENode2GT = "905900000002"
	clusterE2ENode3GT = "905900000003"

	// Short TCAP dialogue timeout for the outage test — keeps wall-time
	// of the silent-drop → TIMEOUT path bounded. 2.5s gives the BEGIN
	// time to traverse SCTP loopback and the timer manager headroom
	// before firing. Real production deployments use 10-30s per Q.774.
	clusterE2EOutageDialogueTimeout = "2500ms"
)

// TestE2E_Cluster_3Node_NATSLoadShare boots three gateways against a
// shared peer + NATS server, publishes a batch of SRI requests, and
// asserts both load distribution and sticky response routing.
//
// The peer's SRI callback is an oracle: it receives every BEGIN on the
// wire and records the TID upper byte (= node_id_numeric of the
// originator). After the test publishes N requests and collects N
// successful responses, two assertions hold:
//
//   - Distribution: each of node_id_numeric ∈ {1,2,3} has > 0 inbound
//     SRI dialogues. With N=24 and roughly-uniform NATS queue-group
//     distribution, the probability of any node getting zero is
//     bounded by (2/3)^24 ≈ 5e-5 — safe for a CI test.
//   - Sticky responses: each NATS response has a corrID matching one
//     of the requests, and the count of responses == N. If a response
//     went to a different node than the originator, the response sink
//     would either silently drop (no NATSCorrelationID match) or
//     double-publish (collision); a count == N + a 1:1 corrID match
//     proves neither happened. The sticky-by-conn write-back in
//     `ss7peer.writeSCCPResponse` ensures the response only ever
//     reaches the originator's M3UA conn.
func TestE2E_Cluster_3Node_NATSLoadShare(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	// Per-node-id OTID counter, populated on every SRI Invoke the peer sees.
	// Sync.Mutex over a map[uint8]int rather than per-key atomics — keeps
	// the assertion read trivial and the contention is negligible at N=24.
	var otidMu sync.Mutex
	otidByNode := make(map[uint8]int)

	peerAddr := allocLoopbackSCTPPort(t)
	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         peerAddr,
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, calledGT, callingGT string, otid uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			nodeID := uint8(otid >> 24)
			otidMu.Lock()
			otidByNode[nodeID]++
			otidMu.Unlock()

			// CgPA must be one of the per-node sticky GTs — proves the
			// gateway sourced the address from its own local_global_title,
			// not the cluster GT. (Cluster GT is inbound-only per §2.)
			require.Contains(t,
				[]string{clusterE2ENode1GT, clusterE2ENode2GT, clusterE2ENode3GT},
				callingGT,
				"CgPA must be a per-node sticky GT, not cluster GT")

			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{
						Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC,
					},
				},
			}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	// Boot 3 gateways concurrently — they share NATS + peer endpoint, but
	// each carries its own node_id, local_global_title, and TCAP TID
	// upper-byte. clusterGT shared so any node can match inbound CdPA on
	// the cluster GT (H1 matcher).
	nodes := bootClusterNodes(t, []clusterNodeSpec{
		{nodeIDNumeric: 1, nodeID: "cluster-node-1", localGT: clusterE2ENode1GT},
		{nodeIDNumeric: 2, nodeID: "cluster-node-2", localGT: clusterE2ENode2GT},
		{nodeIDNumeric: 3, nodeID: "cluster-node-3", localGT: clusterE2ENode3GT},
	}, peer.SCTPAddr(), natsServer.ClientURL(), "" /* default tcap dialogue timeout */, logger)
	defer nodes.stopAll(t)

	// One NATS client subscribes to the shared response subject. The
	// gateway QueueSubscribe pattern means each REQUEST goes to one node;
	// each RESPONSE is a one-shot publish back to the same subject (no
	// queue group on the response side — every subscriber sees every
	// response). Test client = 1 subscriber, so it sees all responses.
	nc, err := nats.Connect(natsServer.ClientURL(),
		nats.Name("e2e-cluster-loadshare-client"),
		nats.Timeout(5*time.Second))
	require.NoError(t, err, "nats client connect")
	defer nc.Close()

	respCh := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(natsbridge.SubjectSRIResponse, respCh)
	require.NoError(t, err, "subscribe SRI response")
	defer sub.Unsubscribe()

	// 24 requests = ~8 per node statistically. Probability of a zero-
	// shard for any single node ≤ (2/3)^24 ≈ 5e-5 — safe for CI.
	const requestCount = 24
	corrIDs := make(map[string]bool, requestCount)
	for i := 0; i < requestCount; i++ {
		corrID := fmt.Sprintf("cluster-lb-%03d", i)
		corrIDs[corrID] = false
		reqBytes, err := json.Marshal(&natsbridge.NatsMessage{
			MessageID:     corrID,
			CorrelationID: corrID,
			MSISDN:        e2eTestMSISDN,
			Sender:        e2eTestSCA,
			Parameters:    map[string]interface{}{"service_centre_address": e2eTestSCA},
		})
		require.NoError(t, err)
		require.NoError(t, nc.Publish(natsbridge.SubjectSRIRequest, reqBytes), "publish SRI request")
	}

	// Collect all 24 responses. Generous deadline (15s) covers cold-
	// handshake on slow CI runners + 24× round-trip on loopback SCTP.
	deadline := time.After(15 * time.Second)
	deliveredCount := 0
	for deliveredCount < requestCount {
		select {
		case msg := <-respCh:
			var resp natsbridge.NatsMessage
			require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode SRI response")

			seen, known := corrIDs[resp.CorrelationID]
			require.True(t, known, "unexpected corrID %q in response", resp.CorrelationID)
			require.False(t, seen, "duplicate response for corrID %q — sticky violation", resp.CorrelationID)
			corrIDs[resp.CorrelationID] = true
			deliveredCount++
		case <-deadline:
			t.Fatalf("timeout: only %d/%d responses received; peer SRI requests=%d responses=%d",
				deliveredCount, requestCount, peer.SRIRequests(), peer.SRIResponses())
		}
	}

	// Distribution assertion: every node served ≥ 1 dialogue.
	otidMu.Lock()
	defer otidMu.Unlock()
	t.Logf("OTID distribution by node_id: %v (total %d)", otidByNode, peer.SRIRequests())
	for _, nodeID := range []uint8{1, 2, 3} {
		require.Greaterf(t, otidByNode[nodeID], 0,
			"node %d served zero SRI dialogues — NATS queue-group distribution failed (probability ≤ 5e-5; investigate cluster_name or QueueSubscribe regression)",
			nodeID)
	}
	require.Equal(t, requestCount, otidByNode[1]+otidByNode[2]+otidByNode[3],
		"OTID totals must equal request count — extra requests indicate duplicate-delivery (queue-group regression)")
	require.Equal(t, uint64(requestCount), peer.SRIRequests(), "peer Invoke count must equal published request count")
	require.Equal(t, uint64(requestCount), peer.SRIResponses(), "peer ReturnResult count must equal published request count")
	require.Equal(t, uint64(0), peer.SRIErrors(), "no peer-side errors")
}

// TestE2E_Cluster_NodeOutage_FailureSinkTimeout drives the TCAP TIMEOUT
// failure path via a peer-side silent drop selective on OTID upper byte,
// then proves the surviving cluster keeps serving after the affected
// node is stopped.
//
// Phase 1 — outage simulation:
//   - Peer drops ALL SRI Invokes whose OTID upper byte == 1 (node-1).
//   - Test publishes 12 SRI requests; NATS distributes; some land on
//     node-1, some on node-2/3.
//   - Node-1's dialogues hit `tcap.dialogue_timeout` (2.5s) and the
//     FailureSink fires `Status=TIMEOUT, ErrorCode=-1` on
//     SubjectSRIResponse. Node-2/3 dialogues complete normally with
//     `Status=DELIVERED`.
//   - Test asserts: every corrID has exactly one response; TIMEOUT
//     count ≥ 1 (node-1 got at least one); DELIVERED count ≥ 1
//     (the others did too); count(TIMEOUT) == count(OTIDs from node-1).
//
// Phase 2 — survivors-keep-serving:
//   - Test calls `stopServices` on node-1.
//   - Test publishes 6 more SRI requests. With node-1 gone, NATS queue
//     group has only nodes 2 and 3 (peer's drop hook still fires on any
//     OTID upper-byte == 1 it might see, but no node-1 means no such
//     OTIDs). All 6 must succeed within the deadline.
//   - Asserts the cluster degrades gracefully — partial outage, full
//     service on remaining capacity, per ADR-006 §H3 (c).
func TestE2E_Cluster_NodeOutage_FailureSinkTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t)

	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	// Peer-side counters by OTID upper byte. droppedByNode tracks how
	// many SRIs the pre-drop hook silently consumed; servedByNode tracks
	// successful ReturnResults. Together they equal SRIRequests.
	var counterMu sync.Mutex
	droppedByNode := make(map[uint8]int)
	servedByNode := make(map[uint8]int)

	peerAddr := allocLoopbackSCTPPort(t)
	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         peerAddr,
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		// Drop selectively: TIDs originated by node-1 → silent drop →
		// gateway TCAP times out → FailureSink fires TIMEOUT.
		OnSRIPreDrop: func(otid uint32) bool {
			nodeID := uint8(otid >> 24)
			if nodeID == 1 {
				counterMu.Lock()
				droppedByNode[nodeID]++
				counterMu.Unlock()
				return true
			}
			return false
		},
		OnSRI: func(req *mapprotocol.SendRoutingInfoForSMRequest, _, _ string, otid uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			nodeID := uint8(otid >> 24)
			counterMu.Lock()
			servedByNode[nodeID]++
			counterMu.Unlock()
			return &mapprotocol.SendRoutingInfoForSMResponse{
				IMSI: mapprotocol.IMSI(e2eTestIMSI),
				LocationInfoWithLMSI: mapprotocol.LocationInfoWithLMSI{
					NetworkNodeNumber: &mapprotocol.AddressString{
						Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC,
					},
				},
			}, nil
		},
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	nodes := bootClusterNodes(t, []clusterNodeSpec{
		{nodeIDNumeric: 1, nodeID: "cluster-node-1", localGT: clusterE2ENode1GT},
		{nodeIDNumeric: 2, nodeID: "cluster-node-2", localGT: clusterE2ENode2GT},
		{nodeIDNumeric: 3, nodeID: "cluster-node-3", localGT: clusterE2ENode3GT},
	}, peer.SCTPAddr(), natsServer.ClientURL(), clusterE2EOutageDialogueTimeout, logger)
	// Defer stop on all nodes still standing — node-1 will be stopped
	// mid-test in Phase 2; the registry tracks per-node Stop state to
	// avoid double-stop panics.
	defer nodes.stopAll(t)

	nc, err := nats.Connect(natsServer.ClientURL(),
		nats.Name("e2e-cluster-outage-client"),
		nats.Timeout(5*time.Second))
	require.NoError(t, err, "nats client connect")
	defer nc.Close()

	respCh := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(natsbridge.SubjectSRIResponse, respCh)
	require.NoError(t, err, "subscribe SRI response")
	defer sub.Unsubscribe()

	// --- Phase 1: outage simulation --------------------------------

	// 12 requests distributed across 3 nodes. P(node-1 gets 0) ≤ (2/3)^12
	// ≈ 0.77% — tail risk acknowledged but acceptable for CI; if the
	// distribution fails to put any request on node-1, the assertion
	// "TIMEOUT count >= 1" will flag it as a distribution issue not
	// a FailureSink issue.
	const phase1Count = 12
	timeoutCorrIDs := make(map[string]bool)
	deliveredCorrIDs := make(map[string]bool)
	for i := 0; i < phase1Count; i++ {
		corrID := fmt.Sprintf("cluster-outage-p1-%03d", i)
		reqBytes, err := json.Marshal(&natsbridge.NatsMessage{
			MessageID:     corrID,
			CorrelationID: corrID,
			MSISDN:        e2eTestMSISDN,
			Sender:        e2eTestSCA,
			Parameters:    map[string]interface{}{"service_centre_address": e2eTestSCA},
		})
		require.NoError(t, err)
		require.NoError(t, nc.Publish(natsbridge.SubjectSRIRequest, reqBytes), "publish SRI phase1")
	}

	// Wall-time budget: TCAP dialogue_timeout (2.5s) + handshake +
	// margin. 10s covers worst-case CI runners.
	phase1Deadline := time.After(10 * time.Second)
	collected := 0
	for collected < phase1Count {
		select {
		case msg := <-respCh:
			var resp natsbridge.NatsMessage
			require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode response")
			switch resp.Status {
			case "DELIVERED", "":
				// Successful SRI response (status often empty on
				// success — see PublishSRIResponse). Treat both as
				// delivered.
				require.False(t, deliveredCorrIDs[resp.CorrelationID], "duplicate DELIVERED for %s", resp.CorrelationID)
				deliveredCorrIDs[resp.CorrelationID] = true
				require.Equal(t, e2eTestIMSI, resp.IMSI, "DELIVERED response must carry IMSI")
			case "TIMEOUT":
				require.False(t, timeoutCorrIDs[resp.CorrelationID], "duplicate TIMEOUT for %s", resp.CorrelationID)
				timeoutCorrIDs[resp.CorrelationID] = true
				require.Equal(t, int32(-1), resp.ErrorCode,
					"TIMEOUT must carry ErrorCode=-1 per FailureSink wire contract")
			default:
				t.Fatalf("unexpected status %q on response %+v", resp.Status, resp)
			}
			collected++
		case <-phase1Deadline:
			t.Fatalf("phase1 timeout: %d/%d collected; delivered=%d timeout=%d; peer requests=%d",
				collected, phase1Count, len(deliveredCorrIDs), len(timeoutCorrIDs), peer.SRIRequests())
		}
	}

	counterMu.Lock()
	t.Logf("Phase 1: dropped=%v served=%v delivered=%d timeout=%d",
		droppedByNode, servedByNode, len(deliveredCorrIDs), len(timeoutCorrIDs))
	require.Greaterf(t, len(timeoutCorrIDs), 0,
		"expected ≥1 TIMEOUT response — node-1 received zero requests (NATS distribution failed) OR FailureSink regressed")
	require.Greaterf(t, len(deliveredCorrIDs), 0,
		"expected ≥1 DELIVERED response — nodes 2/3 received zero requests (NATS distribution failed)")
	require.Equal(t, droppedByNode[1], len(timeoutCorrIDs),
		"every node-1 OTID drop must produce exactly one TIMEOUT publish — FailureSink coverage regression otherwise")
	require.Equal(t, phase1Count, len(timeoutCorrIDs)+len(deliveredCorrIDs),
		"phase1 corrID count must equal request count — sticky-response or duplicate-delivery regression otherwise")
	counterMu.Unlock()

	// --- Phase 2: stop node-1, survivors keep serving --------------

	t.Logf("Phase 2: stopping node-1")
	nodes.stop(t, 0 /* node-1 = index 0 */)

	// Drain any in-flight responses that might leak into respCh between
	// phases (e.g. node-1's TimerManager firing a delayed FailureSink
	// during the Stop window). Empty drain — discard.
	for drained := true; drained; {
		select {
		case <-respCh:
		default:
			drained = false
		}
	}

	const phase2Count = 6
	phase2CorrIDs := make(map[string]bool, phase2Count)
	for i := 0; i < phase2Count; i++ {
		corrID := fmt.Sprintf("cluster-outage-p2-%03d", i)
		phase2CorrIDs[corrID] = false
		reqBytes, err := json.Marshal(&natsbridge.NatsMessage{
			MessageID:     corrID,
			CorrelationID: corrID,
			MSISDN:        e2eTestMSISDN,
			Sender:        e2eTestSCA,
			Parameters:    map[string]interface{}{"service_centre_address": e2eTestSCA},
		})
		require.NoError(t, err)
		require.NoError(t, nc.Publish(natsbridge.SubjectSRIRequest, reqBytes), "publish SRI phase2")
	}

	phase2Deadline := time.After(10 * time.Second)
	phase2Got := 0
	for phase2Got < phase2Count {
		select {
		case msg := <-respCh:
			var resp natsbridge.NatsMessage
			require.NoError(t, json.Unmarshal(msg.Data, &resp), "decode response phase2")
			seen, known := phase2CorrIDs[resp.CorrelationID]
			if !known {
				// Could be a late TIMEOUT from node-1 still draining; skip.
				t.Logf("phase2: ignoring late response for %s (status=%s)", resp.CorrelationID, resp.Status)
				continue
			}
			require.False(t, seen, "duplicate phase2 response for %s", resp.CorrelationID)
			require.NotEqual(t, "TIMEOUT", resp.Status,
				"phase2: node-1 is stopped, no requests should hit it — TIMEOUT means NATS still routing to dead node")
			phase2CorrIDs[resp.CorrelationID] = true
			phase2Got++
		case <-phase2Deadline:
			t.Fatalf("phase2 timeout: %d/%d responses; peer SRI requests=%d", phase2Got, phase2Count, peer.SRIRequests())
		}
	}

	counterMu.Lock()
	defer counterMu.Unlock()
	t.Logf("Phase 2: served counters: %v", servedByNode)
	// Node-1 must not have served anything in phase 2 (it's stopped).
	// We can't directly assert "node-1 received zero new in phase 2"
	// because servedByNode only increments for non-dropped paths; but
	// a phase2 TIMEOUT (which we explicitly fail-fast on above) would
	// be the only way node-1 got a request and answered. The implicit
	// proof is "no TIMEOUT in phase 2" + "all 6 corrIDs delivered".
	for corrID, ok := range phase2CorrIDs {
		require.True(t, ok, "phase2 missing response for corrID %s", corrID)
	}
}

// --- helpers ---------------------------------------------------------

// clusterNodeSpec describes one cluster member's identity. All other
// config (NATS, peer, cluster GT, traffic mode) is shared across nodes
// and supplied by bootClusterNodes.
type clusterNodeSpec struct {
	nodeIDNumeric int
	nodeID        string
	localGT       string
}

// clusterNode bundles a running gateway's handlers with the lifecycle
// state needed for selective Stop in outage tests.
type clusterNode struct {
	spec     clusterNodeSpec
	handlers *Handlers
	cancel   context.CancelFunc
	stopped  atomic.Bool
	logger   *zap.Logger
}

// clusterNodeRegistry tracks all booted nodes for orderly cleanup.
type clusterNodeRegistry struct {
	nodes []*clusterNode
}

// bootClusterNodes spins up N gateway instances against a shared peer +
// NATS server. Each node gets its own viper config (built via
// buildClusterE2EConfig), its own Handlers tree, and its own context.
// On error mid-boot, already-booted nodes are stopped before failing
// the test — leaks would cascade across subsequent tests in the same
// run.
func bootClusterNodes(
	t *testing.T,
	specs []clusterNodeSpec,
	peerSCTPAddr, natsURL, dialogueTimeout string,
	logger *zap.Logger,
) *clusterNodeRegistry {
	t.Helper()
	reg := &clusterNodeRegistry{nodes: make([]*clusterNode, 0, len(specs))}

	for _, spec := range specs {
		nodeLogger := logger.Named(spec.nodeID)
		cfg := buildClusterE2EConfig(t, spec, peerSCTPAddr, natsURL, dialogueTimeout)

		handlers, err := initializeHandlers(cfg, nodeLogger)
		if err != nil {
			reg.stopAll(t)
			t.Fatalf("initializeHandlers for %s: %v", spec.nodeID, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		if err := startServices(ctx, handlers, nodeLogger); err != nil {
			cancel()
			reg.stopAll(t)
			t.Fatalf("startServices for %s: %v", spec.nodeID, err)
		}
		if err := handlers.M3UA.WaitForActive(10 * time.Second); err != nil {
			cancel()
			reg.stopAll(t)
			t.Fatalf("M3UA association for %s: %v", spec.nodeID, err)
		}

		reg.nodes = append(reg.nodes, &clusterNode{
			spec:     spec,
			handlers: handlers,
			cancel:   cancel,
			logger:   nodeLogger,
		})
	}

	// Brief settle: each node's M3UA ASPAC handshake against the peer
	// is async-completed in a background goroutine. NATS QueueSubscribe
	// is registered synchronously in NATS.Start, so requests published
	// after this point are guaranteed deliverable. The 200ms is a
	// margin over observed loopback handshake latencies (~50-100ms).
	time.Sleep(200 * time.Millisecond)

	return reg
}

// stop tears down a single node by index. Idempotent; safe to call from
// both per-test phases and the final defer.stopAll.
func (r *clusterNodeRegistry) stop(t *testing.T, idx int) {
	t.Helper()
	if idx < 0 || idx >= len(r.nodes) {
		t.Fatalf("stop: index %d out of range (have %d nodes)", idx, len(r.nodes))
	}
	n := r.nodes[idx]
	if !n.stopped.CompareAndSwap(false, true) {
		return
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := stopServices(shutdownCtx, n.handlers, n.logger); err != nil {
		t.Logf("stop %s: %v (continuing — partial shutdown is acceptable mid-test)", n.spec.nodeID, err)
	}
	n.cancel()
}

// stopAll stops every node in reverse boot order. Final cleanup hook.
func (r *clusterNodeRegistry) stopAll(t *testing.T) {
	t.Helper()
	for i := len(r.nodes) - 1; i >= 0; i-- {
		r.stop(t, i)
	}
}

// buildClusterE2EConfig produces a viper config for one cluster node.
// Mirrors buildE2EConfig but with cluster-aware settings: per-node
// node_id/node_id_numeric/local_global_title, shared cluster_name +
// cluster_global_title, m3ua.traffic_mode=2 (loadshare per ADR-006 §5),
// optional override of tcap.dialogue_timeout for outage tests.
//
// API binds 127.0.0.1:0 — each node grabs a distinct OS-assigned port
// so multiple instances coexist on the same loopback.
func buildClusterE2EConfig(
	t *testing.T,
	spec clusterNodeSpec,
	peerSCTPAddr, natsURL, dialogueTimeoutOverride string,
) *viper.Viper {
	t.Helper()
	host, portStr, err := net.SplitHostPort(peerSCTPAddr)
	require.NoError(t, err, "split peer addr")
	var peerPort int
	_, err = fmt.Sscanf(portStr, "%d", &peerPort)
	require.NoError(t, err, "parse peer port")

	v := viper.New()

	// Identity — per node
	v.Set("node_id", spec.nodeID)
	v.Set("node_id_numeric", spec.nodeIDNumeric)
	v.Set("cluster_name", clusterE2EClusterName)

	// M3UA — shared peer + traffic_mode=2 (loadshare). ASP id encodes
	// the node identity so the peer-side log can attribute associations.
	v.Set("m3ua.sgw_address", host)
	v.Set("m3ua.sgw_port", peerPort)
	v.Set("m3ua.asp_id", spec.nodeIDNumeric)
	v.Set("m3ua.routing_context", 1)
	v.Set("m3ua.network_appearance", 0)
	v.Set("m3ua.network_indicator", int(e2eNetworkIndicator))
	v.Set("m3ua.traffic_mode", 2) // loadshare per ADR-006 §5
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

	// SCCP — local_pc shared (cluster nodes share gateway PC), per-node
	// local_global_title, shared cluster_global_title.
	v.Set("sccp.network_variant", "ansi")
	v.Set("sccp.local_point_code", int(e2eGatewayPC))
	v.Set("sccp.local_subsystems", []int{6, 8, 146})
	v.Set("sccp.local_global_title.address", spec.localGT)
	v.Set("sccp.cluster_global_title.address", clusterE2EClusterGT)
	v.Set("sccp.gtt_enabled", true)
	// GT-routed messages (MT-Forward-SM to the VMSC GT) go to the peer, as
	// they would to an STP that translates again; the HLR's E.214 range is
	// the cluster's own (location updates).
	v.Set("sccp.gtt_rules", []map[string]any{
		{"pattern": ss7peer.MGTPrefix + "*", "numbering_plan": 7, "point_code": int(e2eGatewayPC), "ssn": 6, "priority": 10},
		{"pattern": "*", "point_code": int(e2ePeerPC)},
	})
	v.Set("sccp.max_hop_counter", 15)
	v.Set("sccp.channel_buffer_size", 1000)
	v.Set("sccp.worker_pool_size", 4)

	// TCAP — node_id falls back from top-level node_id_numeric, but set
	// it explicitly here for clarity. dialogue_timeout configurable so
	// the outage test can compress its wall clock.
	v.Set("tcap.node_id", spec.nodeIDNumeric)
	dt := "10s"
	if dialogueTimeoutOverride != "" {
		dt = dialogueTimeoutOverride
	}
	v.Set("tcap.dialogue_timeout", dt)
	v.Set("tcap.invoke_timeout", "5s")
	v.Set("tcap.idle_timeout", "30s")
	v.Set("tcap.max_dialogues", 1000)
	v.Set("tcap.max_components_per_message", 100)
	v.Set("tcap.max_invokes_per_dialogue", 10)
	v.Set("tcap.channel_buffer_size", 1000)
	v.Set("tcap.worker_pool_size", 4)
	v.Set("tcap.timer_resolution", "500ms") // tighter resolution so 2.5s timeout fires promptly

	// MAP / CAP — peer is HLR + gsmSCF role
	v.Set("map.hlr_address", host)
	v.Set("map.hlr_gt", "")
	v.Set("map.hlr_pc", int(e2ePeerPC))
	v.Set("map.channel_buffer_size", 1000)
	v.Set("map.worker_pool_size", 4)
	v.Set("map.operation_timeout", "5s")

	v.Set("cap.gsmscf_address", host)
	v.Set("cap.gsmscf_gt", "")
	v.Set("cap.gsmscf_pc", int(e2ePeerPC))
	v.Set("cap.service_keys", map[string]string{"1": "test"})
	v.Set("cap.channel_buffer_size", 1000)
	v.Set("cap.worker_pool_size", 4)
	v.Set("cap.operation_timeout", "5s")

	// NATS — shared embedded server URL across nodes
	v.Set("nats.url", natsURL)
	v.Set("nats.max_reconnects", 5)
	v.Set("nats.reconnect_wait", "500ms")
	v.Set("nats.ping_interval", "10s")
	v.Set("nats.worker_pool_size", 4)
	v.Set("nats.channel_buffer_size", 1000)
	v.Set("nats.message_timeout", "5s")

	// Firewall / TDR — disabled
	v.Set("firewall.enabled", false)
	v.Set("tdr.enabled", false)

	// API — random port per node
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

// _ keeps the natsserver alias referenced in case future tests need
// direct server-side hooks (e.g. account simulation, slow-consumer
// tests).
var _ = (*natsserver.Server)(nil)
