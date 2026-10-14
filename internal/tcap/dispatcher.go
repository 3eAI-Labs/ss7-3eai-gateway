// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// AppContextHandler is implemented by application-layer handlers (MAP per
// 3GPP TS 29.002, CAP per 3GPP TS 29.078). The TCAP dispatcher invokes
// HandleTCAPMessage for each incoming dialogue message routed to a handler's
// registered application context.
//
// Implementations must be safe for concurrent invocation. The TCAP layer holds
// no locks on dialogue when calling HandleTCAPMessage; handlers needing
// dialogue-level mutation must take dialogue.invokesMu themselves.
type AppContextHandler interface {
	// HandleTCAPMessage processes one incoming TCAP message for a registered
	// application context. Returning a non-error means the message was
	// accepted (TCAP layer increments dispatch counter). Returning an error
	// means dispatch failure: the TCAP layer increments handler-error counter
	// and the caller decides whether to TCAP-Abort the dialogue.
	HandleTCAPMessage(dialogue *Dialogue, msg *Message) error

	// Name returns a stable identifier for this handler (e.g. "map", "cap").
	// Used for metrics labels and log fields. Must be safe for concurrent reads.
	Name() string
}

var (
	// ErrDuplicateOID indicates an OID is already registered. Carrier-grade
	// strict mode rejects double registration to prevent silent handler-shadow
	// when two protocol modules (e.g. MAP and CAP) declare the same OID.
	ErrDuplicateOID = errors.New("tcap dispatch: application context OID already registered")

	// ErrInvalidOID indicates the OID is empty or contains malformed arcs
	// (negative values, fewer than 2 arcs per ITU-T X.660 §3.5.1).
	ErrInvalidOID = errors.New("tcap dispatch: empty or malformed OID")

	// ErrUnknownAppContext indicates no handler is registered for the message's
	// application context. Caller should respond with TCAP P-Abort, cause
	// no-common-dialogue-portion (ITU-T Q.773 §3.1.5).
	ErrUnknownAppContext = errors.New("tcap dispatch: no handler for application context")
	ErrHandlerPanic      = errors.New("tcap: application handler panicked")

	// ErrMissingAppContext indicates the message has no DialoguePortion or no
	// ApplicationContext within it. Caller should respond with TCAP P-Abort,
	// cause abnormal-dialogue (ITU-T Q.773 §3.1.5).
	ErrMissingAppContext = errors.New("tcap dispatch: message has no application context")

	// ErrNilHandler indicates Register was called with a nil handler.
	ErrNilHandler = errors.New("tcap dispatch: handler cannot be nil")
)

// Dispatcher routes TCAP messages to application-layer handlers based on the
// ASN.1 OID in each message's dialogue portion ApplicationContext.
//
// Carrier-grade strict mode (per GSMA FS.11): exactly-registered OIDs
// dispatch; any other application context is rejected. There is no fallback,
// no longest-prefix match, no version range relaxation. Each (operation,
// version) pair is its own AC OID and must be explicitly registered.
//
// Concurrency: Register and Dispatch are safe for concurrent use. A
// long-running Dispatch does not block Register (RWMutex held only during
// map lookup, released before HandleTCAPMessage is called).
type Dispatcher struct {
	mu     sync.RWMutex
	routes map[string]AppContextHandler // key: canonical dotted OID
	// initiatorRoutes: see RegisterInitiatorOnly.
	initiatorRoutes map[string]AppContextHandler
	logger          *zap.Logger
	metrics         *DispatchMetrics
}

// DispatchMetrics tracks dispatcher-level counters. All fields safe for
// concurrent atomic access.
type DispatchMetrics struct {
	Dispatches     atomic.Uint64 // total successful dispatches (handler returned nil)
	UnknownContext atomic.Uint64 // OID not registered → P-Abort no-common-dialogue-portion
	MissingContext atomic.Uint64 // no AC in message → P-Abort abnormal-dialogue
	HandlerErrors  atomic.Uint64 // handler returned error
	perOID         sync.Map      // string → *atomic.Uint64
}

// NewDispatcher returns a new Dispatcher with no registered routes.
// A nil logger is replaced with zap.NewNop().
func NewDispatcher(logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Dispatcher{
		routes:          make(map[string]AppContextHandler),
		initiatorRoutes: make(map[string]AppContextHandler),
		logger:          logger,
		metrics:         &DispatchMetrics{},
	}
}

// Register associates an application context OID with a handler. Returns
// ErrDuplicateOID if the OID is already registered, ErrInvalidOID for
// malformed OIDs, ErrNilHandler if handler is nil.
//
// Handlers serving multiple application contexts (e.g. MAP networkLocUp v1,
// v2, v3) must call Register once per OID; the dispatcher does not infer
// version ranges.
func (d *Dispatcher) Register(oid []int, handler AppContextHandler) error {
	if handler == nil {
		return ErrNilHandler
	}
	key, err := canonicalOID(oid)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if existing, exists := d.routes[key]; exists {
		return fmt.Errorf("%w: oid=%s existing=%s requested=%s",
			ErrDuplicateOID, key, existing.Name(), handler.Name())
	}

	d.routes[key] = handler
	d.logger.Info("registered application context",
		zap.String("oid", key),
		zap.String("handler", handler.Name()))
	return nil
}

