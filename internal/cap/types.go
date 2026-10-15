// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package cap

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// OperationCode represents CAP operation codes (3GPP TS 29.078)
type OperationCode int32

const (
	// CAMEL Phase 1
	OpInitialDP                    OperationCode = 0
	OpAssistRequestInstructions    OperationCode = 16
	OpEstablishTemporaryConnection OperationCode = 17
	OpDisconnectForwardConnection  OperationCode = 18
	OpConnectToResource            OperationCode = 19
	OpConnect                      OperationCode = 20
	OpReleaseCall                  OperationCode = 22
	OpRequestReportBCSMEvent       OperationCode = 23
	OpEventReportBCSM              OperationCode = 24
	OpContinue                     OperationCode = 31
	OpResetTimer                   OperationCode = 33
	OpFurnishChargingInformation   OperationCode = 34
	OpApplyCharging                OperationCode = 35
	OpApplyChargingReport          OperationCode = 36

	// CAMEL Phase 2
	OpCallInformationReport           OperationCode = 44
	OpCallInformationRequest          OperationCode = 45
	OpSendChargingInformation         OperationCode = 46
	OpPlayAnnouncement                OperationCode = 47
	OpPromptAndCollectUserInformation OperationCode = 48
	OpSpecializedResourceReport       OperationCode = 49
	OpCancel                          OperationCode = 53

	// CAMEL Phase 3
	OpActivityTest        OperationCode = 55
	OpInitiateCallAttempt OperationCode = 56
	OpCollectInformation  OperationCode = 57
	OpDisconnectLeg       OperationCode = 58
	OpMoveLeg             OperationCode = 59
	OpSplitLeg            OperationCode = 60

	// CAMEL Phase 4
	OpCallGap               OperationCode = 41
	OpInitialDPSMS          OperationCode = 60
	OpConnectSMS            OperationCode = 61
	OpRequestReportSMSEvent OperationCode = 62
	OpEventReportSMS        OperationCode = 63
	OpContinueSMS           OperationCode = 64
	OpReleaseSMS            OperationCode = 65
	OpResetTimerSMS         OperationCode = 66
)

// ErrorCode represents CAP error codes (3GPP TS 29.078)
type ErrorCode int32

const (
	ErrCancelled                   ErrorCode = 0
	ErrCancelFailed                ErrorCode = 1
	ErrETCFailed                   ErrorCode = 3
	ErrImproperCallerResponse      ErrorCode = 4
	ErrMissingCustomerRecord       ErrorCode = 6
	ErrMissingParameter            ErrorCode = 7
	ErrParameterOutOfRange         ErrorCode = 8
	ErrRequestedInfoError          ErrorCode = 10
	ErrSystemFailure               ErrorCode = 11
	ErrTaskRefused                 ErrorCode = 12
	ErrUnavailableResource         ErrorCode = 13
	ErrUnexpectedComponentSequence ErrorCode = 14
	ErrUnexpectedDataValue         ErrorCode = 15
	ErrUnexpectedParameter         ErrorCode = 16
	ErrUnknownLegID                ErrorCode = 17
	ErrUnknownPDPID                ErrorCode = 50
	ErrUnknownCSID                 ErrorCode = 51
)

// EventTypeBCSM represents BCSM event types (3GPP TS 29.078)
type EventTypeBCSM uint8

const (
	EventCollectedInfo         EventTypeBCSM = 2
	EventRouteSelectFailure    EventTypeBCSM = 4
	EventOCalledPartyBusy      EventTypeBCSM = 5
	EventONoAnswer             EventTypeBCSM = 6
	EventOAnswer               EventTypeBCSM = 7
	EventOMidCall              EventTypeBCSM = 8
	EventODisconnect           EventTypeBCSM = 9
	EventOAbandon              EventTypeBCSM = 10
	EventTermAttemptAuthorized EventTypeBCSM = 12
	EventTBusy                 EventTypeBCSM = 13
	EventTNoAnswer             EventTypeBCSM = 14
	EventTAnswer               EventTypeBCSM = 15
	EventTMidCall              EventTypeBCSM = 16
	EventTDisconnect           EventTypeBCSM = 17
	EventTAbandon              EventTypeBCSM = 18
)

// MonitorMode represents event monitoring mode
type MonitorMode uint8

const (
	MonitorModeInterrupted       MonitorMode = 0
	MonitorModeNotifyAndContinue MonitorMode = 1
	MonitorModeTransparent       MonitorMode = 2
)

