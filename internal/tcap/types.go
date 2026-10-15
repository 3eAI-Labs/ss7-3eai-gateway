// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package tcap

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"go.uber.org/zap"
)

// MessageType represents TCAP message types (ITU-T Q.773)
type MessageType uint8

const (
	MsgTypeUnidirectional MessageType = 0x61 // 97
	MsgTypeBegin          MessageType = 0x62 // 98
	MsgTypeEnd            MessageType = 0x64 // 100
	MsgTypeContinue       MessageType = 0x65 // 101
	MsgTypeAbort          MessageType = 0x67 // 103
)

// ComponentType represents TCAP component types (ITU-T Q.773)
type ComponentType uint8

const (
	CompTypeInvoke       ComponentType = 0xA1 // 161
	CompTypeReturnResult ComponentType = 0xA2 // 162
	CompTypeReturnError  ComponentType = 0xA3 // 163
	CompTypeReject       ComponentType = 0xA4 // 164
)

// AbortCause represents TCAP abort cause (ITU-T Q.773)
type AbortCause uint8

const (
	AbortUnrecognizedMessageType     AbortCause = 0x00
	AbortUnrecognizedTransactionID   AbortCause = 0x01
	AbortBadlyFormattedTransaction   AbortCause = 0x02
	AbortIncorrectTransactionPortion AbortCause = 0x03
	AbortResourceLimitation          AbortCause = 0x04
	AbortAbnormalDialogue            AbortCause = 0x05
	AbortNoCommonDialoguePortion     AbortCause = 0x06
)

// FailureReason classifies the way an originated dialogue ended without a
// ReturnResult so the upstream NATS waiter can pick a retry strategy.
// Distinguishing TCAP-level failure (Timeout / AbortReceived) from
// MAP-level rejection (RemoteError) matters because GSMA SMS-IWMSC
// retry guidance differs: T-layer failures imply transport retry with
// longer T_R; remote errors imply protocol-level decision per error code.
type FailureReason uint8

const (
	FailureTimeout       FailureReason = iota // local DialogueTimeout — peer never replied
	FailureAbortReceived                      // P-ABORT or U-ABORT received from peer
	FailureRemoteError                        // unused by TCAP: a MAP ReturnError / Reject is reported by mapprotocol's ErrorSink with the same REMOTE_ERROR token
	// FailureNotDelivered: the BEGIN never reached the peer (no SCCP route,
	// M3UA could not send it, or the network returned it in a UDTS); the
	// dialogue is ended locally. Reported as ABORTED — the dialogue did not
	// complete and no answer will come — with the reason in the detail.
	FailureNotDelivered
)

// String renders the reason for logs and NATS payloads. The exact tokens are
// part of the bridge's wire contract — change with care.
func (r FailureReason) String() string {
	switch r {
	case FailureTimeout:
		return "TIMEOUT"
	case FailureAbortReceived:
		return "ABORTED"
	case FailureRemoteError:
		return "REMOTE_ERROR"
	case FailureNotDelivered:
		return "ABORTED"
	}
	return "UNKNOWN"
}

// FailureSink is fired exactly once per originated dialogue that ends
// without a ReturnResult. Composition root republishes on the matching
// NATS reply subject so the upstream waiter unblocks instead of timing
// out blind. corrID is the dialogue's NATSCorrelationID; opCode is the
// originating MAP/CAP op (e.g. 44 MtForwardSM, 45 SRI-for-SM); detail
// is a free-form diagnostic (TCAP abort cause name, etc.) — do not
// parse it programmatically.
type FailureSink func(natsCorrelationID string, opCode int32, reason FailureReason, detail string)

// ProblemCode represents TCAP problem codes (ITU-T Q.773 §3.1.5.3).
type ProblemCode struct {
	Type uint8 // General (0), Invoke (1), ReturnResult (2), ReturnError (3)
	Code uint8
}

// Problem-type constants per Q.773 §3.1.5.3.
const (
	ProblemTypeGeneral      uint8 = 0
	ProblemTypeInvoke       uint8 = 1
	ProblemTypeReturnResult uint8 = 2
	ProblemTypeReturnError  uint8 = 3
)

