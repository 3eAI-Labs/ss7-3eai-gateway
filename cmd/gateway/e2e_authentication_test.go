// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

// Authentication and MS purging end to end: the strict peer is the VLR,
// the gateway the HLR's MAP side, the client the AuC and the HLR's
// subscriber records. The VLR addresses the HLR by the E.214 GT of the
// IMSI, as for location updates.

func e2eQuintuplet(i int) ss7client.AuthenticationQuintuplet {
	o := func(n int) string { return strings.Repeat(fmt.Sprintf("%02x", i), n) }
	return ss7client.AuthenticationQuintuplet{RAND: o(16), XRES: o(16), CK: o(16), IK: o(16), AUTN: o(16)}
}

func e2eTriplet(i int) ss7client.AuthenticationTriplet {
	o := func(n int) string { return strings.Repeat(fmt.Sprintf("%02x", i), n) }
	return ss7client.AuthenticationTriplet{RAND: o(16), SRES: o(4), Kc: o(8)}
}

type authHarness struct {
	peer     *ss7peer.Peer
	requests chan *ss7client.AuthenticationRequest
	purges   chan *ss7client.PurgeRequest
}

// startAuthenticationE2E boots the peer as VLR and a gateway, with the
// client serving authentication and purging through the given answers
// (nil: not served).
func startAuthenticationE2E(t *testing.T, auth func(*ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer,
	purge func(*ss7client.PurgeRequest) ss7client.PurgeAnswer) *authHarness {
	t.Helper()
	h := &authHarness{requests: make(chan *ss7client.AuthenticationRequest, 16), purges: make(chan *ss7client.PurgeRequest, 16)}
	var client *ss7client.Client
	h.peer, client = startInterrogationE2E(t, func(*ss7peer.Config) {}, mobilityGateway)
	if auth != nil {
		sub, err := client.ServeAuthentication("auc", func(_ context.Context, r *ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
			h.requests <- r
			return auth(r)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	if purge != nil {
		sub, err := client.ServePurges("hlr", func(_ context.Context, r *ss7client.PurgeRequest) ss7client.PurgeAnswer {
			h.purges <- r
			return purge(r)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	return h
}

func e2eSAIArg(vectors int) *mapprotocol.SendAuthenticationInfoArg {
	return &mapprotocol.SendAuthenticationInfoArg{IMSI: e2eTestIMSI, NumberOfRequestedVectors: vectors}
}

func requestCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestE2E_Authentication_InParts: five UMTS vectors do not fit one
// message. The VLR gets them in parts, asking for each next one in the
// dialogue, every vector once and in order, every message from the
// gateway's own GT.
func TestE2E_Authentication_InParts(t *testing.T) {
	h := startAuthenticationE2E(t, func(*ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		return ss7client.AuthenticationAnswer{Quintuplets: []ss7client.AuthenticationQuintuplet{
			e2eQuintuplet(1), e2eQuintuplet(2), e2eQuintuplet(3), e2eQuintuplet(4), e2eQuintuplet(5)}}
	}, nil)
	node := mapprotocol.NodeVLR
	arg := e2eSAIArg(5)
	arg.RequestingNodeType, arg.RequestingPLMNID = &node, []byte{0x82, 0xF6, 0x10}
	arg.ResyncRAND, arg.ResyncAUTS = make([]byte, 16), []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	out, err := h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, arg, 0)
	require.NoError(t, err)

	r := next(t, h.requests)
	assert.Equal(t, e2eTestIMSI, r.IMSI)
	assert.Equal(t, 5, r.Request.RequestedVectors)
	assert.Equal(t, ss7client.NodeVLR, r.Request.RequestingNodeType)
	assert.Equal(t, "82f610", r.Request.RequestingPLMNID)
	assert.Equal(t, "0102030405060708090a0b0c0d0e", r.Request.Resynchronisation.AUTS)
	assert.Equal(t, e2eVLRNumber, r.Request.CallingGT)
	assert.Equal(t, "e2e-node-1", r.NodeID)

	require.False(t, out.Aborted)
	require.Greater(t, len(out.Parts), 1, "five quintuplets do not fit one UDT")
	var got []byte
	for _, p := range out.Parts {
		for _, q := range p.Quintuplets {
			got = append(got, q.RAND[0])
		}
	}
	assert.Equal(t, []byte{1, 2, 3, 4, 5}, got)
	t.Logf("five quintuplets in %d parts", len(out.Parts))
	for _, gt := range out.GatewayAddresses {
		assert.Equal(t, e2eTestOwnGT, gt, "the gateway answers from its own GT, not the E.214 GT it was called by")
	}
}

// Triplets that fit one message: one END. With segmentation prohibited,
// what fits one message.
func TestE2E_Authentication_OneMessage(t *testing.T) {
	h := startAuthenticationE2E(t, func(r *ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		if r.Request.SegmentationProhibited {
			return ss7client.AuthenticationAnswer{Quintuplets: []ss7client.AuthenticationQuintuplet{
				e2eQuintuplet(1), e2eQuintuplet(2), e2eQuintuplet(3), e2eQuintuplet(4), e2eQuintuplet(5)}}
		}
		return ss7client.AuthenticationAnswer{Triplets: []ss7client.AuthenticationTriplet{e2eTriplet(7), e2eTriplet(8)}}
	}, nil)

	out, err := h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(2), 0)
	require.NoError(t, err)
	require.Len(t, out.Parts, 1)
	require.Len(t, out.Parts[0].Triplets, 2)
	assert.Equal(t, byte(8), out.Parts[0].Triplets[1].Kc[0])

	arg := e2eSAIArg(5)
	arg.SegmentationProhibited = true
	out, err = h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, arg, 0)
	require.NoError(t, err)
	require.Len(t, out.Parts, 1, "segmentation prohibited: one END")
	n := len(out.Parts[0].Quintuplets)
	assert.True(t, n >= 1 && n < 5, "what fits one message: %d", n)
}

// The VLR has enough after the first part and ends the dialogue.
func TestE2E_Authentication_VLRStopsAfterFirstPart(t *testing.T) {
	h := startAuthenticationE2E(t, func(*ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		return ss7client.AuthenticationAnswer{Quintuplets: []ss7client.AuthenticationQuintuplet{
			e2eQuintuplet(1), e2eQuintuplet(2), e2eQuintuplet(3), e2eQuintuplet(4), e2eQuintuplet(5)}}
	}, nil)
	out, err := h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(5), 1)
	require.NoError(t, err)
	require.Len(t, out.Parts, 1)
	// The next request is served normally: the gateway forgot the first.
	out, err = h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(5), 0)
	require.NoError(t, err)
	assert.Greater(t, len(out.Parts), 1)
}

// The AuC refuses; no AuC at all.
func TestE2E_Authentication_Refused(t *testing.T) {
	h := startAuthenticationE2E(t, func(*ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		return ss7client.AuthenticationAnswer{Reject: ss7client.MAPErrUnknownSubscriber}
	}, nil)
	out, err := h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(3), 0)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrUnknownSubscriber, out.Error)
	assert.Empty(t, out.Parts)

	h = startAuthenticationE2E(t, nil, nil)
	start := time.Now()
	out, err = h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(3), 0)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrSystemFailure, out.Error)
	assert.Less(t, time.Since(start), 2*time.Second, "NATS says there is no responder: no wait for the timeout")
}

