// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package inap

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// OperationCode represents INAP operation codes (ITU-T Q.1218)
type OperationCode int32

const (
	// Call Control
	OpInitialDP                    OperationCode = 0
	OpOriginationAttemptAuthorized OperationCode = 1
	OpCollectedInformation         OperationCode = 2
	OpAnalysedInformation          OperationCode = 3
	OpRouteSelectFailure           OperationCode = 4
	OpOCalledPartyBusy             OperationCode = 5
	OpONoAnswer                    OperationCode = 6
	OpOAnswer                      OperationCode = 7
	OpODisconnect                  OperationCode = 9
	OpTermAttemptAuthorized        OperationCode = 12
	OpTBusy                        OperationCode = 13
	OpTNoAnswer                    OperationCode = 14
	OpTAnswer                      OperationCode = 15
	OpTDisconnect                  OperationCode = 17

	// Call Management
	OpConnect                          OperationCode = 20
	OpConnectToResource                OperationCode = 21
	OpReleaseCall                      OperationCode = 22
	OpRequestReportBCSMEvent           OperationCode = 23
	OpEventReportBCSM                  OperationCode = 24
	OpContinue                         OperationCode = 31
	OpRequestNotificationChargingEvent OperationCode = 32
	OpEventNotificationCharging        OperationCode = 33
	OpResetTimer                       OperationCode = 34

	// Charging
	OpApplyCharging                  OperationCode = 35
	OpApplyChargingReport            OperationCode = 36
	OpRequestCurrentStatusReport     OperationCode = 37
	OpRequestEveryStatusChangeReport OperationCode = 38
	OpRequestFirstStatusMatchReport  OperationCode = 39
	OpStatusReport                   OperationCode = 40

	// Service Management
	OpCallGap                         OperationCode = 41
	OpCallInformationReport           OperationCode = 44
	OpCallInformationRequest          OperationCode = 45
	OpSendChargingInformation         OperationCode = 46
	OpPlayAnnouncement                OperationCode = 47
	OpPromptAndCollectUserInformation OperationCode = 48
	OpSpecializedResourceReport       OperationCode = 49
	OpCancel                          OperationCode = 53
	OpActivityTest                    OperationCode = 54

	// CS-2 Operations (Advanced Features)
	OpCollectInformation         OperationCode = 55
	OpCallFiltering              OperationCode = 56
	OpManageTriggerData          OperationCode = 57
	OpRequestReportUTSI          OperationCode = 58
	OpSendSTUI                   OperationCode = 59
	OpReportUTSI                 OperationCode = 60
	OpSendFacilityInformation    OperationCode = 61
	OpRequestReportFacilityEvent OperationCode = 62
	OpEventReportFacility        OperationCode = 63
	OpPromptAndReceiveMessage    OperationCode = 64
	OpScriptClose                OperationCode = 65
	OpScriptEvent                OperationCode = 66
	OpScriptInformation          OperationCode = 67
	OpScriptRun                  OperationCode = 68
)

// ErrorCode represents INAP error codes (ITU-T Q.1218)
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
	ErrUnknownResource             ErrorCode = 18
	ErrChainingRefused             ErrorCode = 21
	ErrScfReferralDenied           ErrorCode = 22
	ErrScfTaskRefused              ErrorCode = 23
)

// EventTypeBCSM represents BCSM event types (ITU-T Q.1218)
type EventTypeBCSM uint8