// Dispatch routes a TCAP message to its registered handler. Returns:
//   - nil on successful handler invocation
//   - ErrMissingAppContext if msg has no DialoguePortion/ApplicationContext
//   - ErrUnknownAppContext if the OID is not registered
//   - wrapped handler error if HandleTCAPMessage returned non-nil
//
// Caller is responsible for translating ErrMissingAppContext and
// ErrUnknownAppContext into TCAP P-Abort messages (see ITU-T Q.773 §3.1.5
// for cause codes).
func (d *Dispatcher) Dispatch(dialogue *Dialogue, msg *Message) error {
	if msg == nil || msg.DialoguePortion == nil || msg.DialoguePortion.ApplicationContext == nil {
		d.metrics.MissingContext.Add(1)
		return ErrMissingAppContext
	}

	key, err := canonicalOID(msg.DialoguePortion.ApplicationContext.OID)
	if err != nil {
		d.metrics.MissingContext.Add(1)
		return fmt.Errorf("%w: %v", ErrMissingAppContext, err)
	}

	d.mu.RLock()
	handler, ok := d.routes[key]
	if !ok && dialogue != nil && dialogue.initiatedLocally {
		handler, ok = d.initiatorRoutes[key]
	}
	d.mu.RUnlock()

	if !ok {
		d.metrics.UnknownContext.Add(1)
		d.logger.Warn("no handler for application context",
			zap.String("oid", key),
			zap.String("message_type", msg.MessageType.String()))
		return fmt.Errorf("%w: oid=%s", ErrUnknownAppContext, key)
	}

	if err := d.invoke(handler, dialogue, msg); err != nil {
		d.metrics.HandlerErrors.Add(1)
		return fmt.Errorf("handler %s: %w", handler.Name(), err)
	}

	d.metrics.Dispatches.Add(1)
	d.incrementPerOID(key)
	return nil
}

// LookupHandler returns the handler registered for the given OID, or nil if
// no handler is registered or the OID is malformed. Intended for testing and
// observability; the dispatch path uses Dispatch.
func (d *Dispatcher) LookupHandler(oid []int) AppContextHandler {
	key, err := canonicalOID(oid)
	if err != nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.routes[key]
}

// invoke calls an application handler, turning a panic into an error. The
// handlers decode what peers send; a defect there must cost one message, not
// every dialogue on the node.
func (d *Dispatcher) invoke(handler AppContextHandler, dialogue *Dialogue, msg *Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("application handler panicked",
				zap.String("handler", handler.Name()),
				zap.Any("panic", r),
				zap.Stack("stack"))
			err = fmt.Errorf("%w: %v", ErrHandlerPanic, r)
		}
	}()
	return handler.HandleTCAPMessage(dialogue, msg)
}

// RegisterInitiatorOnly routes a context only for dialogues this node
// opened: the answers to a lower-version retry. A peer opening a dialogue
// under it is still refused (and SupportedAlternative never proposes it).
func (d *Dispatcher) RegisterInitiatorOnly(oid []int, handler AppContextHandler) error {
	if handler == nil {
		return ErrNilHandler
	}
	key, err := canonicalOID(oid)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initiatorRoutes[key] = handler
	return nil
}

// SupportedAlternative returns the context to propose when a peer asks for
// one we do not support: the highest registered version of the same context
// (same arcs except the last), or the proposed context itself when we support
// no version of it (the AARE must name some context; Q.773 §4.2.1).
func (d *Dispatcher) SupportedAlternative(proposed []int) []int {
	if len(proposed) < 2 {
		return proposed
	}
	prefix := proposed[:len(proposed)-1]

	d.mu.RLock()
	defer d.mu.RUnlock()
	best := -1
	for key := range d.routes {
		oid, ok := parseDottedOID(key)
		if !ok || len(oid) != len(proposed) || !slices.Equal(oid[:len(oid)-1], prefix) {
			continue
		}
		if v := oid[len(oid)-1]; v > best {
			best = v
		}
	}
	if best < 0 {
		return proposed
	}
	return append(slices.Clone(prefix), best)
}

func parseDottedOID(s string) ([]int, bool) {
	parts := strings.Split(s, ".")
	oid := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		oid[i] = n
	}
	return oid, true
}

// RegisteredOIDs returns the canonical dotted-string OIDs currently
// registered, in unspecified order. Snapshot at call time; concurrent
// registration may add OIDs not present in the returned slice.
func (d *Dispatcher) RegisteredOIDs() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	oids := make([]string, 0, len(d.routes))
	for k := range d.routes {
		oids = append(oids, k)
	}
	return oids
}

// Metrics returns the dispatcher's metrics struct. Safe for concurrent reads.
func (d *Dispatcher) Metrics() *DispatchMetrics {
	return d.metrics
}

func (d *Dispatcher) incrementPerOID(key string) {
	counter, _ := d.metrics.perOID.LoadOrStore(key, &atomic.Uint64{})
	counter.(*atomic.Uint64).Add(1)
}

// PerOIDCount returns the dispatch count for a specific OID (0 if never
// dispatched or unknown). Used by /health and Prometheus exporters.
func (m *DispatchMetrics) PerOIDCount(oid string) uint64 {
	counter, ok := m.perOID.Load(oid)
	if !ok {
		return 0
	}
	return counter.(*atomic.Uint64).Load()
}

// canonicalOID converts an OID arc slice to its canonical dotted-string form
// (e.g. {0,4,0,0,1,0,1,3} → "0.4.0.0.1.0.1.3"). Per ITU-T X.660 §3.5.1, valid
// OIDs have at least 2 arcs and all arcs are non-negative.
func canonicalOID(oid []int) (string, error) {
	if len(oid) < 2 {
		return "", fmt.Errorf("%w: requires at least 2 arcs, got %d", ErrInvalidOID, len(oid))
	}
	parts := make([]string, len(oid))
	for i, arc := range oid {
		if arc < 0 {
			return "", fmt.Errorf("%w: negative arc at position %d (value=%d)", ErrInvalidOID, i, arc)
		}
		parts[i] = strconv.Itoa(arc)
	}
	return strings.Join(parts, "."), nil
}