func TestE2E_PurgeMS(t *testing.T) {
	h := startAuthenticationE2E(t, nil, func(r *ss7client.PurgeRequest) ss7client.PurgeAnswer {
		if r.IMSI != e2eTestIMSI {
			return ss7client.PurgeAnswer{Reject: ss7client.MAPErrUnknownSubscriber}
		}
		return ss7client.PurgeAnswer{FreezeTMSI: r.Request.VLRNumber != nil && r.Request.VLRNumber.Digits == e2eVLRNumber}
	})
	vlr := mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: e2eVLRNumber}
	out, err := h.peer.PurgeMS(requestCtx(t), e2eVLRNumber, &mapprotocol.PurgeMSArg{IMSI: e2eTestIMSI, VLRNumber: &vlr})
	require.NoError(t, err)
	r := next(t, h.purges)
	assert.Equal(t, e2eVLRNumber, r.Request.CallingGT)
	require.NotNil(t, out.Purge)
	assert.Equal(t, &mapprotocol.PurgeMSRes{FreezeTMSI: true}, out.Purge)
	for _, gt := range out.GatewayAddresses {
		assert.Equal(t, e2eTestOwnGT, gt)
	}

	out, err = h.peer.PurgeMS(requestCtx(t), e2eVLRNumber, &mapprotocol.PurgeMSArg{IMSI: "286010000000999", VLRNumber: &vlr})
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrUnknownSubscriber, out.Error)

	// No application: purgeMS has no systemFailure, the dialogue is aborted.
	h = startAuthenticationE2E(t, nil, nil)
	out, err = h.peer.PurgeMS(requestCtx(t), e2eVLRNumber, &mapprotocol.PurgeMSArg{IMSI: e2eTestIMSI, VLRNumber: &vlr})
	require.NoError(t, err)
	assert.True(t, out.Aborted)
}

