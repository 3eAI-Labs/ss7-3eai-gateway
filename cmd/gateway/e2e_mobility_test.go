// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

// Location management end to end: the strict peer is the VLR, the gateway
// the HLR's MAP side, the client the HLR's location management. The VLR
// addresses the HLR by the E.214 GT of the IMSI (ss7peer.MGT), which the
// gateway's own translation rule takes as its own.

const (
	e2eVLRNumber = "905321000002"
	e2eMSCNumber = "905321000001"
)

// mobilityGateway: the HLR's E.214 range translates to the gateway itself.
func mobilityGateway(v *viper.Viper) {
	v.Set("sccp.gtt_rules", []map[string]any{
		{"pattern": ss7peer.MGTPrefix + "*", "numbering_plan": 7, "point_code": int(e2eGatewayPC), "ssn": 6, "priority": 10},
		{"pattern": "*", "point_code": int(e2ePeerPC)},
	})
}

func e2eUpdateLocationArg() *mapprotocol.UpdateLocationArg {
	return &mapprotocol.UpdateLocationArg{IMSI: e2eTestIMSI,
		MSCNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: e2eMSCNumber},
		VLRNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: e2eVLRNumber},
		LMSI:      []byte{1, 2, 3, 4},
		VLRCapability: &mapprotocol.VLRCapability{SupportedCAMELPhases: []int{0, 1, 2}, SupportedRATTypes: []int{0, 1},
			LongFTNSupported: true},
		IMEISV: "3534560112345601"}
}

func e2eProfile() *ss7client.SubscriberData {
	msisdn, ftn := ss7client.E164(e2eTestMSISDN), ss7client.E164("905321119999")
	return &ss7client.SubscriberData{
		MSISDN: &msisdn, Category: "0a", SubscriberStatus: "serviceGranted",
		Teleservices: []string{"11", "21", "22"},
		Forwarding: []ss7client.ForwardingInfo{{SSCode: "29", Features: []ss7client.ForwardingFeature{{
			BasicService: &ss7client.BasicService{Kind: "ext-Teleservice", Code: "10"},
			Status:       ss7client.SSStatus{Provisioned: true, Registered: true, Active: true}, ForwardedToNumber: &ftn,
			Options: &ss7client.ForwardingOptions{NotifyCallingParty: true, Reason: "busy"}}}}},
		SSData: []ss7client.SSData{{SSCode: "12", Status: ss7client.SSStatus{Provisioned: true, Active: true},
			CLIRestriction: "temporaryDefaultAllowed"}},
	}
}

type luHarness struct {
	peer   *ss7peer.Peer
	client *ss7client.Client
	turns  chan *ss7client.LocationUpdateTurn
	ends   chan ss7peer.LocationUpdateEnd
}

