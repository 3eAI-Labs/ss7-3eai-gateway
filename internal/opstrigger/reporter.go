// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package opstrigger

import (
	"context"

	"go.uber.org/zap"
)

// Reporter reports the outcome of an executed opsTrigger action. Implementations
// are the standard, product-agnostic observability surfaces Arinna already
// consumes (contract §5): a K8s Event (required) and the process log.
type Reporter interface {
	// Report records that action `name` (idempotency token `token`) ran; a nil
	// actionErr means success.
	Report(ctx context.Context, name, token string, actionErr error)
}

// resultString maps an action error to the contract's result token.
func resultString(actionErr error) string {
	if actionErr == nil {
		return "success"
	}
	return "failed"
}

// message builds the fixed-schema outcome message required by contract §5:
//
//	action=<a> token=<echo> result=success|failed
func message(name, token string, actionErr error) string {
	msg := "action=" + name + " token=" + token + " result=" + resultString(actionErr)
	if actionErr != nil {
		msg += " error=" + actionErr.Error()
	}
	return msg
}

// LogReporter always logs the outcome; it is the fallback when no K8s Event
// surface is available (e.g. running outside a cluster, in tests) and is always
// included so an outcome is never silently lost.
type LogReporter struct{ logger *zap.Logger }

// NewLogReporter returns a LogReporter.
func NewLogReporter(logger *zap.Logger) *LogReporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &LogReporter{logger: logger}
}

// Report logs at Info on success, Error on failure.
func (r *LogReporter) Report(_ context.Context, name, token string, actionErr error) {
	fields := []zap.Field{
		zap.String("action", name),
		zap.String("token", token),
		zap.String("result", resultString(actionErr)),
	}
	if actionErr != nil {
		r.logger.Error("opsTrigger action failed", append(fields, zap.Error(actionErr))...)
		return
	}
	r.logger.Info("opsTrigger action completed", fields...)
}

// MultiReporter fans an outcome out to several reporters. A panic or slow
// reporter in one does not block the others in the common case (they run
// sequentially but each is expected to be non-blocking / bounded).
type MultiReporter struct{ reporters []Reporter }

// NewMultiReporter composes reporters, dropping nils.
func NewMultiReporter(reporters ...Reporter) *MultiReporter {
	kept := make([]Reporter, 0, len(reporters))
	for _, r := range reporters {
		if r != nil {
			kept = append(kept, r)
		}
	}
	return &MultiReporter{reporters: kept}
}

// Report forwards to every composed reporter.
func (m *MultiReporter) Report(ctx context.Context, name, token string, actionErr error) {
	for _, r := range m.reporters {
		r.Report(ctx, name, token, actionErr)
	}
}
