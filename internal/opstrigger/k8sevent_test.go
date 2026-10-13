// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package opstrigger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestK8sEventReporterPostsCoreV1Event verifies the reporter POSTs a well-formed
// core/v1 Event to the namespaced events endpoint with the contract's
// reason/message schema and the pod as involvedObject.
func TestK8sEventReporterPostsCoreV1Event(t *testing.T) {
	var gotPath, gotAuth string
	var payload map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Event"}`))
	}))
	defer srv.Close()

	rep := NewK8sEventReporter(K8sEventConfig{
		Client:    srv.Client(),
		APIURL:    srv.URL,
		Token:     "test-token",
		Namespace: "ss7",
		PodName:   "ss7-3eai-gateway-0",
		PodUID:    "uid-123",
	}, zap.NewNop())

	rep.Report(context.Background(), "flushTDR", "tok-9", nil)

	if want := "/api/v1/namespaces/ss7/events"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if payload["reason"] != "OpsTriggerCompleted" {
		t.Errorf("reason = %v, want OpsTriggerCompleted", payload["reason"])
	}
	if payload["type"] != "Normal" {
		t.Errorf("type = %v, want Normal", payload["type"])
	}
	msg, _ := payload["message"].(string)
	if !strings.Contains(msg, "action=flushTDR") || !strings.Contains(msg, "token=tok-9") || !strings.Contains(msg, "result=success") {
		t.Errorf("message = %q, want action/token/result", msg)
	}
	invObj, _ := payload["involvedObject"].(map[string]interface{})
	if invObj["kind"] != "Pod" || invObj["name"] != "ss7-3eai-gateway-0" || invObj["uid"] != "uid-123" {
		t.Errorf("involvedObject = %v", invObj)
	}
}

// TestK8sEventReporterFailureIsWarning verifies a failed action produces a
// Warning Event with reason OpsTriggerFailed and result=failed.
func TestK8sEventReporterFailureIsWarning(t *testing.T) {
	var payload map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	rep := NewK8sEventReporter(K8sEventConfig{
		Client: srv.Client(), APIURL: srv.URL, Namespace: "ss7", PodName: "p0",
	}, zap.NewNop())
	rep.Report(context.Background(), "resetPeer", "t7", errors.New("sctp down"))

	if payload["reason"] != "OpsTriggerFailed" || payload["type"] != "Warning" {
		t.Errorf("reason/type = %v/%v", payload["reason"], payload["type"])
	}
	msg, _ := payload["message"].(string)
	if !strings.Contains(msg, "result=failed") || !strings.Contains(msg, "error=sctp down") {
		t.Errorf("message = %q", msg)
	}
}

// TestK8sEventReporterNon2xxDoesNotPanic ensures a rejecting API server is
// handled gracefully (logged, no propagation).
func TestK8sEventReporterNon2xxDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()
	rep := NewK8sEventReporter(K8sEventConfig{Client: srv.Client(), APIURL: srv.URL, Namespace: "ss7", PodName: "p0"}, zap.NewNop())
	rep.Report(context.Background(), "drain", "t1", nil) // must not panic
}