// startLocationE2E boots the peer as VLR and a gateway, with the client
// serving location updates through answer.
func startMobilityE2E(t *testing.T, configure func(*ss7peer.Config), answer func(*ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer) *luHarness {
	t.Helper()
	h := &luHarness{turns: make(chan *ss7client.LocationUpdateTurn, 16), ends: make(chan ss7peer.LocationUpdateEnd, 16)}
	h.peer, h.client = startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnLocationUpdateEnd = func(e ss7peer.LocationUpdateEnd) { h.ends <- e }
		if configure != nil {
			configure(c)
		}
	}, mobilityGateway)
	sub, err := h.client.ServeLocationUpdates("hlr", func(_ context.Context, turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		h.turns <- turn
		return answer(turn)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return h
}

// TestE2E_LocationUpdate: the VLR registers the subscriber; the HLR
// application sends the profile; the VLR does not take one teleservice; the
// application accepts. The VLR gets the HLR number, and every message of
// the gateway's comes from its own GT.
func TestE2E_LocationUpdate(t *testing.T) {
	var got []*mapprotocol.InsertSubscriberDataArg
	h := startMobilityE2E(t, func(c *ss7peer.Config) {
		c.OnInsertSubscriberData = func(_ uint32, arg *mapprotocol.InsertSubscriberDataArg) ss7peer.ISDReply {
			got = append(got, arg)
			return ss7peer.ISDReply{Result: &mapprotocol.InsertSubscriberDataRes{Teleservices: [][]byte{{0x22}}}}
		}
	}, func(turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		if turn.Action == ss7client.ULUpdateLocation {
			return ss7client.LocationUpdateAnswer{Insert: e2eProfile()}
		}
		return ss7client.LocationUpdateAnswer{Accept: true}
	})

	dialogue, err := h.peer.StartLocationUpdate(e2eTestIMSI, e2eUpdateLocationArg())
	require.NoError(t, err)

	turn := next(t, h.turns)
	assert.Equal(t, ss7client.ULUpdateLocation, turn.Action)
	assert.Equal(t, e2eTestIMSI, turn.IMSI)
	assert.Equal(t, e2eVLRNumber, turn.Update.VLRNumber.Digits)
	assert.Equal(t, e2eMSCNumber, turn.Update.MSCNumber.Digits)
	assert.Equal(t, e2eVLRNumber, turn.Update.CallingGT, "the VLR's SCCP calling GT")
	assert.Equal(t, "01020304", turn.Update.LMSI)
	assert.Equal(t, "3534560112345601", turn.Update.IMEISV)
	assert.Equal(t, []int{1, 2, 3}, turn.Update.VLRCapability.SupportedCAMELPhases)
	assert.Equal(t, []string{"utran", "geran"}, turn.Update.VLRCapability.SupportedRATTypes)
	assert.Equal(t, "e2e-node-1", turn.NodeID)

	turn = next(t, h.turns)
	assert.Equal(t, ss7client.ULInsertResult, turn.Action)
	assert.Empty(t, turn.Status)
	assert.Equal(t, []string{"22"}, turn.Insert.UnsupportedTeleservices)

	end := next(t, h.ends)
	assert.Equal(t, dialogue, end.Dialogue)
	assert.False(t, end.Aborted)
	require.NotNil(t, end.HLRNumber)
	assert.Equal(t, e2eTestOwnGT, end.HLRNumber.Digits, "no map.hlr_number or cluster GT: the node GT")
	for _, gt := range end.GatewayAddresses {
		assert.Equal(t, e2eTestOwnGT, gt, "the gateway answers from its own GT, not the E.214 GT it was called by")
	}
	require.Len(t, got, 1)
	d := got[0].Data
	assert.Equal(t, e2eTestMSISDN, d.MSISDN.Digits)
	assert.Equal(t, []byte{0x0A}, d.Category)
	require.Len(t, d.ProvisionedSS, 2)
	f := d.ProvisionedSS[0].Forwarding.Features[0]
	assert.Equal(t, byte(0x07), f.SSStatus)
	assert.Equal(t, "905321119999", f.ForwardedToNumber.Digits)
	assert.Equal(t, []byte{0x24}, f.ForwardingOptions, "notify calling party, busy")
	assert.Equal(t, 2, *d.ProvisionedSS[1].SSData.CLIRestrictionOption)
}

// A profile too long for one message reaches the VLR in parts, the
// application's view unchanged: one insert, one result.
func TestE2E_LocationUpdate_ProfileInParts(t *testing.T) {
	profile := e2eProfile()
	for code := 0x91; code <= 0x9B && len(profile.CallBarring) < 8; code++ {
		profile.CallBarring = append(profile.CallBarring, ss7client.CallBarringInfo{SSCode: fmt.Sprintf("%02x", code),
			Features: []ss7client.CallBarringFeature{
				{BasicService: &ss7client.BasicService{Kind: "ext-Teleservice", Code: "10"}, Status: ss7client.SSStatus{Provisioned: true}},
				{BasicService: &ss7client.BasicService{Kind: "ext-Teleservice", Code: "20"}, Status: ss7client.SSStatus{Provisioned: true}},
				{BasicService: &ss7client.BasicService{Kind: "ext-BearerService", Code: "10"}, Status: ss7client.SSStatus{Provisioned: true}},
			}})
	}
	var parts atomic.Int32
	h := startMobilityE2E(t, func(c *ss7peer.Config) {
		c.OnInsertSubscriberData = func(uint32, *mapprotocol.InsertSubscriberDataArg) ss7peer.ISDReply {
			if parts.Add(1) == 1 {
				return ss7peer.ISDReply{Result: &mapprotocol.InsertSubscriberDataRes{SSCodes: []byte{0x91}}}
			}
			return ss7peer.ISDReply{}
		}
	}, func(turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		if turn.Action == ss7client.ULUpdateLocation {
			return ss7client.LocationUpdateAnswer{Insert: profile}
		}
		return ss7client.LocationUpdateAnswer{Accept: true}
	})
	_, err := h.peer.StartLocationUpdate(e2eTestIMSI, e2eUpdateLocationArg())
	require.NoError(t, err)
	next(t, h.turns)
	turn := next(t, h.turns)
	assert.Equal(t, []string{"91"}, turn.Insert.UnsupportedSSCodes)
	end := next(t, h.ends)
	require.NotNil(t, end.HLRNumber)
	require.Greater(t, len(end.Inserts), 1, "the profile did not need parts: make it longer")
	var barring []byte
	for i, arg := range end.Inserts {
		if i > 0 {
			assert.Nil(t, arg.Data.MSISDN, "the subscriber's own data is in the first part")
		}
		for _, ss := range arg.Data.ProvisionedSS {
			if ss.CallBarring != nil {
				barring = append(barring, ss.CallBarring.SSCode)
			}
		}
	}
	var want []byte
	for _, b := range profile.CallBarring {
		var c byte
		_, _ = fmt.Sscanf(b.SSCode, "%02x", &c)
		want = append(want, c)
	}
	assert.Equal(t, want, barring, "every barring service, once, in order")
}

// The application refuses; the VLR aborts while the gateway waits for it;
// the VLR refuses the data.
func TestE2E_LocationUpdate_Failures(t *testing.T) {
	var abortISD, refuseISD atomic.Bool
	h := startMobilityE2E(t, func(c *ss7peer.Config) {
		c.OnInsertSubscriberData = func(uint32, *mapprotocol.InsertSubscriberDataArg) ss7peer.ISDReply {
			switch {
			case abortISD.Load():
				return ss7peer.ISDReply{Abort: true}
			case refuseISD.Load():
				return ss7peer.ISDReply{Error: mapprotocol.ErrUnexpectedDataValue}
			}
			return ss7peer.ISDReply{}
		}
	}, func(turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		switch {
		case turn.Action == ss7client.ULEnd:
			return ss7client.LocationUpdateAnswer{}
		case turn.IMSI == "286010000000999":
			return ss7client.LocationUpdateAnswer{Reject: ss7client.MAPErrUnknownSubscriber}
		case turn.Action == ss7client.ULUpdateLocation:
			return ss7client.LocationUpdateAnswer{Insert: e2eProfile()}
		}
		return ss7client.LocationUpdateAnswer{Reject: ss7client.MAPErrSystemFailure}
	})

	unknown := e2eUpdateLocationArg()
	unknown.IMSI = "286010000000999"
	_, err := h.peer.StartLocationUpdate(string(unknown.IMSI), unknown)
	require.NoError(t, err)
	next(t, h.turns)
	end := next(t, h.ends)
	assert.Equal(t, mapprotocol.ErrUnknownSubscriber, end.Error)
	assert.Nil(t, end.HLRNumber)

	abortISD.Store(true)
	_, err = h.peer.StartLocationUpdate(e2eTestIMSI, e2eUpdateLocationArg())
	require.NoError(t, err)
	next(t, h.turns)
	turn := next(t, h.turns)
	assert.Equal(t, ss7client.ULEnd, turn.Action)
	assert.Equal(t, ss7client.StatusAborted, turn.Status)
	assert.Contains(t, turn.Detail, "aborted by the VLR")

	abortISD.Store(false)
	refuseISD.Store(true)
	_, err = h.peer.StartLocationUpdate(e2eTestIMSI, e2eUpdateLocationArg())
	require.NoError(t, err)
	next(t, h.turns)
	turn = next(t, h.turns)
	assert.Equal(t, ss7client.ULInsertResult, turn.Action)
	assert.Equal(t, ss7client.StatusRemoteError, turn.Status)
	assert.Equal(t, ss7client.MAPErrUnexpectedDataValue, turn.Code)
	end = next(t, h.ends)
	assert.Equal(t, mapprotocol.ErrSystemFailure, end.Error, "the application's answer to the refusal")
}

// No HLR application: the location update is refused at once.
func TestE2E_LocationUpdate_NoApplication(t *testing.T) {
	ends := make(chan ss7peer.LocationUpdateEnd, 1)
	peer, _ := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnLocationUpdateEnd = func(e ss7peer.LocationUpdateEnd) { ends <- e }
	}, mobilityGateway)
	_, err := peer.StartLocationUpdate(e2eTestIMSI, e2eUpdateLocationArg())
	require.NoError(t, err)
	start := time.Now()
	end := next(t, ends)
	assert.Equal(t, mapprotocol.ErrSystemFailure, end.Error)
	assert.Less(t, time.Since(start), 2*time.Second, "NATS says there is no responder: no wait for the timeout")
}