// Invoke-problem codes per Q.773 §3.1.5.3.3 (Type=ProblemTypeInvoke).
const (
	InvokeProblemDuplicateInvokeID         uint8 = 0
	InvokeProblemUnrecognizedOperation     uint8 = 1
	InvokeProblemMistypedParameter         uint8 = 2
	InvokeProblemResourceLimitation        uint8 = 3
	InvokeProblemInitiatingRelease         uint8 = 4
	InvokeProblemUnrecognizedLinkedID      uint8 = 5
	InvokeProblemLinkedResponseUnexpected  uint8 = 6
	InvokeProblemUnexpectedLinkedOperation uint8 = 7
)

// ApplicationContext represents TCAP application context
type ApplicationContext struct {
	OID  []int  // Object Identifier
	Name string // Human-readable name
}

// Common Application Contexts (ITU-T Q.773)
var (
	ACMapV1 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 1, 1},
		Name: "map-ac-networkLocUp-v1",
	}
	ACMapV2 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 1, 2},
		Name: "map-ac-networkLocUp-v2",
	}
	ACMapV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 1, 3},
		Name: "map-ac-networkLocUp-v3",
	}

	// Short message application contexts, TS 29.002 §17.3.3:
	// {map-ac shortMsgGateway(20) version3(3)} etc., map-ac = 0.4.0.0.1.0.
	// SRI-for-SM runs under shortMsgGateway, MT-Forward-SM under
	// shortMsgMT-Relay and MO-Forward-SM under shortMsgMO-Relay (named
	// shortMsgRelay before v3).
	ACShortMsgGatewayV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 20, 3},
		Name: "shortMsgGatewayContext-v3",
	}
	ACShortMsgMORelayV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 21, 3},
		Name: "shortMsgMO-RelayContext-v3",
	}
	ACShortMsgMTRelayV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 25, 3},
		Name: "shortMsgMT-RelayContext-v3",
	}

	// Version 2 equivalents (TS 29.002 §17.3.2.21, §17.3.2.26), used only
	// to fall back when a peer refuses version 3 (§5.2.1).
	ACShortMsgGatewayV2 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 20, 2},
		Name: "shortMsgGatewayContext-v2",
	}
	ACShortMsgMTRelayV2 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 25, 2},
		Name: "shortMsgMT-RelayContext-v2",
	}
	// Subscriber location and state, TS 29.002 §17.3.2.28–29, both "v3
	// only": ProvideSubscriberInfo (HLR → VLR) under subscriberInfoEnquiry,
	// AnyTimeInterrogation (gsmSCF → HLR) under anyTimeInfoEnquiry.
	ACSubscriberInfoEnquiryV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 28, 3},
		Name: "subscriberInfoEnquiryContext-v3",
	}
	ACAnyTimeInfoEnquiryV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 29, 3},
		Name: "anyTimeInfoEnquiryContext-v3",
	}
	// Call routing, TS 29.002 §17.3.2.6: SendRoutingInfo (GMSC → HLR)
	// under locationInfoRetrieval. Versions 1 and 2 exist; the gateway uses
	// 3 only — the version 2 ASN.1 is no longer in TS 29.002.
	ACLocationInfoRetrievalV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 5, 3},
		Name: "locationInfoRetrievalContext-v3",
	}
	// USSD, TS 29.002 §17.3.2.20: between HLR and gsmSCF either side opens
	// it. Version 2 only (version 1 is networkFunctionalSs).
	ACNetworkUnstructuredSsV2 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 19, 2},
		Name: "networkUnstructuredSsContext-v2",
	}
	// Location management with the gateway as HLR, TS 29.002 §17.3.2.2–4
	// and §17.3.2.17: a VLR opens networkLocUp (updateLocation, the HLR's
	// insertSubscriberData in the same dialogue); the HLR opens
	// locationCancellation (cancelLocation), roamingNumberEnquiry
	// (provideRoamingNumber) and subscriberDataMngt (insertSubscriberData
	// alone). Version 3 only. ACNetworkLocUpV3 has ACMapV3's OID.
	ACNetworkLocUpV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 1, 3},
		Name: "networkLocUpContext-v3",
	}
	ACLocationCancellationV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 2, 3},
		Name: "locationCancellationContext-v3",
	}
	ACRoamingNumberEnquiryV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 3, 3},
		Name: "roamingNumberEnquiryContext-v3",
	}
	ACSubscriberDataMngtV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 16, 3},
		Name: "subscriberDataMngtContext-v3",
	}
	ACCapV2 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 50, 1},
		Name: "cap-ac-gsmSCF-gsmSSF-v2",
	}
	ACCapV3 = ApplicationContext{
		OID:  []int{0, 4, 0, 0, 1, 0, 50, 2},
		Name: "cap-ac-gsmSCF-gsmSSF-v3",
	}
)

