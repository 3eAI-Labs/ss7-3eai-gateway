// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

// Errors with what qualifies them, end to end: the application names the
// cause, the VLR reads it from the error's parameter. roamingNotAllowed's
// parameter is mandatory (TS 29.002 MAP-Errors), so it goes with a cause
// even when the application names none; the strict peer reports one
// without it as a violation.

func TestE2E_ErrorCauses_LocationUpdate(t *testing.T) {
	answers := map[string]ss7client.LocationUpdateAnswer{
		"286010000000991": {Reject: ss7client.MAPErrRoamingNotAllowed},
		"286010000000992": {Reject: ss7client.MAPErrRoamingNotAllowed, RejectCause: ss7client.CauseOperatorDeterminedBarring},
		"286010000000993": {Reject: ss7client.MAPErrRoamingNotAllowed, RejectCause: ss7client.CauseSupportedRATTypesNotAllowed},
		"286010000000994": {Reject: ss7client.MAPErrUnknownSubscriber, RejectCause: ss7client.CauseIMSIUnknown},
		"286010000000995": {Reject: ss7client.MAPErrUnknownSubscriber, RejectCause: ss7client.CauseOperatorDeterminedBarring},
	}
	h := startMobilityE2E(t, func(*ss7peer.Config) {}, func(turn *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
		if turn.Action == ss7client.ULEnd {
			return ss7client.LocationUpdateAnswer{}
		}
		return answers[turn.IMSI]
	})
	for imsi, want := range map[string]struct {
		code  mapprotocol.ErrorCode
		cause mapprotocol.ErrorCause
	}{
		"286010000000991": {mapprotocol.ErrRoamingNotAllowed, mapprotocol.CausePLMNRoamingNotAllowed},
		"286010000000992": {mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseOperatorDeterminedBarring},
		"286010000000993": {mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseSupportedRATTypesNotAllowed},
		"286010000000994": {mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseIMSIUnknown},
		// A cause that does not qualify the error: the client sends systemFailure.
		"286010000000995": {mapprotocol.ErrSystemFailure, mapprotocol.CauseNone},
	} {
		arg := e2eUpdateLocationArg()
		arg.IMSI = mapprotocol.IMSI(imsi)
		_, err := h.peer.StartLocationUpdate(imsi, arg)
		require.NoError(t, err)
		next(t, h.turns)
		end := next(t, h.ends)
		assert.Equal(t, want.code, end.Error, imsi)
		assert.Equal(t, want.cause, end.Cause, imsi)
	}
}

func TestE2E_ErrorCauses_AuthenticationAndPurge(t *testing.T) {
	h := startAuthenticationE2E(t, func(r *ss7client.AuthenticationRequest) ss7client.AuthenticationAnswer {
		if r.Request.RequestingNodeType == ss7client.NodeSGSN {
			// TS 29.002 figure 25.5/6: an SGSN asking for a subscriber
			// without GPRS.
			return ss7client.AuthenticationAnswer{Reject: ss7client.MAPErrUnknownSubscriber, RejectCause: ss7client.CauseGPRSEPSSubscriptionUnknown}
		}
		return ss7client.AuthenticationAnswer{Reject: ss7client.MAPErrUnknownSubscriber, RejectCause: ss7client.CausePLMNRoamingNotAllowed}
	}, func(*ss7client.PurgeRequest) ss7client.PurgeAnswer {
		return ss7client.PurgeAnswer{Reject: ss7client.MAPErrUnknownSubscriber, RejectCause: ss7client.CauseIMSIUnknown}
	})

	sgsn := mapprotocol.NodeSGSN
	arg := e2eSAIArg(3)
	arg.RequestingNodeType = &sgsn
	out, err := h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, arg, 0)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrUnknownSubscriber, out.Error)
	assert.Equal(t, mapprotocol.CauseGPRSEPSSubscriptionUnknown, out.ErrorCause)

	out, err = h.peer.SendAuthenticationInfo(requestCtx(t), e2eVLRNumber, e2eSAIArg(3), 0)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrSystemFailure, out.Error, "a roaming cause does not qualify unknownSubscriber")
	assert.Equal(t, mapprotocol.CauseNone, out.ErrorCause)

	vlr := mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: e2eVLRNumber}
	out, err = h.peer.PurgeMS(requestCtx(t), e2eVLRNumber, &mapprotocol.PurgeMSArg{IMSI: e2eTestIMSI, VLRNumber: &vlr})
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.ErrUnknownSubscriber, out.Error)
	assert.Equal(t, mapprotocol.CauseIMSIUnknown, out.ErrorCause)
}