const (
	EventOrigAttemptAuthorized EventTypeBCSM = 1
	EventCollectedInfo         EventTypeBCSM = 2
	EventAnalyzedInfo          EventTypeBCSM = 3
	EventRouteSelectFailure    EventTypeBCSM = 4
	EventOCalledPartyBusy      EventTypeBCSM = 5
	EventONoAnswer             EventTypeBCSM = 6
	EventOAnswer               EventTypeBCSM = 7
	EventODisconnect           EventTypeBCSM = 9
	EventOAbandon              EventTypeBCSM = 10
	EventTermAttemptAuthorized EventTypeBCSM = 12
	EventTBusy                 EventTypeBCSM = 13
	EventTNoAnswer             EventTypeBCSM = 14
	EventTAnswer               EventTypeBCSM = 15
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

// ServiceKey represents INAP service key
type ServiceKey uint32

// Common service keys for IN services
const (
	ServiceKeyTollFree              ServiceKey = 800 // 1-800 numbers
	ServiceKeyPremiumRate           ServiceKey = 900 // 1-900 numbers
	ServiceKeyVirtualPrivateNetwork ServiceKey = 700 // VPN services
	ServiceKeyUniversalAccess       ServiceKey = 888 // Universal access
	ServiceKeyPersonalNumber        ServiceKey = 500 // Personal numbering
	ServiceKeyFreephone             ServiceKey = 808 // Freephone
	ServiceKeySharedCost            ServiceKey = 844 // Shared cost services
)

// InitialDPRequest represents InitialDP operation (ITU-T Q.1218)
type InitialDPRequest struct {
	ServiceKey                   ServiceKey
	CalledPartyNumber            *Digits
	CallingPartyNumber           *Digits
	CallingPartysCategory        *uint8
	CGEncountered                *CGEncountered
	IPSSPCapabilities            []byte
	LocationNumber               *Digits
	OriginalCalledPartyID        *Digits
	Extensions                   []byte
	HighLayerCompatibility       []byte
	AdditionalCallingPartyNumber *Digits
	BearerCapability             *BearerCapability
	EventTypeBCSM                *EventTypeBCSM
	RedirectingPartyID           *Digits
	RedirectionInformation       []byte
	Cause                        []byte
	ServiceInteractionIndicators []byte
	Carrier                      []byte
	CUGIndex                     *int32
	CUGInterlock                 []byte
	ForwardCallIndicators        []byte
	CalledPartyBCDNumber         []byte
	ServiceProfileIdentifier     *ServiceProfileIdentifier
	TerminalType                 *TerminalType
	ChargeNumber                 *Digits
	ServingAreaID                []byte
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
	DestinationRoutingAddress        *Digits
	AlertingPattern                  []byte
	CorrelationID                    []byte
	CutAndPaste                      *CutAndPaste
	ForwardingCondition              *ForwardingCondition
	RouteList                        []*RouteItem
	CallingPartyNumber               *Digits
	CallingPartysCategory            *uint8
	RedirectingPartyID               *Digits
	RedirectionInformation           []byte
	OriginalCalledPartyID            *Digits
	Extensions                       []byte
	Carrier                          []byte
	ServiceInteractionIndicators     []byte
	INServiceCompatibilityIndication []byte
	GenericNumbers                   []byte
	ServiceInteractionIndicatorsTwo  []byte
	ForwardGVNS                      []byte
	CreatedCallSegmentAssociation    *int32
	IPAvailable                      []byte
	IPSSPCapabilities                []byte
}

// ContinueRequest represents Continue operation
type ContinueRequest struct {
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
	SendCalculationToSCPIndication    bool
	PartyToCharge                     *LegType
	Extensions                        []byte
}

// ApplyChargingReportRequest represents ApplyChargingReport operation
type ApplyChargingReportRequest struct {
	CallResult []byte
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

// PlayAnnouncementRequest represents PlayAnnouncement operation
type PlayAnnouncementRequest struct {
	InformationToSend           *InformationToSend
	DisconnectFromIPForbidden   bool
	RequestAnnouncementComplete bool
	Extensions                  []byte
	ConnectedParty              *LegType
}

// PromptAndCollectUserInformationRequest represents PromptAndCollectUserInformation
type PromptAndCollectUserInformationRequest struct {
	CollectedInfo             *CollectedInfo
	DisconnectFromIPForbidden bool
	InformationToSend         *InformationToSend
	Extensions                []byte
}

// CallGapRequest represents CallGap operation (traffic management)
type CallGapRequest struct {
	GapCriteria   *GapCriteria
	GapIndicators *GapIndicators
	ControlType   *ControlType
	GapTreatment  *GapTreatment
	Extensions    []byte
}

// Supporting types

// Digits represents digit string (phone numbers)
type Digits struct {
	EncodingScheme uint8 // 0=BCD even, 1=BCD odd, 2=IA5
	TypeOfDigits   uint8 // 0-7 (see ITU-T Q.763)
	Digits         string
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

// ServiceProfileIdentifier identifies service profile
type ServiceProfileIdentifier struct {
	TriggerID         *int32
	AddressAndService *AddressAndService
}

// AddressAndService contains address and service info
type AddressAndService struct {
	CalledAddressValue []byte
	ServiceKey         ServiceKey
}

// TerminalType represents terminal type
type TerminalType uint8

const (
	TerminalTypeUnknown TerminalType = 0
	TerminalTypePOTS    TerminalType = 1
	TerminalTypeISDN    TerminalType = 2
	TerminalTypeAnalog  TerminalType = 3
	TerminalTypeDigital TerminalType = 4
)

// EventSpecificInformationBCSM contains event-specific information
type EventSpecificInformationBCSM struct {
	CollectedInfoSpecificInfo      *CollectedInfoSpecificInfo
	AnalyzedInfoSpecificInfo       *AnalyzedInfoSpecificInfo
	RouteSelectFailureSpecificInfo *RouteSelectFailureSpecificInfo
	OCalledPartyBusySpecificInfo   *BusySpecificInfo
	ONoAnswerSpecificInfo          *NoAnswerSpecificInfo
	OAnswerSpecificInfo            *AnswerSpecificInfo
	ODisconnectSpecificInfo        *DisconnectSpecificInfo
	TBusySpecificInfo              *BusySpecificInfo
	TNoAnswerSpecificInfo          *NoAnswerSpecificInfo
	TAnswerSpecificInfo            *AnswerSpecificInfo
	TDisconnectSpecificInfo        *DisconnectSpecificInfo
}

type CollectedInfoSpecificInfo struct {
	CalledPartynumber []byte
}

type AnalyzedInfoSpecificInfo struct {
	CalledPartynumber []byte
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

type DisconnectSpecificInfo struct {
	ReleaseCause []byte
}

// MiscCallInfo represents miscellaneous call information
type MiscCallInfo struct {
	MessageType  uint8 // 0=request, 1=notification
	DpAssignment *DpAssignment
}

// DpAssignment represents detection point assignment
type DpAssignment uint8

const (
	DpAssignmentIndividualLine DpAssignment = 0
	DpAssignmentGroupBased     DpAssignment = 1
	DpAssignmentOfficeBased    DpAssignment = 2
)

// CutAndPaste for Connect operation
type CutAndPaste struct {
	ForwardingCondition *ForwardingCondition
}

// ForwardingCondition
type ForwardingCondition uint8

const (
	ForwardingConditionBusy          ForwardingCondition = 0
	ForwardingConditionNoReply       ForwardingCondition = 1
	ForwardingConditionUnconditional ForwardingCondition = 2
)

// RouteItem represents a route
type RouteItem struct {
	RouteNumber *Digits
	CauseValue  []byte
}

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

// GapCriteria for CallGap operation
type GapCriteria struct {
	CalledAddressValue  []byte
	CallingAddressValue []byte
	ServiceKey          *ServiceKey
	GapOnService        []byte
}

// GapIndicators
type GapIndicators struct {
	Duration    int32 // seconds
	GapInterval int32 // seconds
}

// ControlType
type ControlType uint8

const (
	ControlTypeNoControl          ControlType = 0
	ControlTypeAnnouncement       ControlType = 1
	ControlTypeDestinationRouting ControlType = 2
)

// GapTreatment
type GapTreatment struct {
	InformationToSend *InformationToSend
	ReleaseCause      []byte
}

// Handler manages INAP protocol operations
type Handler struct {
	config *Config

	// TCAP integration
	tcapIncomingCh <-chan interface{} // From TCAP
	tcapOutgoingCh chan<- interface{} // To TCAP

	// Operation handlers
	initialDPHandler     InitialDPHandler
	eventReportHandler   EventReportBCSMHandler
	applyChargingHandler ApplyChargingHandler
	callGapHandler       CallGapHandler

	// Control channels
	errorCh    chan error
	shutdownCh chan struct{}

	// Metrics
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

type CallGapHandler interface {
	HandleCallGap(req *CallGapRequest) error
}

// Config holds INAP handler configuration
type Config struct {
	// SCP configuration
	SCPAddress     string `yaml:"scp_address" json:"scp_address"`
	SCPGlobalTitle string `yaml:"scp_gt" json:"scp_gt"`
	SCPPointCode   uint32 `yaml:"scp_pc" json:"scp_pc"`

	// Service Key mapping
	ServiceKeys map[ServiceKey]string `yaml:"service_keys" json:"service_keys"`

	// Performance
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	OperationTimeout  time.Duration `yaml:"operation_timeout" json:"operation_timeout"`
}

// Metrics tracks INAP statistics
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
	CallGapCount         atomic.Uint64

	// Service type counters
	TollFreeCallsCount    atomic.Uint64
	PremiumRateCallsCount atomic.Uint64
	VPNCallsCount         atomic.Uint64

	// Event counters
	AnswerEvents     atomic.Uint64
	DisconnectEvents atomic.Uint64
	BusyEvents       atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents handler health
type HealthStatus struct {
	OperationsReceived    uint64    `json:"operations_received"`
	OperationsSent        uint64    `json:"operations_sent"`
	Errors                uint64    `json:"errors"`
	InitialDPCount        uint64    `json:"initial_dp_count"`
	EventReportCount      uint64    `json:"event_report_count"`
	TollFreeCallsCount    uint64    `json:"toll_free_calls_count"`
	PremiumRateCallsCount uint64    `json:"premium_rate_calls_count"`
	LastUpdate            time.Time `json:"last_update"`
}