// VersionFallback picks the retry for an originated dialogue whose context
// the peer refused, proposing another (TS 29.002 §5.2.1): the context to
// use and the operation code to send under it (an operation may differ
// between versions). ok is false when no fallback applies.
type VersionFallback func(refused []int, logicalOp int32, proposed []int) (ac *ApplicationContext, wireOp int32, ok bool)

// Component represents a TCAP component
type Component struct {
	Type          ComponentType
	InvokeID      *int32 // Invoke ID (nil for some component types)
	LinkedID      *int32 // Linked ID (Invoke only)
	OperationCode *int32 // Operation code (for Invoke)
	Parameter     []byte // ASN.1 encoded parameters
	ProblemCode   *ProblemCode
	ErrorCode     *int32
	// NotLast marks a ReturnResult sent as Return Result Not Last: one
	// segment of a result that a Return Result Last completes.
	NotLast bool
}

// DialoguePortion represents TCAP dialogue portion
//
// Which dialogue PDU it encodes as follows from the message type (Q.773
// §4.2.1): AARQ in a BEGIN, AARE (accepted) in the END or CONTINUE that first
// answers one, and in a TC-U-ABORT an AARE whose result is reject-permanent
// with "application-context-name-not-supported" when Rejected is set.
type DialoguePortion struct {
	ApplicationContext *ApplicationContext
	// UserInformation is the content of user-information [30]: the
	// EXTERNALs, encoded, without the [30] tag and length.
	UserInformation []byte
	// Rejected: an AARE with result reject-permanent (received), or the
	// AARE to send refuses the context (application-context-name-not-
	// supported).
	Rejected bool
	// PDU is the dialogue PDU received (AARQ, AARE, ABRT); ignored when
	// encoding, where the message type decides.
	PDU DialoguePDUType
	// AbortSourceProvider: an ABRT from the dialogue service provider
	// rather than the user.
	AbortSourceProvider bool
}

// DialoguePDUType is the [APPLICATION n] tag of a dialogue PDU (Q.773 §3.2.1).
type DialoguePDUType uint8

const (
	DialoguePDUAARQ DialoguePDUType = 0
	DialoguePDUAARE DialoguePDUType = 1
	DialoguePDUABRT DialoguePDUType = 4
)

// Message represents a TCAP message
type Message struct {
	MessageType     MessageType
	OriginatingTID  []byte // Transaction ID (OTID)
	DestinationTID  []byte // Transaction ID (DTID)
	DialoguePortion *DialoguePortion
	Components      []*Component
	AbortCause      *AbortCause
	PAbort          bool // true = P-Abort, false = U-Abort

	// Dialogue is an optional in-process back-reference set by the application
	// when emitting an outbound response. The TCAP outbound path prefers this
	// over the wire-TID-based dialogue lookup, since for END the wire DTID is
	// the peer's TID — looking up a peer's TID in our local-TID-keyed dialogue
	// map would miss. Not encoded on the wire.
	Dialogue *Dialogue
}

