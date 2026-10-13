// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package opstrigger

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// DefaultPollInterval is how often the mounted token file is re-read. ConfigMap
// updates land on the pod with kubelet's sync delay (tens of seconds), so a
// low-frequency poll is sufficient and robust against the atomic symlink swap
// that breaks naive fsnotify watches.
const DefaultPollInterval = 15 * time.Second

// DefaultActionTimeout bounds a single action execution.
const DefaultActionTimeout = 60 * time.Second

// Action performs a day-2 operation. It MUST be idempotent for a given token;
// the consumer additionally guarantees it is not invoked twice for the same
// token value.
type Action func(ctx context.Context, token string) error

// tokenFile is the on-disk shape of opstrigger.yaml (rendered by the chart).
type tokenFile struct {
	OpsTrigger map[string]string `yaml:"opsTrigger"`
}

// Consumer watches the opsTrigger token file and dispatches changed tokens to
// registered actions, reporting each outcome.
type Consumer struct {
	path          string
	interval      time.Duration
	actionTimeout time.Duration
	actions       map[string]Action
	lastToken     map[string]string
	reporter      Reporter
	logger        *zap.Logger
}

// NewConsumer creates a Consumer reading `path` every `interval` (0 ⇒ default).
func NewConsumer(path string, interval time.Duration, reporter Reporter, logger *zap.Logger) *Consumer {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if reporter == nil {
		reporter = NewLogReporter(logger)
	}
	return &Consumer{
		path:          path,
		interval:      interval,
		actionTimeout: DefaultActionTimeout,
		actions:       make(map[string]Action),
		lastToken:     make(map[string]string),
		reporter:      reporter,
		logger:        logger,
	}
}

// Register maps an action name (the opsTrigger key) to its handler. Not
// goroutine-safe; call before Start.
func (c *Consumer) Register(name string, action Action) {
	c.actions[name] = action
}

// readTokens parses the token file. A missing file is treated as "all tokens
// empty" (the chart always renders it, but tolerate absence for safety).
func (c *Consumer) readTokens() (map[string]string, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read token file: %w", err)
	}
	var tf tokenFile
	if err := yaml.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	if tf.OpsTrigger == nil {
		return map[string]string{}, nil
	}
	return tf.OpsTrigger, nil
}

// Start baselines the current tokens (so pre-existing tokens do NOT fire on a
// fresh pod — a just-started process is equivalent to having applied its config)
// then polls until ctx is cancelled. Runs in the caller's goroutine? No — it
// spawns its own loop and returns immediately.
func (c *Consumer) Start(ctx context.Context) {
	c.baseline()
	c.logger.Info("opsTrigger consumer baselined",
		zap.String("path", c.path),
		zap.Int("actions", len(c.actions)),
		zap.Duration("interval", c.interval))
	go c.loop(ctx)
}

// baseline records the current tokens as already-seen without dispatching, so a
// fresh pod does not re-run whatever token happened to be in the ConfigMap.
func (c *Consumer) baseline() {
	base, err := c.readTokens()
	if err != nil {
		c.logger.Warn("opsTrigger: baseline read failed; starting from empty", zap.Error(err))
		return
	}
	for name := range c.actions {
		c.lastToken[name] = base[name]
	}
}

func (c *Consumer) loop(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("opsTrigger consumer stopped")
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

// poll reads the tokens once and dispatches any that changed to a new non-empty
// value. Exported-ish via tests through pollOnce.
func (c *Consumer) poll(ctx context.Context) {
	tokens, err := c.readTokens()
	if err != nil {
		c.logger.Warn("opsTrigger: token read failed", zap.Error(err))
		return
	}
	for name, action := range c.actions {
		token := tokens[name]
		if token == "" || token == c.lastToken[name] {
			// no request, or same token already processed (idempotency)
			c.lastToken[name] = token
			continue
		}
		c.dispatch(ctx, name, token, action)
		// Advance past this token regardless of outcome: a failed action is
		// reported (Warning Event) and retried only by issuing a NEW token,
		// which prevents a failing action from re-firing every poll.
		c.lastToken[name] = token
	}
}

func (c *Consumer) dispatch(ctx context.Context, name, token string, action Action) {
	c.logger.Info("opsTrigger action requested",
		zap.String("action", name), zap.String("token", token))
	actCtx, cancel := context.WithTimeout(ctx, c.actionTimeout)
	defer cancel()

	var actionErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				actionErr = fmt.Errorf("panic: %v", r)
			}
		}()
		actionErr = action(actCtx, token)
	}()

	c.reporter.Report(ctx, name, token, actionErr)
}
