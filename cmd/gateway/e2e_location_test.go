// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

// startLocationE2E boots the strict peer with the given location callbacks,
// one gateway through the production boot path, and a client.
func startLocationE2E(t *testing.T, onATI ss7peer.ATICallback, onPSI ss7peer.PSICallback) (*ss7peer.Peer, *ss7client.Client) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	natsServer := startEmbeddedNATS(t)
	t.Cleanup(natsServer.Shutdown)

	peer, err := ss7peer.Start(context.Background(), ss7peer.Config{
		SCTPAddr:         allocLoopbackSCTPPort(t),
		PointCode:        e2ePeerPC,
		GatewayPointCode: e2eGatewayPC,
		NetworkIndicator: e2eNetworkIndicator,
		NetworkVariant:   sccp.VariantANSI,
		Logger:           logger.Named("peer"),
		OnSRI: func(*mapprotocol.SendRoutingInfoForSMRequest, string, string, uint32) (*mapprotocol.SendRoutingInfoForSMResponse, error) {
			t.Error("no SRI-for-SM expected")
			return nil, nil
		},
		OnATI: onATI,
		OnPSI: onPSI,
	})
	require.NoError(t, err, "start ss7peer")
	t.Cleanup(peer.Stop)
	t.Cleanup(func() { require.Empty(t, peer.Violations(), "ss7peer conformance violations") })

	handlers, err := initializeHandlers(buildE2EConfig(t, peer.SCTPAddr(), natsServer.ClientURL()), logger.Named("gw"))
	require.NoError(t, err, "initializeHandlers")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, startServices(ctx, handlers, logger.Named("gw")), "startServices")
	require.NoError(t, handlers.M3UA.WaitForActive(10*time.Second), "M3UA association")
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = stopServices(shutdownCtx, handlers, logger.Named("gw"))
	})

	nc, err := nats.Connect(natsServer.ClientURL(), nats.Name("e2e-location"))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	client, err := ss7client.New(nc)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return peer, client
}

// Well under tcap.dialogue_timeout (10s): a timeout cannot pass these tests.
func locationCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestE2E_ATI_ByMSISDN: AnyTimeInterrogation goes to the subscriber's HLR as
// the gsmSCF — called GT the MSISDN with SSN 6, calling SSN 147, under
// anyTimeInfoEnquiryContext-v3 (the peer checks all three) — and the HLR's
// location and state come back on map.ati.response.
func TestE2E_ATI_ByMSISDN(t *testing.T) {
	age := 2
	peer, client := startLocationE2E(t,
		func(req *mapprotocol.AnyTimeInterrogationRequest, calledGT, callingGT string, _ uint32) (*mapprotocol.AnyTimeInterrogationResponse, error) {
			require.Equal(t, e2eTestMSISDN, calledGT, "ATI is addressed to the MSISDN")
			require.Equal(t, e2eTestOwnGT, callingGT)
			require.NotNil(t, req.SubscriberIdentity.MSISDN)
			require.Equal(t, e2eTestMSISDN, string(*req.SubscriberIdentity.MSISDN))
			// No map.gsmscf_address and no cluster GT: the node's own GT.
			require.Equal(t, e2eTestOwnGT, req.GSMSCFAddress.Digits)
			require.Equal(t, mapprotocol.NatureInternational, req.GSMSCFAddress.Nature)
			require.True(t, req.RequestedInfo.LocationInformation)
			require.True(t, req.RequestedInfo.SubscriberState)
			require.Nil(t, req.RequestedInfo.RequestedDomain)
			return &mapprotocol.AnyTimeInterrogationResponse{SubscriberInfo: mapprotocol.SubscriberInfo{
				LocationInformation: &mapprotocol.LocationInformation{
					AgeOfLocationInformation: &age,
					VLRNumber:                &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: e2eTestVMSC},
					CellGlobalID:             &mapprotocol.CellGlobalID{MCC: "286", MNC: "01", LAC: 0x1234, CI: 0x5678},
				},
				SubscriberState: &mapprotocol.SubscriberState{State: "assumedIdle"},
			}}, nil
		}, nil)

	res, err := client.AnyTimeInterrogation(locationCtx(t), ss7client.ATIRequest{
		MSISDN:    e2eTestMSISDN,
		Requested: ss7client.RequestedInfo{Location: true, State: true},
	})
	require.NoError(t, err)
	loc := res.Info.Location
	require.NotNil(t, loc)
	require.Equal(t, e2eTestVMSC, loc.VLRNumber)
	require.Equal(t, 2, *loc.AgeMinutes)
	require.Equal(t, &ss7client.CellGlobalID{MCC: "286", MNC: "01", LAC: 0x1234, CI: 0x5678}, loc.CellGlobalID)
	require.Equal(t, "assumedIdle", res.Info.State.State)
	require.Equal(t, "e2e-node-1", res.NodeID)
	require.EqualValues(t, 1, peer.ATIRequests())
}