// Dialogue represents an active TCAP dialogue
type Dialogue struct {
	LocalTID uint32
	// RemoteTID is the peer's transaction ID. A CONTINUE may set it after
	// the dialogue is shared; code outside the dialogue's TCAP worker reads
	// it with PeerTID.
	RemoteTID          []byte
	remoteTIDMu        sync.Mutex
	State              DialogueState
	ApplicationContext *ApplicationContext

	// Addresses pinned from the BEGIN/UNI that opened the dialogue (Q.713).
	// LocalAddress is "us" (CdPA on incoming, CgPA on outgoing); RemoteAddress
	// is the peer. Outgoing TCAP messages within this dialogue use these as
	// CalledAddress=Remote, CallingAddress=Local.
	LocalAddress  *sccp.SccpAddress
	RemoteAddress *sccp.SccpAddress

	// Origination context — set when the gateway opens the dialogue (vs
	// receives a BEGIN). NATSCorrelationID lets the response path republish
	// the answer onto the originating NATS request. OriginatingOpCode lets
	// the application dispatcher pick the right ReturnResult decoder: a
	// ReturnResult without a result carries no op code, and the op that was
	// asked for, not the one echoed, decides how the answer is read.
	NATSCorrelationID string
	OriginatingOpCode int32

	// Timing
	CreatedAt    time.Time
	LastActivity time.Time
	ExpiresAt    time.Time

	// dialogueResponsePending is set when the peer opened the dialogue with
	// an AARQ; the first END or CONTINUE we send must answer it with an AARE
	// (Q.773 §4.2.1), after which it is cleared.
	dialogueResponsePending atomic.Bool

	// deferredComponents are held back by an "empty Begin" (TS 29.002
	// §C.2.1): the BEGIN carried only the dialogue portion because the
	// whole message would not fit one SCCP unitdata; they go in the first
	// CONTINUE, once the peer has accepted the dialogue. Guarded by
	// invokesMu.
	deferredComponents []*Component

	// awaitingContinuation marks a dialogue the peer opened with an empty
	// Begin that we accepted, until its CONTINUE arrives. Such dialogues
	// hold state for a peer that has sent no operation yet, so they are
	// bounded (count and lifetime; see handleBegin).
	awaitingContinuation atomic.Bool

	// initiatedLocally marks a dialogue this node opened (Originate).
	initiatedLocally bool
	// OpeningUserInformation is the user-information of the AARQ the peer
	// opened the dialogue with (its EXTERNALs, encoded), kept for an
	// operation that arrives in the CONTINUE after an empty Begin. Set
	// before the dialogue is registered; read-only after.
	OpeningUserInformation []byte
	// timeout, when set, replaces DialogueTimeout for this dialogue
	// (OriginateWith, SetDialogueTimeout). Guarded by Handler.dialoguesMu.
	timeout time.Duration
	// versionFallbackUsed marks a dialogue that is already the lower-version
	// retry of a refused one; it is not retried again.
	versionFallbackUsed bool

	// answered is set once the originator of the dialogue has been given
	// its outcome — a result, an error, or a failure — so that it is given
	// exactly one (see MarkAnswered).
	answered atomic.Bool

	// resultSegments are the parameters of the Return Result Not Last
	// components received so far (TS 29.002 §C.2.3), held for the
	// application until the Return Result Last; resultOverflow records that
	// they went past the bounds and were dropped. Guarded by invokesMu.
	resultSegments      [][]byte
	resultSegmentOctets int
	resultOverflow      bool

	// Pending operations
	PendingInvokes map[int32]*PendingInvoke
	invokesMu      sync.RWMutex

	// Statistics
	MessagesReceived   atomic.Uint64
	MessagesSent       atomic.Uint64
	ComponentsReceived atomic.Uint64
	ComponentsSent     atomic.Uint64
}

// MarkAnswered records that the dialogue's originator has been given its
// outcome. It reports true to the first caller only: whoever gets true
// publishes the outcome, anyone after drops theirs. A result that arrives
// in a CONTINUE is therefore not followed by a timeout, nor an error by a
// failure.
func (d *Dialogue) MarkAnswered() bool {
	return d.answered.CompareAndSwap(false, true)
}

// PeerTID is RemoteTID, safe to read from any goroutine.
func (d *Dialogue) PeerTID() []byte {
	d.remoteTIDMu.Lock()
	defer d.remoteTIDMu.Unlock()
	return d.RemoteTID
}

// InitiatedLocally reports whether this node opened the dialogue.
func (d *Dialogue) InitiatedLocally() bool { return d.initiatedLocally }

// DialogueState represents the state of a TCAP dialogue
type DialogueState uint8

