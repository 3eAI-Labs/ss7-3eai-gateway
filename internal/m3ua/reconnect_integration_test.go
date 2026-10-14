// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package m3ua

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua/wire"
	"github.com/ishidawataru/sctp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// sgw is a minimal SG side over real SCTP: it completes the ASP handshake
// of each association in a goroutine of its own and hands the association
// to the test.
type sgw struct {
	listener *wire.Listener
	conns    chan *wire.Conn
}

func startSGW(t *testing.T, port int) *sgw {
	t.Helper()
	l, err := wire.Listen(&sctp.SCTPAddr{
		IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, Port: port,
	}, wire.Config{HandshakeTimeout: 2 * time.Second})
	require.NoError(t, err)

	s := &sgw{listener: l, conns: make(chan *wire.Conn, 8)}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				if c.Handshake() == nil {
					s.conns <- c
					return
				}
				_ = c.Close()
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return s
}

func (s *sgw) accept(t *testing.T) *wire.Conn {
	t.Helper()
	select {
	case c := <-s.conns:
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("no association reached the SGW")
		return nil
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	_, p, _ := net.SplitHostPort(l.Addr().String())
	require.NoError(t, l.Close())
	port, err := strconv.Atoi(p)
	require.NoError(t, err)
	return port
}

func startASP(t *testing.T, port int) *Handler {
	t.Helper()
	h, err := NewHandler(&Config{
		SGWAddress:        "127.0.0.1",
		SGWPort:           port,
		ASPIdentifier:     1,
		RoutingContext:    1,
		TrafficMode:       2,
		NetworkIndicator:  2,
		LocalIPs:          []string{"127.0.0.1"},
		ASPUPACKTimeout:   time.Second,
		ReconnectInterval: 200 * time.Millisecond,
	}, zaptest.NewLogger(t))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
		cancel()
	})
	return h
}

// The SGW is not there when the gateway boots. Start must not fail, and the
// link must come up once the SGW appears.
func TestM3UA_StartWithoutSGWThenSGWAppears(t *testing.T) {
	port := freeLoopbackPort(t)
	h := startASP(t, port)
	time.Sleep(500 * time.Millisecond)
	require.False(t, h.IsActive(), "no SGW yet")

	s := startSGW(t, port)
	s.accept(t)
	require.NoError(t, h.WaitForActive(10*time.Second))
}

// The SGW drops the association. The gateway must re-establish it, and keep
// doing so (the old code re-dialled once, with a context it then cancelled,
// which closed the fresh association straight away).
func TestM3UA_ReconnectsAfterAssociationLoss(t *testing.T) {
	port := freeLoopbackPort(t)
	s := startSGW(t, port)
	h := startASP(t, port)

	for round := 1; round <= 3; round++ {
		c := s.accept(t)
		require.NoError(t, h.WaitForActive(10*time.Second), "round %d", round)
		_ = c.Close()
	}
	c := s.accept(t)
	require.NoError(t, h.WaitForActive(10*time.Second))

	// The re-established association stays up: no self-inflicted close.
	time.Sleep(time.Second)
	require.True(t, h.IsActive(), "association closed itself after reconnect")
	require.NotNil(t, c)
}

// The messages the SG sends on one SLS reach SCCP in that order, through the
// handler's read loop and channel — not only through the association.
func TestM3UA_InboundOrderReachesSCCP(t *testing.T) {
	port := freeLoopbackPort(t)
	s := startSGW(t, port)
	h := startASP(t, port)
	c := s.accept(t)
	require.NoError(t, h.WaitForActive(10*time.Second))

	const n = 500
	go func() {
		for i := 0; i < n; i++ {
			pd := &wire.ProtocolData{OPC: 200, DPC: 100, SI: 3, NI: 2, SLS: 7, Data: []byte{byte(i >> 8), byte(i)}}
			if _, err := c.WriteData(pd, 1+7%c.MaxStream()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < n; i++ {
		select {
		case m := <-h.IncomingMessages():
			require.Equal(t, i, int(m.PayloadData[0])<<8|int(m.PayloadData[1]), "message %d out of order", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("message %d did not arrive", i)
		}
	}
}
