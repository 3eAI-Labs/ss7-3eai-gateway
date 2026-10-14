// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

func e2eISDN(digits string) *mapprotocol.AddressString {
	return &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: digits}
}

// TestE2E_SRI_RoamingNumber: SendRoutingInfo goes to the subscriber's HLR
// as the GMSC — called GT the MSISDN with SSN 6, calling SSN 8, under
// locationInfoRetrievalContext-v3 (the peer checks all three), a basic call
// interrogation carrying the gateway's GT as GMSC address — and the roaming
// number comes back on map.routing.response.
func TestE2E_SRI_RoamingNumber(t *testing.T) {
	peer, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnRouting = func(req *mapprotocol.SendRoutingInfoRequest, calledGT, callingGT string, _ uint32) ([]*mapprotocol.SendRoutingInfoResponse, error) {
			require.Equal(t, e2eTestMSISDN, calledGT, "SRI is addressed to the MSISDN")
			require.Equal(t, e2eTestOwnGT, callingGT)
			require.Equal(t, mapprotocol.MSISDN(e2eTestMSISDN), req.MSISDN)
			require.Equal(t, mapprotocol.InterrogationBasicCall, req.InterrogationType)
			require.Equal(t, e2eTestOwnGT, req.GMSCAddress.Digits, "no cluster GT: the node's GT")
			require.Equal(t, mapprotocol.NatureInternational, req.GMSCAddress.Nature)
			return []*mapprotocol.SendRoutingInfoResponse{{
				IMSI:           mapprotocol.IMSI(e2eTestIMSI),
				RoamingNumber:  e2eISDN("905329990001"),
				VMSCAddress:    e2eISDN(e2eTestVMSC),
				SubscriberInfo: &mapprotocol.SubscriberInfo{SubscriberState: &mapprotocol.SubscriberState{State: "assumedIdle"}},
			}}, nil
		}
	})

	res, err := client.SendRoutingInfo(locationCtx(t), ss7client.RoutingRequest{MSISDN: e2eTestMSISDN})
	require.NoError(t, err)
	require.Equal(t, e2eTestIMSI, res.IMSI)
	require.Equal(t, &ss7client.AddressString{Digits: "905329990001", Nature: 1, NumberingPlan: 1}, res.Routing.RoamingNumber)
	require.Equal(t, e2eTestVMSC, res.Routing.VMSCAddress)
	require.NotNil(t, res.SubscriberInfo)
	require.Equal(t, "assumedIdle", res.SubscriberInfo.State.State)
	require.Equal(t, "e2e-node-1", res.NodeID)
	require.EqualValues(t, 1, peer.RoutingRequests())
}

// TestE2E_SRI_SegmentedResult: an HLR that sends its answer as Return
// Result Not Last segments in CONTINUEs and a Return Result Last in the END
// (TS 29.002 §C.2.3) gets one merged answer through — every time: the
// segments of a transaction are handled in arrival order.
func TestE2E_SRI_SegmentedResult(t *testing.T) {
	const requests = 20
	peer, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnRouting = func(*mapprotocol.SendRoutingInfoRequest, string, string, uint32) ([]*mapprotocol.SendRoutingInfoResponse, error) {
			return []*mapprotocol.SendRoutingInfoResponse{
				{IMSI: mapprotocol.IMSI(e2eTestIMSI), SSList: []byte{0x21}},
				{SSList: []byte{0x29}, BasicService: &mapprotocol.BasicServiceCode{Code: []byte{0x11}}},
				{ForwardingData: &mapprotocol.ForwardingData{ForwardedToNumber: e2eISDN("905551112233")}, MSISDN: e2eISDN(e2eTestMSISDN)},
			}, nil
		}
	})

	for i := 0; i < requests; i++ {
		res, err := client.SendRoutingInfo(locationCtx(t), ss7client.RoutingRequest{MSISDN: e2eTestMSISDN})
		require.NoError(t, err, "request %d", i)
		require.Equal(t, e2eTestIMSI, res.IMSI, "request %d: the first segment", i)
		require.Equal(t, []string{"21", "29"}, res.Routing.SSCodes, "request %d: ss-Lists in order", i)
		require.Equal(t, "905551112233", res.Routing.Forwarding.ForwardedToNumber.Digits, "request %d: the last segment", i)
		require.Equal(t, e2eTestMSISDN, res.Routing.MSISDN)
	}
	require.EqualValues(t, requests, peer.RoutingRequests())
}

// TestE2E_SRI_Errors: the HLR's refusals reach the caller with their MAP
// codes and names.
func TestE2E_SRI_Errors(t *testing.T) {
	codes := []mapprotocol.ErrorCode{mapprotocol.ErrAbsentSubscriber, mapprotocol.ErrBusySubscriber, mapprotocol.ErrCallBarred}
	next := 0
	_, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnRouting = func(*mapprotocol.SendRoutingInfoRequest, string, string, uint32) ([]*mapprotocol.SendRoutingInfoResponse, error) {
			code := codes[next]
			next++
			return nil, ss7peer.MAPError{Code: code}
		}
	})
	for _, code := range codes {
		_, err := client.SendRoutingInfo(locationCtx(t), ss7client.RoutingRequest{MSISDN: e2eTestMSISDN})
		var gwErr *ss7client.GatewayError
		require.ErrorAs(t, err, &gwErr)
		require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
		require.Equal(t, int32(code), gwErr.Code)
		require.Equal(t, code.String(), gwErr.Message)
	}
}