const (
	StateIdle        DialogueState = 0x00
	StateInitWait    DialogueState = 0x01 // Waiting for response after BEGIN
	StateActive      DialogueState = 0x02 // Dialogue active
	StateTerminating DialogueState = 0x03 // Waiting for END/ABORT
)

// PendingInvoke represents an invoke waiting for response
type PendingInvoke struct {
	InvokeID      int32
	OperationCode int32
	SentAt        time.Time
	ExpiresAt     time.Time
	Parameter     []byte
	RetryCount    int
}

// Handler manages TCAP protocol operations
type Handler struct {
	config *Config
	nodeID uint8 // For distributed TID generation (0-255)

	// Dialogue management
	dialogues   map[uint32]*Dialogue
	dialoguesMu sync.RWMutex

	// TID generation
	tidCounter  atomic.Uint32
	tidSequence atomic.Uint64

	// SCCP integration. Owner: sccp.Handler. sccpIncomingCh delivers messages
	// already routed to a local subsystem (Q.714 deliverLocal). sccpOutgoingCh
	// receives wrapped TCAP responses for forwarding back via M3UA.
	sccpIncomingCh <-chan *sccp.SccpMessage
	// workerIn are the workers' inbound queues (see transactionShard).
	workerIn       []chan *sccp.SccpMessage
	sccpOutgoingCh chan<- *sccp.SccpMessage

	// Application layer integration (MAP/CAP/INAP) — routed by AC OID via Dispatcher.
	// Q.773 §3.1.4: AC is dialogue-scoped (set on BEGIN), not message-scoped.
	dispatcher    *Dispatcher
	appOutgoingCh chan *Message // owned by Handler; bidi internally, exposed write-only

	// maxUserData returns how much TCAP data one SCCP unitdata can carry
	// between two addresses (see SetUserDataLimit); nil means the 255-octet
	// data parameter limit.
	maxUserData func(called, calling *sccp.SccpAddress) int

	// versionFallback chooses the lower-version retry of a refused dialogue
	// (see SetVersionFallback); nil disables fallback.
	versionFallback VersionFallback

	// pendingEmptyBegins counts dialogues with awaitingContinuation set.
	pendingEmptyBegins atomic.Int64

	// Control channels
	errorCh    chan error
	shutdownCh chan struct{}

	// wg tracks workers and the timer manager so Stop can wait for them to
	// drain. Without this, Stop would race with goroutines still emitting
	// log lines, panicking under -race when zaptest cores fire after the
	// test completes.
	wg sync.WaitGroup

	// Timers
	timerManager *TimerManager

	// Metrics
	metrics *Metrics
	logger  *zap.Logger

	// failureSink fires when an originated dialogue ends without a
	// ReturnResult (timeout / inbound ABORT). nil disables — useful in
	// tests that don't wire the NATS bridge. Installed via SetFailureSink.
	failureSink FailureSink
	// lostSink is told of any dialogue TCAP ends on its own (timeout,
	// non-delivery), whoever opened it.
	lostSink func(d *Dialogue, reason FailureReason, detail string)

	// respondingAddress gives the calling address of this end of a dialogue
	// a peer opened, from the called address its BEGIN came to.
	respondingAddress func(called *sccp.SccpAddress) *sccp.SccpAddress
}

// Config holds TCAP handler configuration
type Config struct {
	// Node configuration
	NodeID uint8 `yaml:"node_id" json:"node_id"` // 0-255 for distributed TID

	// Dialogue timeouts (ITU-T Q.774)
	DialogueTimeout time.Duration `yaml:"dialogue_timeout" json:"dialogue_timeout"`
	InvokeTimeout   time.Duration `yaml:"invoke_timeout" json:"invoke_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout" json:"idle_timeout"`

	// Limits
	MaxDialogues            int `yaml:"max_dialogues" json:"max_dialogues"`
	MaxComponentsPerMessage int `yaml:"max_components_per_message" json:"max_components_per_message"`
	MaxInvokesPerDialogue   int `yaml:"max_invokes_per_dialogue" json:"max_invokes_per_dialogue"`

	// Performance
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	TimerResolution   time.Duration `yaml:"timer_resolution" json:"timer_resolution"`
}

