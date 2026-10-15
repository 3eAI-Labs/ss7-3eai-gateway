// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	"github.com/3eAI-Labs/ss7-3eai-gateway/test/integration/ss7peer"
)

// USSD end to end: the client, the gateway as gsmSCF, and the strict peer
// as the subscriber's HLR (test/integration/ss7peer/ussd.go), which checks
// the context, subsystems, MAP-OpenInfo, transaction and invoke IDs.

func ussdRoutes(v *viper.Viper) {
	v.Set("map.ussd_routes", []map[string]any{{"prefix": "*123", "route": "balance"}})
}

func ussdText(t *testing.T, s string) *mapprotocol.USSDArg {
	t.Helper()
	dcs, str, err := mapprotocol.EncodeUSSDText(s)
	require.NoError(t, err)
	return &mapprotocol.USSDArg{DCS: dcs, String: str}
}

func ussdDecoded(t *testing.T, a *mapprotocol.USSDArg) string {
	t.Helper()
	require.NotNil(t, a)
	s, err := mapprotocol.DecodeUSSDText(a.DCS, a.String)
	require.NoError(t, err)
	return s
}

// events subscribes to a route's session events.
func ussdEvents(t *testing.T, client *ss7client.Client, route string, handle func(*ss7client.USSDEvent)) chan *ss7client.USSDEvent {
	t.Helper()
	ch := make(chan *ss7client.USSDEvent, 16)
	sub, err := client.SubscribeUSSD(route, "e2e", func(e *ss7client.USSDEvent) {
		if handle != nil {
			handle(e)
		}
		ch <- e
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return ch
}

func next[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
		var zero T
		return zero
	}
}

// TestE2E_USSD_NetworkInitiated: the application asks the subscriber a
// question through their HLR, gets the answer, notifies, releases.
func TestE2E_USSD_NetworkInitiated(t *testing.T) {
	ends := make(chan ss7peer.USSDEnd, 4)
	invokes := make(chan *ss7peer.USSDInvoke, 4)
	_, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnUSSD = func(inv *ss7peer.USSDInvoke) ss7peer.USSDReply {
			invokes <- inv
			if inv.Op == mapprotocol.OpUnstructuredSSRequest {
				return ss7peer.USSDReply{Result: ussdText(t, "1")}
			}
			return ss7peer.USSDReply{}
		}
		c.OnUSSDEnd = func(e ss7peer.USSDEnd) { ends <- e }
	})
	events := ussdEvents(t, client, "billing", nil)

	a, err := client.StartUSSD(locationCtx(t), ss7client.USSDStart{MSISDN: e2eTestMSISDN, Route: "billing",
		Message: ss7client.USSDText{Text: "Balance? 1=yes"}})
	require.NoError(t, err)
	require.Equal(t, "1", a.Reply.Text)
	require.Equal(t, ss7client.DCSGSM7, a.Reply.DCS)
	require.Equal(t, "e2e-node-1", a.NodeID)

	inv := next(t, invokes)
	require.Equal(t, mapprotocol.OpUnstructuredSSRequest, inv.Op)
	require.Equal(t, "Balance? 1=yes", ussdDecoded(t, inv.Arg))
	require.NotNil(t, inv.Open)
	require.Equal(t, e2eTestMSISDN, inv.Open.DestinationReference.Digits)
	require.Equal(t, mapprotocol.NumberingPlanISDN, inv.Open.DestinationReference.NumberPlan)
	require.Equal(t, e2eTestOwnGT, inv.Open.OriginationReference.Digits, "the gsmSCF: the node's GT")

	_, err = client.USSDNotify(locationCtx(t), a.SessionID, ss7client.USSDText{Text: "Bakiye: 5 ₺"})
	require.NoError(t, err)
	inv = next(t, invokes)
	require.Equal(t, mapprotocol.OpUnstructuredSSNotify, inv.Op)
	require.Equal(t, ss7client.DCSUCS2, inv.Arg.DCS, "₺ is not in the GSM alphabet")
	require.Equal(t, "Bakiye: 5 ₺", ussdDecoded(t, inv.Arg))
	require.Nil(t, inv.Open)

	require.NoError(t, client.USSDRelease(locationCtx(t), a.SessionID, nil))
	require.Equal(t, ss7peer.USSDEnd{Session: inv.Session}, next(t, ends), "a basic END")
	e := next(t, events)
	require.Equal(t, ss7client.USSDEnd, e.Action)
	require.Equal(t, a.SessionID, e.SessionID)
	require.Equal(t, "", e.Status)
	require.Equal(t, "application", e.Detail)

	// The session is gone.
	_, err = client.USSDNotify(locationCtx(t), a.SessionID, ss7client.USSDText{Text: "x"})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusRejected, gwErr.Status)
}