// LegType represents call leg identifier
type LegType uint8

const (
	LegID1 LegType = 0x01
	LegID2 LegType = 0x02
)

// InitialDPRequest represents InitialDP operation (3GPP TS 29.078)
type InitialDPRequest struct {
	ServiceKey                   uint32
	CalledPartyNumber            *AddressString
	CallingPartyNumber           *AddressString
	CallingPartysCategory        *uint8
	CGEncountered                *CGEncountered
	IPSSPCapabilities            []byte
	LocationNumber               *AddressString
	OriginalCalledPartyID        *AddressString
	Extensions                   []byte
	HighLayerCompatibility       []byte
	AdditionalCallingPartyNumber *AddressString
	BearerCapability             *BearerCapability
	EventTypeBCSM                *EventTypeBCSM
	RedirectingPartyID           *AddressString
	RedirectionInformation       []byte
	Cause                        []byte
	ServiceInteractionIndicators []byte
	Carrier                      []byte
	CUGIndex                     *int32
	CUGInterlock                 []byte
	ForwardCallIndicators        []byte
	CalledPartyBCDNumber         []byte
	IMSIBytes                    []byte
	SubscriberState              *uint8
	LocationInformation          *LocationInformation
	ExtensionContainer           []byte
	TimeAndTimezone              []byte
	CallReferenceNumber          []byte
}

// EventReportBCSMRequest represents EventReportBCSM operation
type EventReportBCSMRequest struct {
	EventTypeBCSM                EventTypeBCSM
	EventSpecificInformationBCSM *EventSpecificInformationBCSM
	LegID                        *LegType
	MiscCallInfo                 *MiscCallInfo
	Extensions                   []byte
}

// RequestReportBCSMEventRequest represents RequestReportBCSMEvent operation
type RequestReportBCSMEventRequest struct {
	BCMSEvents []*BCSMEvent
	Extensions []byte
}

// BCSMEvent represents BCSM event monitoring
type BCSMEvent struct {
	EventTypeBCSM      EventTypeBCSM
	MonitorMode        MonitorMode
	LegID              *LegType
	DPSpecificCriteria []byte
	AutomaticRearm     bool
}

// ConnectRequest represents Connect operation
type ConnectRequest struct {
	DestinationRoutingAddress    []*AddressString
	AlertingPattern              []byte
	OriginalCalledPartyID        *AddressString
	Extensions                   []byte
	Carrier                      []byte
	CallingPartysCategory        *uint8
	RedirectingPartyID           *AddressString
	RedirectionInformation       []byte
	GenericNumbers               []byte
	ServiceInteractionIndicators []byte
	ChargeNumber                 *AddressString
	LegToBeConnected             *LegType
	CUGInterlock                 []byte
	CUGOutgoingAccess            bool
	SuppressTCSI                 bool
}

// ContinueRequest represents Continue operation
type ContinueRequest struct {
	// Continue has no mandatory parameters
	Extensions []byte
}

// ReleaseCallRequest represents ReleaseCall operation
type ReleaseCallRequest struct {
	Cause      []byte
	Extensions []byte
}

// ApplyChargingRequest represents ApplyCharging operation
type ApplyChargingRequest struct {
	ACHBillingChargingCharacteristics []byte
	PartyToCharge                     *SendingSideID
	Extensions                        []byte
	ACHCorrelationID                  []byte
}

// ApplyChargingReportRequest represents ApplyChargingReport operation
type ApplyChargingReportRequest struct {
	CallResult []byte
}

// FurnishChargingInformationRequest represents FurnishChargingInformation
type FurnishChargingInformationRequest struct {
	FCIBillingChargingCharacteristics []byte
	Extensions                        []byte
}

// SendChargingInformationRequest represents SendChargingInformation
type SendChargingInformationRequest struct {
	SCIBillingChargingCharacteristics []byte
	PartyToCharge                     *SendingSideID
	Extensions                        []byte
}

// CallInformationRequestRequest represents CallInformationRequest
type CallInformationRequestRequest struct {
	RequestedInformationTypeList []RequestedInformationType
	Extensions                   []byte
	LegID                        *LegType
}

// CallInformationReportRequest represents CallInformationReport
type CallInformationReportRequest struct {
	RequestedInformationList []*RequestedInformation
	Extensions               []byte
	LegID                    *LegType
}

