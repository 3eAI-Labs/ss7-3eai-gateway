// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package opstrigger

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	reasonCompleted = "OpsTriggerCompleted"
	reasonFailed    = "OpsTriggerFailed"
	eventComponent  = "ss7-opstrigger"
)

// K8sEventReporter posts a core/v1 Event to the API server for each action
// outcome, associated with this pod (involvedObject). It speaks the API
// directly over net/http (bearer token + cluster CA) to avoid pulling in the
// full client-go dependency tree — keeping the AGPL-free, minimal Go stack.
type K8sEventReporter struct {
	client    *http.Client
	apiURL    string // e.g. https://10.0.0.1:443
	token     string
	namespace string
	podName   string
	podUID    string
	logger    *zap.Logger
}

// K8sEventConfig configures a K8sEventReporter. Used directly by tests; the
// in-cluster constructor fills it from the mounted service-account + downward
// API env.
type K8sEventConfig struct {
	Client    *http.Client
	APIURL    string
	Token     string
	Namespace string
	PodName   string
	PodUID    string
}

// NewInClusterEventReporter builds a reporter from the pod's mounted
// service-account and the downward-API env (POD_NAMESPACE/POD_NAME/POD_UID,
// KUBERNETES_SERVICE_HOST/PORT). It returns an error when any piece is missing
// (e.g. running outside a cluster) so the caller can fall back to log-only.
func NewInClusterEventReporter(logger *zap.Logger) (*K8sEventReporter, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not in-cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	ns := os.Getenv("POD_NAMESPACE")
	name := os.Getenv("POD_NAME")
	if ns == "" || name == "" {
		return nil, fmt.Errorf("POD_NAMESPACE/POD_NAME unset (downward API not wired)")
	}
	tokenBytes, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service-account token: %w", err)
	}
	caBytes, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("parse cluster CA")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	return NewK8sEventReporter(K8sEventConfig{
		Client:    client,
		APIURL:    fmt.Sprintf("https://%s:%s", host, port),
		Token:     string(tokenBytes),
		Namespace: ns,
		PodName:   name,
		PodUID:    os.Getenv("POD_UID"),
	}, logger), nil
}

// NewK8sEventReporter builds a reporter from an explicit config (in-cluster
// constructor and tests both funnel through here).
func NewK8sEventReporter(cfg K8sEventConfig, logger *zap.Logger) *K8sEventReporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &K8sEventReporter{
		client:    client,
		apiURL:    cfg.APIURL,
		token:     cfg.Token,
		namespace: cfg.Namespace,
		podName:   cfg.PodName,
		podUID:    cfg.PodUID,
		logger:    logger,
	}
}

// Report POSTs a core/v1 Event. Failures are logged (the LogReporter carries
// the authoritative record) and never propagate — reporting must not disrupt
// the gateway.
func (r *K8sEventReporter) Report(ctx context.Context, name, token string, actionErr error) {
	reason, etype := reasonCompleted, "Normal"
	if actionErr != nil {
		reason, etype = reasonFailed, "Warning"
	}
	now := time.Now().UTC()
	event := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]interface{}{
			// generateName keeps each outcome a distinct Event (no count-merge
			// surprises) and stays within name limits.
			"generateName": r.podName + ".opstrigger.",
			"namespace":    r.namespace,
		},
		"involvedObject": map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"namespace":  r.namespace,
			"name":       r.podName,
			"uid":        r.podUID,
		},
		"reason":             reason,
		"message":            message(name, token, actionErr),
		"type":               etype,
		"source":             map[string]interface{}{"component": eventComponent},
		"firstTimestamp":     now.Format(time.RFC3339),
		"lastTimestamp":      now.Format(time.RFC3339),
		"eventTime":          now.Format("2006-01-02T15:04:05.000000Z07:00"),
		"count":              1,
		"reportingComponent": eventComponent,
		"reportingInstance":  r.podName,
		"action":             name,
	}
	body, err := json.Marshal(event)
	if err != nil {
		r.logger.Error("opsTrigger: marshal event", zap.Error(err))
		return
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/events", r.apiURL, r.namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.logger.Error("opsTrigger: build event request", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Warn("opsTrigger: post event", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		r.logger.Warn("opsTrigger: event rejected",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", snippet))
	}
}