// The HLR's requests to the VLR: cancelLocation, provideRoamingNumber,
// insertSubscriberData alone.
func TestE2E_VLRRequests(t *testing.T) {
	var cancelled atomic.Pointer[mapprotocol.CancelLocationArg]
	var inserted atomic.Pointer[mapprotocol.InsertSubscriberDataArg]
	_, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnCancelLocation = func(arg *mapprotocol.CancelLocationArg) mapprotocol.ErrorCode {
			cancelled.Store(arg)
			return 0
		}
		c.OnProvideRoamingNumber = func(arg *mapprotocol.ProvideRoamingNumberArg) (*mapprotocol.ProvideRoamingNumberRes, mapprotocol.ErrorCode) {
			if arg.IMSI != e2eTestIMSI || arg.MSCNumber.Digits != e2eMSCNumber {
				return nil, mapprotocol.ErrUnexpectedDataValue
			}
			if arg.MSISDN == nil {
				return nil, mapprotocol.ErrAbsentSubscriber
			}
			vmsc := mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: e2eMSCNumber}
			return &mapprotocol.ProvideRoamingNumberRes{RoamingNumber: mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905300099999"},
				VMSCAddress: &vmsc}, 0
		}
		c.OnInsertSubscriberData = func(_ uint32, arg *mapprotocol.InsertSubscriberDataArg) ss7peer.ISDReply {
			inserted.Store(arg)
			return ss7peer.ISDReply{Result: &mapprotocol.InsertSubscriberDataRes{SSCodes: []byte{0x92}}}
		}
	}, mobilityGateway)

	require.NoError(t, client.CancelLocation(locationCtx(t), ss7client.CancelLocationRequest{IMSI: e2eTestIMSI, VLRNumber: e2eVLRNumber,
		LMSI: "01020304", Type: ss7client.CancellationSubscriptionWithdraw}))
	require.NotNil(t, cancelled.Load())
	assert.Equal(t, mapprotocol.IMSI(e2eTestIMSI), cancelled.Load().IMSI)
	assert.Equal(t, mapprotocol.CancellationSubscriptionWithdraw, *cancelled.Load().CancellationType)

	msc, msisdn := ss7client.E164(e2eMSCNumber), ss7client.E164(e2eTestMSISDN)
	prn, err := client.ProvideRoamingNumber(locationCtx(t), ss7client.PRNRequest{IMSI: e2eTestIMSI, VLRNumber: e2eVLRNumber,
		Enquiry: ss7client.RoamingNumberEnquiry{MSCNumber: &msc, MSISDN: &msisdn, GMSCAddress: &msisdn, ORInterrogation: true}})
	require.NoError(t, err)
	assert.Equal(t, "905300099999", prn.RoamingNumber.Digits)
	assert.Equal(t, e2eMSCNumber, prn.VMSCAddress.Digits)
	assert.Equal(t, "e2e-node-1", prn.NodeID)

	_, err = client.ProvideRoamingNumber(locationCtx(t), ss7client.PRNRequest{IMSI: e2eTestIMSI, VLRNumber: e2eVLRNumber,
		Enquiry: ss7client.RoamingNumberEnquiry{MSCNumber: &msc}})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr)
	assert.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	assert.Equal(t, ss7client.MAPErrAbsentSubscriber, gwErr.Code)

	isd, err := client.InsertSubscriberData(locationCtx(t), ss7client.ISDRequest{IMSI: e2eTestIMSI, VLRNumber: e2eVLRNumber,
		Data: ss7client.SubscriberData{SubscriberStatus: "operatorDeterminedBarring",
			ODB: &ss7client.ODB{General: []string{"internationalOGCallsBarred"}}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"92"}, isd.Result.UnsupportedSSCodes)
	require.NotNil(t, inserted.Load())
	assert.Equal(t, mapprotocol.IMSI(e2eTestIMSI), inserted.Load().IMSI)
	assert.Equal(t, mapprotocol.NamedBits{1}, inserted.Load().Data.ODB.General)
}