// ResetTimerRequest represents ResetTimer operation
type ResetTimerRequest struct {
	TimerID       TimerID
	TimerValue    int32 // seconds
	Extensions    []byte
	CallSegmentID *int32
}

// PlayAnnouncementRequest represents PlayAnnouncement operation
type PlayAnnouncementRequest struct {
	InformationToSend           *InformationToSend
	DisconnectFromIPForbidden   bool
	RequestAnnouncementComplete bool
	Extensions                  []byte
	ConnectedParty              *SendingSideID
}

// PromptAndCollectUserInformationRequest represents PromptAndCollectUserInformation
type PromptAndCollectUserInformationRequest struct {
	CollectedInfo             *CollectedInfo
	DisconnectFromIPForbidden bool
	InformationToSend         *InformationToSend
	Extensions                []byte
}

// Supporting types

// AddressString represents E.164 address
type AddressString struct {
	Nature     uint8 // 3=National, 4=International
	NumberPlan uint8 // 1=ISDN/Telephony
	Digits     string
}

// CGEncountered represents call gap encountered
type CGEncountered uint8

const (
	CGNoCGEncountered        CGEncountered = 0
	CGManualCGEncountered    CGEncountered = 1
	CGAutomaticCGEncountered CGEncountered = 2
)

// BearerCapability represents bearer capability
type BearerCapability struct {
	BearerCap []byte
}

// LocationInformation represents location information
type LocationInformation struct {
	AgeOfLocationInformation         *int32
	GeographicalInformation          []byte
	VLRNumber                        *AddressString
	LocationNumber                   *AddressString
	CellGlobalIdOrServiceAreaIdOrLAI []byte
	ExtensionContainer               []byte
	LSAIdentity                      []byte
	MSCNumber                        *AddressString
	GeodeticInformation              []byte
	CurrentLocationRetrieved         bool
	SAIPresent                       bool
}

// EventSpecificInformationBCSM contains event-specific information
type EventSpecificInformationBCSM struct {
	RouteSelectFailureSpecificInfo *RouteSelectFailureSpecificInfo
	OCalledPartyBusySpecificInfo   *BusySpecificInfo
	ONoAnswerSpecificInfo          *NoAnswerSpecificInfo
	OAnswerSpecificInfo            *AnswerSpecificInfo
	OMidCallSpecificInfo           *MidCallSpecificInfo
	ODisconnectSpecificInfo        *DisconnectSpecificInfo
	TBusySpecificInfo              *BusySpecificInfo
	TNoAnswerSpecificInfo          *NoAnswerSpecificInfo
	TAnswerSpecificInfo            *AnswerSpecificInfo
	TMidCallSpecificInfo           *MidCallSpecificInfo
	TDisconnectSpecificInfo        *DisconnectSpecificInfo
}

type RouteSelectFailureSpecificInfo struct {
	FailureCause []byte
}

type BusySpecificInfo struct {
	BusyCause     []byte
	CallForwarded bool
}

type NoAnswerSpecificInfo struct {
	CallForwarded bool
}

type AnswerSpecificInfo struct {
	ForwardCallIndicators []byte
}

type MidCallSpecificInfo struct {
	MidCallEvents []byte
}

type DisconnectSpecificInfo struct {
	ReleaseCause []byte
}

// MiscCallInfo represents miscellaneous call information
type MiscCallInfo struct {
	MessageType uint8 // 0=request, 1=notification
}

// SendingSideID identifies the party to charge
type SendingSideID uint8

const (
	SendingSideIDLeg1 SendingSideID = 0
	SendingSideIDLeg2 SendingSideID = 1
)

// TimerID represents timer identifier
type TimerID uint8

const (
	TimerIDTSSF TimerID = 0
)

// RequestedInformationType represents requested information type
type RequestedInformationType uint8

const (
	RequestedInfoCallAttemptElapsedTime   RequestedInformationType = 0
	RequestedInfoCallStopTime             RequestedInformationType = 1
	RequestedInfoCallConnectedElapsedTime RequestedInformationType = 2
	RequestedInfoReleaseCause             RequestedInformationType = 30
)

// RequestedInformation contains requested information
type RequestedInformation struct {
	RequestedInformationType  RequestedInformationType
	RequestedInformationValue []byte
}

// InformationToSend represents information for announcement
type InformationToSend struct {
	InbandInfo *InbandInfo
	Tone       *Tone
}

