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

	"github.com/ishidawataru/sctp"
	"github.com/stretchr/testify/require"
	gom3ua "github.com/wmnsk/go-m3ua"
	"go.uber.org/zap/zaptest"
)

// sgw is a minimal SG side over real SCTP: it completes the ASP handshake
// and hands each association to the test.
type sgw struct {
	listener *gom3ua.Listener
	conns    chan *gom3ua.Conn
	cancel   context.CancelFunc
}

func startSGW(t *testing.T, port int) *sgw {
	t.Helper()
	cfg := gom3ua.NewServerConfig(&gom3ua.HeartbeatInfo{Enabled: false},
		200, 100, 1, 2, 0, 0, []uint32{1}, 3, 2, 0, 0)
	cfg.CorrelationID = nil
	l, err := gom3ua.Listen("m3ua", &sctp.SCTPAddr{
		IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, Port: port,
	}, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	s := &sgw{listener: l, conns: make(chan *gom3ua.Conn, 4), cancel: cancel}
	go func() {
		for {
			c, err := l.Accept(ctx)
			if err != nil {
				return
			}
			s.conns <- c
		}
	}()
	t.Cleanup(s.stop)
	return s
}

func (s *sgw) stop() {
	s.cancel()
	_ = s.listener.Close()
}

func (s *sgw) accept(t *testing.T) *gom3ua.Conn {
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
