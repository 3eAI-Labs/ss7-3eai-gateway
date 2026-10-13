// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package natsbridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// startEmbeddedNATS boots an in-process NATS server on a random port for
// the test. Cleanup registered with t.Cleanup. JetStream off — this
// package only uses core pub/sub.
func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	require.True(t, srv.ReadyForConnections(5*time.Second), "NATS not ready")
	return srv
}

// TestValidateConfig_RequiresClusterName locks the validation that
// prevents the duplicate-processing bug from re-introducing. An empty
// cluster_name would silently degrade to plain Subscribe (no queue
// group) — every cluster node would process every NATS request,
// originating N duplicate SS7 dialogues per request. This must fail
// at construction time.
func TestValidateConfig_RequiresClusterName(t *testing.T) {
	cfg := &Config{
		ServerURL: "nats://127.0.0.1:4222",
		NodeID:    "node-1",
		// ClusterName intentionally empty.
	}
	_, err := NewHandler(cfg, zaptest.NewLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster_name is required",
		"empty cluster_name must fail-fast at construction")
}

// TestQueueSubscribe_LoadBalancesAcrossClusterNodes is the regression
// test for the queue group hot-fix. Two handlers with the same
// cluster_name (= same queue group) connect to one NATS server. A
// single MT SMS request publish must be processed by EXACTLY ONE of
// them — not both. Without the QueueSubscribe fix, both handlers would
// receive the message and originate two SS7 dialogues for one request,
// duplicating SMS delivery on a 2-node cluster.
func TestQueueSubscribe_LoadBalancesAcrossClusterNodes(t *testing.T) {
	srv := startEmbeddedNATS(t)
	url := srv.ClientURL()

	const cluster = "regression-cluster"

	mkHandler := func(nodeID string, counter *atomic.Uint64) *Handler {
		t.Helper()
		h, err := NewHandler(&Config{
			ServerURL:         url,
			NodeID:            nodeID,
			ClusterName:       cluster,
			ChannelBufferSize: 16,
			WorkerPoolSize:    1,
			MessageTimeout:    1 * time.Second,
			ConnectionTimeout: 1 * time.Second,
			ReconnectWait:     100 * time.Millisecond,
			PingInterval:      10 * time.Second,
			MaxReconnects:     3,
		}, zaptest.NewLogger(t).Named(nodeID))
		require.NoError(t, err)

		// Register an MT SMS request handler that bumps the counter.
		// processIncomingMessage hands off to onMTSmsRequest when set.
		h.OnMTSmsRequest(func(_ *NatsMessage) error {
			counter.Add(1)
			return nil
		})
		return h
	}

	var n1Count, n2Count atomic.Uint64
	h1 := mkHandler("node-1", &n1Count)
	h2 := mkHandler("node-2", &n2Count)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, h1.Start(ctx))
	require.NoError(t, h2.Start(ctx))
	defer h1.Stop(ctx)
	defer h2.Stop(ctx)

	// Independent publisher connection — represents an external SMSC.
	pub, err := nats.Connect(url, nats.Timeout(1*time.Second))
	require.NoError(t, err)
	defer pub.Close()

	// One request — only one handler should fire.
	const requestPayload = `{"message_id":"rg-1","correlation_id":"c-1","message_type":"MAP_MT_SMS","direction":"INBOUND","timestamp":"2026-05-04T00:00:00Z","msisdn":"905321234567","imsi":"286010000000123"}`
	require.NoError(t, pub.Publish(SubjectMTSmsRequest, []byte(requestPayload)))
	require.NoError(t, pub.Flush())

	// Allow NATS delivery + handler dispatch.
	require.Eventually(t, func() bool {
		return n1Count.Load()+n2Count.Load() >= 1
	}, 2*time.Second, 25*time.Millisecond, "no handler fired")

	// Sleep an extra beat to surface the bug: in the broken code, both
	// handlers receive plain Subscribe deliveries and the second hit
	// arrives within milliseconds. Eventually-with-once-met would miss it.
	time.Sleep(150 * time.Millisecond)

	total := n1Count.Load() + n2Count.Load()
	assert.Equal(t, uint64(1), total,
		"queue group must deliver each message to exactly one cluster node — got %d total deliveries (n1=%d n2=%d)",
		total, n1Count.Load(), n2Count.Load())
}

// TestQueueSubscribe_DistributesAcrossPublishes — over many publishes,
// both nodes should receive some share of traffic (NATS hash-distributes
// across queue group members). Tolerance is loose; we only assert each
// node got at least one message, proving the queue group isn't pinning
// to one subscriber.
func TestQueueSubscribe_DistributesAcrossPublishes(t *testing.T) {
	srv := startEmbeddedNATS(t)
	url := srv.ClientURL()
	const cluster = "distribution-cluster"

	var n1Count, n2Count atomic.Uint64
	var wg sync.WaitGroup

	mkHandler := func(nodeID string, counter *atomic.Uint64) *Handler {
		t.Helper()
		h, err := NewHandler(&Config{
			ServerURL:         url,
			NodeID:            nodeID,
			ClusterName:       cluster,
			ChannelBufferSize: 64,
			WorkerPoolSize:    1,
			MessageTimeout:    1 * time.Second,
			ConnectionTimeout: 1 * time.Second,
			ReconnectWait:     100 * time.Millisecond,
			PingInterval:      10 * time.Second,
			MaxReconnects:     3,
		}, zaptest.NewLogger(t).Named(nodeID))
		require.NoError(t, err)
		h.OnMTSmsRequest(func(_ *NatsMessage) error {
			counter.Add(1)
			wg.Done()
			return nil
		})
		return h
	}

	h1 := mkHandler("node-1", &n1Count)
	h2 := mkHandler("node-2", &n2Count)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h1.Start(ctx))
	require.NoError(t, h2.Start(ctx))
	defer h1.Stop(ctx)
	defer h2.Stop(ctx)

	pub, err := nats.Connect(url, nats.Timeout(1*time.Second))
	require.NoError(t, err)
	defer pub.Close()

	const N = 50
	wg.Add(N)
	for i := 0; i < N; i++ {
		require.NoError(t, pub.Publish(SubjectMTSmsRequest,
			[]byte(`{"message_id":"d","correlation_id":"d","message_type":"MAP_MT_SMS","direction":"INBOUND","timestamp":"2026-05-04T00:00:00Z","msisdn":"1","imsi":"1"}`)))
	}
	require.NoError(t, pub.Flush())

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("not all messages delivered (n1=%d n2=%d, want %d total)",
			n1Count.Load(), n2Count.Load(), N)
	}

	total := n1Count.Load() + n2Count.Load()
	assert.Equal(t, uint64(N), total, "every message delivered exactly once")
	assert.Greater(t, n1Count.Load(), uint64(0), "node-1 must receive some share")
	assert.Greater(t, n2Count.Load(), uint64(0), "node-2 must receive some share")
}