// TestE2E_PSI: ProvideSubscriberInfo goes to the serving VLR — called GT the
// VLR number with SSN 7, calling SSN 6 (the HLR's role), under
// subscriberInfoEnquiryContext-v3 — paging included.
func TestE2E_PSI(t *testing.T) {
	peer, client := startLocationE2E(t, nil,
		func(req *mapprotocol.ProvideSubscriberInfoRequest, calledGT, _ string, _ uint32) (*mapprotocol.ProvideSubscriberInfoResponse, error) {
			require.Equal(t, e2eTestVMSC, calledGT, "PSI is addressed to the VLR number")
			require.Equal(t, e2eTestIMSI, string(req.IMSI))
			require.True(t, req.RequestedInfo.CurrentLocation)
			require.True(t, req.RequestedInfo.IMEI)
			return &mapprotocol.ProvideSubscriberInfoResponse{SubscriberInfo: mapprotocol.SubscriberInfo{
				LocationInformation: &mapprotocol.LocationInformation{
					ServiceAreaID:            &mapprotocol.ServiceAreaID{MCC: "286", MNC: "01", LAC: 7, SAC: 9},
					CurrentLocationRetrieved: true,
				},
				SubscriberState: &mapprotocol.SubscriberState{State: "netDetNotReachable", NotReachableReason: "imsiDetached"},
				IMEI:            "490154203237518",
			}}, nil
		})

	res, err := client.ProvideSubscriberInfo(locationCtx(t), ss7client.PSIRequest{
		IMSI:      e2eTestIMSI,
		VLRNumber: ss7client.E164(e2eTestVMSC),
		Requested: ss7client.RequestedInfo{Location: true, CurrentLocation: true, State: true, IMEI: true},
	})
	require.NoError(t, err)
	require.Equal(t, &ss7client.ServiceAreaID{MCC: "286", MNC: "01", LAC: 7, SAC: 9}, res.Info.Location.ServiceAreaID)
	require.True(t, res.Info.Location.CurrentLocationRetrieved)
	require.Equal(t, &ss7client.SubscriberState{State: "netDetNotReachable", NotReachableReason: "imsiDetached"}, res.Info.State)
	require.Equal(t, "490154203237518", res.Info.IMEI)
	require.EqualValues(t, 1, peer.PSIRequests())
}

// TestE2E_LocationErrors: the HLR's and VLR's errors reach the caller with
// their MAP codes, and a request the gateway cannot address is answered at
// once with REJECTED instead of silence.
func TestE2E_LocationErrors(t *testing.T) {
	peer, client := startLocationE2E(t,
		func(*mapprotocol.AnyTimeInterrogationRequest, string, string, uint32) (*mapprotocol.AnyTimeInterrogationResponse, error) {
			return nil, ss7peer.MAPError{Code: mapprotocol.ErrATINotAllowed}
		},
		func(*mapprotocol.ProvideSubscriberInfoRequest, string, string, uint32) (*mapprotocol.ProvideSubscriberInfoResponse, error) {
			return nil, ss7peer.MAPError{Code: mapprotocol.ErrUnexpectedDataValue}
		})
	ctx := locationCtx(t)
	var gwErr *ss7client.GatewayError

	_, err := client.AnyTimeInterrogation(ctx, ss7client.ATIRequest{
		MSISDN: e2eTestMSISDN, Requested: ss7client.RequestedInfo{State: true},
	})
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	require.Equal(t, ss7client.MAPErrATINotAllowed, gwErr.Code)
	require.Equal(t, "ati-NotAllowed", gwErr.Message)

	_, err = client.ProvideSubscriberInfo(ctx, ss7client.PSIRequest{
		IMSI: e2eTestIMSI, VLRNumber: ss7client.E164(e2eTestVMSC), Requested: ss7client.RequestedInfo{Location: true},
	})
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	require.Equal(t, ss7client.MAPErrUnexpectedDataValue, gwErr.Code)

	// By IMSI with map.sri_routing "msisdn" and no HLR named: nothing to
	// route on. The client cannot know; the gateway answers.
	start := time.Now()
	_, err = client.AnyTimeInterrogation(ctx, ss7client.ATIRequest{
		IMSI: e2eTestIMSI, Requested: ss7client.RequestedInfo{State: true},
	})
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusRejected, gwErr.Status)
	require.Equal(t, ss7client.ErrCodeRejected, gwErr.Code)
	require.Contains(t, gwErr.Message, "by IMSI")
	require.Less(t, time.Since(start), 2*time.Second, "a rejection is immediate")
	require.EqualValues(t, 1, peer.ATIRequests(), "the rejected request never left the gateway")
}