// TestE2E_USSD_SubscriberInitiated: the subscriber dials *123#; the begin
// reaches the route *123 maps to; the application asks, gets the answer,
// releases with the final text, which the HLR receives as
// processUnstructuredSS-Request's result.
func TestE2E_USSD_SubscriberInitiated(t *testing.T) {
	ends := make(chan ss7peer.USSDEnd, 4)
	peer, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnUSSD = func(inv *ss7peer.USSDInvoke) ss7peer.USSDReply {
			if ussdDecoded(t, inv.Arg) != "1=TL 2=USD" {
				t.Errorf("prompt %q", ussdDecoded(t, inv.Arg))
			}
			return ss7peer.USSDReply{Result: ussdText(t, "2")}
		}
		c.OnUSSDEnd = func(e ss7peer.USSDEnd) { ends <- e }
	}, ussdRoutes)

	serve := make(chan error, 1)
	events := ussdEvents(t, client, "balance", func(e *ss7client.USSDEvent) {
		if e.Action != ss7client.USSDBegin {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a, err := client.USSDRequest(ctx, e.SessionID, ss7client.USSDText{Text: "1=TL 2=USD"})
			if err == nil && a.Reply.Text != "2" {
				err = errUnexpected(a.Reply.Text)
			}
			if err == nil {
				err = client.USSDRelease(ctx, e.SessionID, &ss7client.USSDText{Text: "USD: 12.50"})
			}
			serve <- err
		}()
	})

	session, err := peer.StartUSSD("*123#", e2eTestIMSI, e2eTestMSISDN, e2eTestOwnGT)
	require.NoError(t, err)
	begin := next(t, events)
	require.Equal(t, ss7client.USSDBegin, begin.Action)
	require.Equal(t, "balance", begin.Route)
	require.Equal(t, "*123#", begin.Message.Text)
	require.Equal(t, e2eTestIMSI, begin.IMSI)
	require.Equal(t, e2eTestMSISDN, begin.MSISDN)

	require.NoError(t, next(t, serve))
	end := next(t, ends)
	require.Equal(t, session, end.Session)
	require.False(t, end.Aborted)
	require.Equal(t, "USD: 12.50", ussdDecoded(t, end.Result))
	e := next(t, events)
	require.Equal(t, ss7client.USSDEnd, e.Action)
	require.Equal(t, "application", e.Detail)
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected reply " + string(e) }

// TestE2E_USSD_Refusals: the network refuses a request (ussd-Busy) — the
// command fails with the code and the session ends with it; the
// application refuses a subscriber's session (unknownAlphabet); the
// subscriber cancels (U-ABORT).
func TestE2E_USSD_Refusals(t *testing.T) {
	ends := make(chan ss7peer.USSDEnd, 8)
	var abortNext bool
	peer, client := startInterrogationE2E(t, func(c *ss7peer.Config) {
		c.OnUSSD = func(inv *ss7peer.USSDInvoke) ss7peer.USSDReply {
			if abortNext {
				return ss7peer.USSDReply{Abort: true}
			}
			return ss7peer.USSDReply{Error: mapprotocol.ErrUSSDBusy}
		}
		c.OnUSSDEnd = func(e ss7peer.USSDEnd) { ends <- e }
	}, ussdRoutes)
	defaults := ussdEvents(t, client, ss7client.USSDDefaultRoute, nil)

	// The network's error.
	_, err := client.StartUSSD(locationCtx(t), ss7client.USSDStart{MSISDN: e2eTestMSISDN, Message: ss7client.USSDText{Text: "?"}})
	var gwErr *ss7client.GatewayError
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusRemoteError, gwErr.Status)
	require.Equal(t, ss7client.MAPErrUSSDBusy, gwErr.Code)
	e := next(t, defaults)
	require.Equal(t, ss7client.USSDEnd, e.Action)
	require.Equal(t, ss7client.StatusRemoteError, e.Status)
	require.Equal(t, ss7client.MAPErrUSSDBusy, e.Code)
	require.False(t, next(t, ends).Aborted, "the gateway closes the dialogue with an END")

	// The application's refusal: *55# matches no route.
	session, err := peer.StartUSSD("*55#", e2eTestIMSI, "", e2eTestOwnGT)
	require.NoError(t, err)
	begin := next(t, defaults)
	require.Equal(t, ss7client.USSDDefaultRoute, begin.Route)
	require.Equal(t, "", begin.MSISDN, "the HLR named the subscriber by IMSI only")
	require.NoError(t, client.USSDReject(locationCtx(t), begin.SessionID, ss7client.MAPErrUnknownAlphabet))
	end := next(t, ends)
	require.Equal(t, session, end.Session)
	require.Equal(t, mapprotocol.ErrUnknownAlphabet, end.Error)
	require.Equal(t, "application", next(t, defaults).Detail)

	// The subscriber cancels while the gateway's request is outstanding.
	abortNext = true
	_, err = peer.StartUSSD("*56#", e2eTestIMSI, "", e2eTestOwnGT)
	require.NoError(t, err)
	begin = next(t, defaults)
	_, err = client.USSDRequest(locationCtx(t), begin.SessionID, ss7client.USSDText{Text: "PIN?"})
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, ss7client.StatusAborted, gwErr.Status)
	e = next(t, defaults)
	require.Equal(t, ss7client.StatusAborted, e.Status)
	require.Equal(t, begin.SessionID, e.SessionID)
}