// waitAll collects n location update ends.
func waitAll(t *testing.T, ends <-chan ss7peer.LocationUpdateEnd, n int) []ss7peer.LocationUpdateEnd {
	t.Helper()
	var out []ss7peer.LocationUpdateEnd
	deadline := time.After(20 * time.Second)
	for len(out) < n {
		select {
		case e := <-ends:
			out = append(out, e)
		case <-deadline:
			t.Fatalf("%d of %d location updates ended", len(out), n)
		}
	}
	return out
}

// TestE2E_Cluster_LocationUpdatesStayOnTheirNode: three nodes share the
// cluster GT and the HLR's E.214 range, which the SG (the peer) load-shares
// over them. A location update is several exchanges: the VLR's BEGIN, the
// gateway's insertSubscriberData, the VLR's result, the gateway's END. The
// VLR sends its result to the calling address of the gateway's answer; that
// must be the node's own GT, or the SG may hand it to another node, which
// does not hold the dialogue.
func TestE2E_Cluster_LocationUpdatesStayOnTheirNode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	natsServer := startEmbeddedNATS(t)
	defer natsServer.Shutdown()

	ends := make(chan ss7peer.LocationUpdateEnd, 32)
	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr: allocLoopbackSCTPPort(t), PointCode: e2ePeerPC, GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator, NetworkVariant: sccp.VariantANSI, Logger: logger.Named("peer"),
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			return nil, nil
		},
		OnInsertSubscriberData: func(uint32, *mapprotocol.InsertSubscriberDataArg) ss7peer.ISDReply { return ss7peer.ISDReply{} },
		OnLocationUpdateEnd:    func(e ss7peer.LocationUpdateEnd) { ends <- e },
		SharedGTPrefixes:       []string{ss7peer.MGTPrefix, clusterE2EClusterGT},
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
	sub, err := client.ServeLocationUpdates("hlr", func(_ context.Context, turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		if turn.Action == ss7client.ULUpdateLocation {
			return ss7client.LocationUpdateAnswer{Insert: e2eProfile()}
		}
		return ss7client.LocationUpdateAnswer{Accept: true}
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	require.NoError(t, nc.Flush())

	const n = 12
	for i := 0; i < n; i++ {
		arg := e2eUpdateLocationArg()
		arg.IMSI = mapprotocol.IMSI(fmt.Sprintf("2860100000001%02d", i))
		_, err := peer.StartLocationUpdate(string(arg.IMSI), arg)
		require.NoError(t, err)
	}
	nodeGTs := map[string]bool{clusterE2ENode1GT: true, clusterE2ENode2GT: true, clusterE2ENode3GT: true}
	served := map[string]int{}
	for _, e := range waitAll(t, ends, n) {
		require.NotNil(t, e.HLRNumber, "location update %d not accepted (aborted %v, error %v)", e.Dialogue, e.Aborted, e.Error)
		assert.Equal(t, clusterE2EClusterGT, e.HLRNumber.Digits, "the HLR number is the cluster's")
		require.NotEmpty(t, e.GatewayAddresses)
		node := e.GatewayAddresses[0]
		assert.True(t, nodeGTs[node], "answered from %q, not a node GT", node)
		for _, gt := range e.GatewayAddresses {
			assert.Equal(t, node, gt, "one node holds the dialogue to its end")
		}
		served[node]++
	}
	t.Logf("location updates by node: %v", served)
	assert.GreaterOrEqual(t, len(served), 2, "the SG load-shared the BEGINs over the nodes")
}