// InbandInfo represents inband information
type InbandInfo struct {
	MessageID           *MessageID
	NumberOfRepetitions *int32
	Duration            *int32
	Interval            *int32
}

// MessageID represents announcement message
type MessageID struct {
	ElementaryMessageID *int32
	Text                *Text
	VariableParts       []byte
}

// Text represents text message
type Text struct {
	MessageContent string
	Attributes     []byte
}

// Tone represents tone
type Tone struct {
	ToneID   int32
	Duration *int32
}

// CollectedInfo represents collected digits
type CollectedInfo struct {
	CollectedDigits *CollectedDigits
}

// CollectedDigits represents digit collection parameters
type CollectedDigits struct {
	MinimumNbOfDigits   *int32
	MaximumNbOfDigits   int32
	EndOfReplyDigit     []byte
	CancelDigit         []byte
	StartDigit          []byte
	FirstDigitTimeOut   *int32
	InterDigitTimeOut   *int32
	ErrorTreatment      *ErrorTreatment
	InterruptableAnnInd bool
	VoiceInformation    bool
	VoiceBack           bool
}

// ErrorTreatment represents error treatment
type ErrorTreatment uint8

const (
	ErrorTreatmentStd          ErrorTreatment = 0
	ErrorTreatmentHelp         ErrorTreatment = 1
	ErrorTreatmentRepeatPrompt ErrorTreatment = 2
)

// Handler manages CAP protocol operations
type Handler struct {
	config *Config

	// tcapOutbound is the channel the dispatcher pushes outbound CAP Invokes
	// (Connect, Continue, …) onto for TCAP delivery. Installed via
	// SetTCAPOutbound after both CAP and TCAP handlers exist.
	tcapOutbound chan<- *tcap.Message
	// notSent is told of a dialogue whose message tcapOutbound could not take.
	notSent func(d *tcap.Dialogue, reason string)

	// Operation handlers
	initialDPHandler     InitialDPHandler
	eventReportHandler   EventReportBCSMHandler
	applyChargingHandler ApplyChargingHandler

	// Asynchronous error sink (currently unused — kept for future
	// background workers; consumers tolerate a never-written channel).
	errorCh chan error

	metrics *Metrics
	logger  *zap.Logger
}

// Operation handler interfaces
type InitialDPHandler interface {
	HandleInitialDP(req *InitialDPRequest) error
}

type EventReportBCSMHandler interface {
	HandleEventReportBCSM(req *EventReportBCSMRequest) error
}

type ApplyChargingHandler interface {
	HandleApplyCharging(req *ApplyChargingRequest) error
}

// Config holds CAP handler configuration
type Config struct {
	// gsmSCF configuration
	GSMSCFAddress     string `yaml:"gsmscf_address" json:"gsmscf_address"`
	GSMSCFGlobalTitle string `yaml:"gsmscf_gt" json:"gsmscf_gt"`
	GSMSCFPointCode   uint32 `yaml:"gsmscf_pc" json:"gsmscf_pc"`

	// Service Key mapping
	ServiceKeys map[uint32]string `yaml:"service_keys" json:"service_keys"`

	// Performance
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	OperationTimeout  time.Duration `yaml:"operation_timeout" json:"operation_timeout"`
}

// Metrics tracks CAP statistics
type Metrics struct {
	OperationsReceived atomic.Uint64
	OperationsSent     atomic.Uint64
	Errors             atomic.Uint64

	// Per-operation counters
	InitialDPCount       atomic.Uint64
	EventReportBCSMCount atomic.Uint64
	ConnectCount         atomic.Uint64
	ContinueCount        atomic.Uint64
	ReleaseCallCount     atomic.Uint64
	ApplyChargingCount   atomic.Uint64

	// Event counters
	AnswerEvents     atomic.Uint64
	DisconnectEvents atomic.Uint64
	BusyEvents       atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents handler health
type HealthStatus struct {
	OperationsReceived uint64    `json:"operations_received"`
	OperationsSent     uint64    `json:"operations_sent"`
	Errors             uint64    `json:"errors"`
	InitialDPCount     uint64    `json:"initial_dp_count"`
	EventReportCount   uint64    `json:"event_report_count"`
	ApplyChargingCount uint64    `json:"apply_charging_count"`
	LastUpdate         time.Time `json:"last_update"`
}
