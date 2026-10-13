// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

// Package opstrigger implements the Arinna packaging-contract §5 declarative
// day-2 trigger consumer: it observes opsTrigger.<action> tokens delivered via
// the mounted ConfigMap, performs the mapped action idempotently (same token ⇒
// no repeat), and reports the outcome on a K8s Event (Arinna reads it via
// k8s-mcp) — the product CR is never touched.
package opstrigger

import "sync/atomic"

// DrainState is the process-wide graceful-drain flag. The `drain` action sets
// it; the API readiness probe (/api/v1/ready) reads it so Kubernetes stops
// routing new traffic to a draining pod while liveness (/api/v1/health) stays
// up and in-flight dialogues finish.
type DrainState struct {
	draining atomic.Bool
}

// NewDrainState returns a not-draining state.
func NewDrainState() *DrainState { return &DrainState{} }

// SetDraining flips the drain flag. Idempotent.
func (d *DrainState) SetDraining(v bool) { d.draining.Store(v) }

// Draining reports whether the node is draining.
func (d *DrainState) Draining() bool { return d.draining.Load() }