// TimerManager handles dialogue and invoke timeouts.
type TimerManager struct {
	handler    *Handler
	shutdownCh chan struct{}
}

// Metrics tracks TCAP statistics
type Metrics struct {
	MessagesReceived   atomic.Uint64
	MessagesSent       atomic.Uint64
	ComponentsReceived atomic.Uint64
	ComponentsSent     atomic.Uint64

	// Message type counters
	BeginReceived    atomic.Uint64
	ContinueReceived atomic.Uint64
	EndReceived      atomic.Uint64
	AbortReceived    atomic.Uint64

	BeginSent    atomic.Uint64
	ContinueSent atomic.Uint64
	EndSent      atomic.Uint64
	AbortSent    atomic.Uint64

	// Component type counters
	InvokesReceived atomic.Uint64
	ResultsReceived atomic.Uint64
	ErrorsReceived  atomic.Uint64
	RejectsReceived atomic.Uint64

	InvokesSent atomic.Uint64
	ResultsSent atomic.Uint64
	ErrorsSent  atomic.Uint64
	RejectsSent atomic.Uint64

	// Dialogue statistics
	ActiveDialogues  atomic.Int64
	TotalDialogues   atomic.Uint64
	DialogueTimeouts atomic.Uint64
	InvokeTimeouts   atomic.Uint64

	// Errors
	Errors           atomic.Uint64
	ParseErrors      atomic.Uint64
	ValidationErrors atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents handler health
type HealthStatus struct {
	ActiveDialogues    int64     `json:"active_dialogues"`
	TotalDialogues     uint64    `json:"total_dialogues"`
	MessagesReceived   uint64    `json:"messages_received"`
	MessagesSent       uint64    `json:"messages_sent"`
	ComponentsReceived uint64    `json:"components_received"`
	ComponentsSent     uint64    `json:"components_sent"`
	DialogueTimeouts   uint64    `json:"dialogue_timeouts"`
	InvokeTimeouts     uint64    `json:"invoke_timeouts"`
	Errors             uint64    `json:"errors"`
	LastUpdate         time.Time `json:"last_update"`
}

// DialogueInfo contains dialogue information for API
type DialogueInfo struct {
	LocalTID           uint32    `json:"local_tid"`
	RemoteTID          string    `json:"remote_tid"` // Hex encoded
	State              string    `json:"state"`
	ApplicationContext string    `json:"application_context"`
	CreatedAt          time.Time `json:"created_at"`
	LastActivity       time.Time `json:"last_activity"`
	ExpiresAt          time.Time `json:"expires_at"`
	MessagesReceived   uint64    `json:"messages_received"`
	MessagesSent       uint64    `json:"messages_sent"`
	PendingInvokes     int       `json:"pending_invokes"`
}

// Bounds on a segmented result (Return Result Not Last components) held for
// one dialogue. A real result is a few segments of at most one TCAP message
// each; the bounds keep a peer from holding memory with an endless stream.
const (
	MaxResultSegments      = 16
	MaxResultSegmentOctets = 16 * 1024
)

// AddResultSegment holds the parameter of a Return Result Not Last
// component until the Return Result Last. It reports false, and holds
// nothing more, once the bounds are exceeded.
func (d *Dialogue) AddResultSegment(param []byte) bool {
	d.invokesMu.Lock()
	defer d.invokesMu.Unlock()
	if d.resultOverflow {
		return false
	}
	if len(d.resultSegments) >= MaxResultSegments || d.resultSegmentOctets+len(param) > MaxResultSegmentOctets {
		d.resultOverflow = true
		d.resultSegments, d.resultSegmentOctets = nil, 0
		return false
	}
	d.resultSegments = append(d.resultSegments, param)
	d.resultSegmentOctets += len(param)
	return true
}

// TakeResultSegments returns, and forgets, the segments held by
// AddResultSegment; overflow reports that some were dropped.
func (d *Dialogue) TakeResultSegments() (segments [][]byte, overflow bool) {
	d.invokesMu.Lock()
	defer d.invokesMu.Unlock()
	segments, overflow = d.resultSegments, d.resultOverflow
	d.resultSegments, d.resultSegmentOctets, d.resultOverflow = nil, 0, false
	return segments, overflow
}