// TestE2E_Cluster_AuthenticationStaysOnItsNode: three nodes share the
// HLR's E.214 range, which the SG load-shares. Vectors in parts are
// several exchanges; the VLR asks for each next part at the calling
// address of the last, which must be the node's own GT, or the SG may hand
// the request to a node that does not hold the dialogue.
func TestE2E_Cluster_AuthenticationStaysOnItsNode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr: allocLoopbackSCTPPort(t), PointCode: e2ePeerPC, GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator, NetworkVariant: sccp.VariantANSI, Logger: logger.Named("peer"),
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return nil, nil
		},
		SharedGTPrefixes: []string{ss7peer.MGTPrefix, clusterE2EClusterGT},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })
	defer peer.Stop()

	nodes := bootClusterNodes(t, []clusterNodeSpec{
		{nodeIDNumeric: 1, nodeID: "cluster-node-1", localGT: clusterE2ENode1GT},
		{nodeIDNumeric: 2, nodeID: "cluster-node-2", localGT: clusterE2ENode2GT},
		{nodeIDNumeric: 3, nodeID: "cluster-node-3", localGT: clusterE2ENode3GT},
	}, peer.SCTPAddr(), natsServer.ClientURL(), "", logger)
	defer nodes.stopAll(t)
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, peer.WaitForConns(waitCtx, 3))

	nc, err := nats.Connect(natsServer.ClientURL())
	require.NoError(t, err)
	defer nc.Close()
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	defer client.Close()
	sub, err := client.ServeAuthentication("auc", func(context.Context, *ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		return ss7client.AuthenticationAnswer{Quintuplets: []ss7client.AuthenticationQuintuplet{
			e2eQuintuplet(1), e2eQuintuplet(2), e2eQuintuplet(3), e2eQuintuplet(4), e2eQuintuplet(5)}}
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	require.NoError(t, nc.Flush())

	const n = 9
	type result struct {
		out ss7peer.HLRRequestOutcome
		err error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		arg := e2eSAIArg(5)
		arg.IMSI = mapprotocol.IMSI(fmt.Sprintf("2860100000002%02d", i))
		go func() {
			out, err := peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, arg, 0)
			results <- result{out, err}
		}()
	}
	nodeGTs := map[string]bool{clusterE2ENode1GT: true, clusterE2ENode2GT: true, clusterE2ENode3GT: true}
	served := map[string]int{}
	for i := 0; i < n; i++ {
		r := <-results
		require.NoError(t, r.err)
		require.False(t, r.out.Aborted, "a request aborted: a part's request reached a node not holding it")
		require.Greater(t, len(r.out.Parts), 1)
		total := 0
		for _, p := range r.out.Parts {
			total += len(p.Quintuplets)
		}
		assert.Equal(t, 5, total)
		node := r.out.GatewayAddresses[0]
		assert.True(t, nodeGTs[node], "answered from %q, not a node GT", node)
		for _, gt := range r.out.GatewayAddresses {
			assert.Equal(t, node, gt, "one node holds the dialogue to its end")
		}
		served[node]++
	}
	t.Logf("authentication requests by node: %v", served)
	assert.GreaterOrEqual(t, len(served), 2, "the SG load-shared the BEGINs over the nodes")
}
