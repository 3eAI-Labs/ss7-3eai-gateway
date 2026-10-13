// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package opstrigger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"
)

type recordingReporter struct {
	mu       sync.Mutex
	outcomes []struct {
		name, token string
		failed      bool
	}
}

func (r *recordingReporter) Report(_ context.Context, name, token string, actionErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, struct {
		name, token string
		failed      bool
	}{name, token, actionErr != nil})
}

func (r *recordingReporter) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.outcomes) }

// writeTokens rewrites the opstrigger.yaml file for the given key/values.
func writeTokens(t *testing.T, path string, kv map[string]string) {
	t.Helper()
	body := "opsTrigger:\n"
	for k, v := range kv {
		body += "  " + k + ": " + `"` + v + `"` + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestConsumer(t *testing.T, rep Reporter) (*Consumer, string, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opstrigger.yaml")
	writeTokens(t, path, map[string]string{"reloadConfig": ""})
	calls := 0
	c := NewConsumer(path, 0, rep, zap.NewNop())
	c.Register("reloadConfig", func(_ context.Context, _ string) error {
		calls++
		return nil
	})
	return c, path, &calls
}

func TestNewTokenFiresOnceThenIdempotent(t *testing.T) {
	rep := &recordingReporter{}
	c, path, calls := newTestConsumer(t, rep)
	ctx := context.Background()

	writeTokens(t, path, map[string]string{"reloadConfig": "t1"})
	c.poll(ctx)
	c.poll(ctx) // same token — must not re-run
	if *calls != 1 {
		t.Fatalf("expected 1 action call, got %d", *calls)
	}

	writeTokens(t, path, map[string]string{"reloadConfig": "t2"})
	c.poll(ctx) // new token — runs again
	if *calls != 2 {
		t.Fatalf("expected 2 action calls, got %d", *calls)
	}
	if rep.count() != 2 {
		t.Fatalf("expected 2 outcomes reported, got %d", rep.count())
	}
}

func TestBaselineSuppressesExistingToken(t *testing.T) {
	rep := &recordingReporter{}
	c, path, calls := newTestConsumer(t, rep)
	// A token already present at startup must NOT fire after baseline.
	writeTokens(t, path, map[string]string{"reloadConfig": "preexisting"})
	c.baseline()
	c.poll(context.Background())
	if *calls != 0 {
		t.Fatalf("baseline should suppress pre-existing token, got %d calls", *calls)
	}
}

func TestEmptyTokenNeverFires(t *testing.T) {
	rep := &recordingReporter{}
	c, path, calls := newTestConsumer(t, rep)
	writeTokens(t, path, map[string]string{"reloadConfig": ""})
	c.poll(context.Background())
	if *calls != 0 || rep.count() != 0 {
		t.Fatalf("empty token must not fire: calls=%d outcomes=%d", *calls, rep.count())
	}
}

func TestFailureReportedAndNotRetriedForSameToken(t *testing.T) {
	rep := &recordingReporter{}
	path := filepath.Join(t.TempDir(), "opstrigger.yaml")
	writeTokens(t, path, map[string]string{"resetPeer": ""})
	calls := 0
	c := NewConsumer(path, 0, rep, zap.NewNop())
	c.Register("resetPeer", func(_ context.Context, _ string) error {
		calls++
		return errors.New("boom")
	})

	writeTokens(t, path, map[string]string{"resetPeer": "x1"})
	c.poll(context.Background())
	c.poll(context.Background()) // same failing token — must not retry
	if calls != 1 {
		t.Fatalf("failing action must not retry same token, got %d calls", calls)
	}
	if rep.count() != 1 || !rep.outcomes[0].failed {
		t.Fatalf("expected 1 failed outcome, got %+v", rep.outcomes)
	}
}

func TestPanicInActionIsRecoveredAndReportedFailed(t *testing.T) {
	rep := &recordingReporter{}
	path := filepath.Join(t.TempDir(), "opstrigger.yaml")
	writeTokens(t, path, map[string]string{"drain": ""})
	c := NewConsumer(path, 0, rep, zap.NewNop())
	c.Register("drain", func(_ context.Context, _ string) error {
		panic("kaboom")
	})

	writeTokens(t, path, map[string]string{"drain": "d1"})
	c.poll(context.Background()) // must not crash the test process
	if rep.count() != 1 || !rep.outcomes[0].failed {
		t.Fatalf("panic should be recovered and reported failed, got %+v", rep.outcomes)
	}
}
