// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// OperationCode represents MAP operation codes (3GPP TS 29.002)
type OperationCode int32

const (
	// Location Management (3GPP TS 29.002 v17)
	OpUpdateLocation     OperationCode = 2
	OpCancelLocation     OperationCode = 3
	OpSendIdentification OperationCode = 55
	OpUpdateGprsLocation OperationCode = 23
	OpPurgeMSRequest     OperationCode = 67

	// Subscriber Information
	OpProvideSubscriberInfo  OperationCode = 70
	OpAnyTimeInterrogation   OperationCode = 71
	OpSendRoutingInfoForSM   OperationCode = 45
	OpSendRoutingInfoForGPRS OperationCode = 24

	// Call Handling
	OpSendRoutingInfo      OperationCode = 22
	OpProvideRoamingNumber OperationCode = 4
	OpResumeCallHandling   OperationCode = 6

	// SMS
	OpMoForwardSM            OperationCode = 46
	OpMtForwardSM            OperationCode = 44
	OpReportSMDeliveryStatus OperationCode = 47
	OpAlertServiceCentre     OperationCode = 64
	OpInformServiceCentre    OperationCode = 63

	// Supplementary Services
	OpRegisterSS               OperationCode = 10
	OpEraseSS                  OperationCode = 11
	OpActivateSS               OperationCode = 12
	OpDeactivateSS             OperationCode = 13
	OpInterrogateSS            OperationCode = 14
	OpProcessUnstructuredSSReq OperationCode = 59
	OpUnstructuredSSRequest    OperationCode = 60
	OpUnstructuredSSNotify     OperationCode = 61
)

// ErrorCode represents MAP error codes (3GPP TS 29.002)
type ErrorCode int32

const (
	ErrUnknownSubscriber           ErrorCode = 1
	ErrUnknownMSC                  ErrorCode = 3
	ErrUnidentifiedSubscriber      ErrorCode = 5
	ErrAbsentSubscriberSM          ErrorCode = 6
	ErrUnknownEquipment            ErrorCode = 7
	ErrRoamingNotAllowed           ErrorCode = 8
	ErrIllegalSubscriber           ErrorCode = 9
	ErrBearerServiceNotProvisioned ErrorCode = 10
	ErrTeleserviceNotProvisioned   ErrorCode = 11
	ErrIllegalEquipment            ErrorCode = 12
	ErrCallBarred                  ErrorCode = 13
	ErrForwardingViolation         ErrorCode = 14
	ErrCUGReject                   ErrorCode = 15
	ErrIllegalSSOperation          ErrorCode = 16
	ErrSSErrorStatus               ErrorCode = 17
	ErrSSNotAvailable              ErrorCode = 18
	ErrSSSubscriptionViolation     ErrorCode = 19
	ErrSSIncompatibility           ErrorCode = 20
	ErrFacilityNotSupported        ErrorCode = 21
	ErrAbsentSubscriber            ErrorCode = 27
	ErrSubscriberBusyForMTSMS      ErrorCode = 31
	ErrSMDeliveryFailure           ErrorCode = 32
	ErrMessageWaitingListFull      ErrorCode = 33
	ErrSystemFailure               ErrorCode = 34
	ErrDataMissing                 ErrorCode = 35
	ErrUnexpectedDataValue         ErrorCode = 36
	ErrNumberChanged               ErrorCode = 44
	ErrBusySubscriber              ErrorCode = 45
	ErrNoSubscriberReply           ErrorCode = 46
	ErrORNotAllowed                ErrorCode = 48
	ErrATINotAllowed               ErrorCode = 49
	ErrResourceLimitation          ErrorCode = 51
	ErrUnknownAlphabet             ErrorCode = 71
	ErrUSSDBusy                    ErrorCode = 72
)

// errorCodeNames are the ASN.1 value names of 3GPP TS 29.002 MAP-ErrorCodes.
var errorCodeNames = map[ErrorCode]string{
	ErrUnknownSubscriber:           "unknownSubscriber",
	ErrUnknownMSC:                  "unknownMSC",
	ErrUnidentifiedSubscriber:      "unidentifiedSubscriber",
	ErrAbsentSubscriberSM:          "absentSubscriberSM",
	ErrUnknownEquipment:            "unknownEquipment",
	ErrRoamingNotAllowed:           "roamingNotAllowed",
	ErrIllegalSubscriber:           "illegalSubscriber",
	ErrBearerServiceNotProvisioned: "bearerServiceNotProvisioned",
	ErrTeleserviceNotProvisioned:   "teleserviceNotProvisioned",
	ErrIllegalEquipment:            "illegalEquipment",
	ErrCallBarred:                  "callBarred",
	ErrForwardingViolation:         "forwardingViolation",
	ErrCUGReject:                   "cug-Reject",
	ErrIllegalSSOperation:          "illegalSS-Operation",
	ErrSSErrorStatus:               "ss-ErrorStatus",
	ErrSSNotAvailable:              "ss-NotAvailable",
	ErrSSSubscriptionViolation:     "ss-SubscriptionViolation",
	ErrSSIncompatibility:           "ss-Incompatibility",
	ErrFacilityNotSupported:        "facilityNotSupported",
	ErrAbsentSubscriber:            "absentSubscriber",
	ErrSubscriberBusyForMTSMS:      "subscriberBusyForMT-SMS",
	ErrSMDeliveryFailure:           "sm-DeliveryFailure",
	ErrMessageWaitingListFull:      "messageWaitingListFull",
	ErrSystemFailure:               "systemFailure",
	ErrDataMissing:                 "dataMissing",
	ErrUnexpectedDataValue:         "unexpectedDataValue",
	ErrNumberChanged:               "numberChanged",
	ErrBusySubscriber:              "busySubscriber",
	ErrNoSubscriberReply:           "noSubscriberReply",
	ErrORNotAllowed:                "or-NotAllowed",
	ErrATINotAllowed:               "ati-NotAllowed",
	ErrResourceLimitation:          "resourceLimitation",
	ErrUnknownAlphabet:             "unknownAlphabet",
	ErrUSSDBusy:                    "ussd-Busy",
}

// String returns the MAP-ErrorCodes value name, or "mapError(<n>)" for a code
// this gateway does not name.
func (e ErrorCode) String() string {
	if n, ok := errorCodeNames[e]; ok {
		return n
	}
	return fmt.Sprintf("mapError(%d)", int32(e))
}

// ErrorSink receives the peer's answer to an operation this gateway
// originated when that answer is not a result: a MAP ReturnError (code is the
// MAP error code) or a TCAP Reject (code is 0). detail is human-readable.
type ErrorSink func(natsCorrelationID string, op OperationCode, code ErrorCode, detail string)

// IMSI represents International Mobile Subscriber Identity
type IMSI string

// MSISDN represents Mobile Station ISDN Number
type MSISDN string

// IMEI represents International Mobile Equipment Identity
type IMEI string

// AddressString is TS 29.002 AddressString: nature of address, numbering
// plan and digits.
type AddressString struct {
	Nature     uint8 // NatureInternational, …
	NumberPlan uint8 // NumberingPlanISDN, …
	Digits     string
}

// Nature-of-address indicators of an AddressString, bits 765 of its first
// octet (TS 29.002 AddressString). Not the SCCP global title's nature of
// address (Q.713), where international is 4.
const (
	NatureUnknown         uint8 = 0
	NatureInternational   uint8 = 1
	NatureNational        uint8 = 2
	NatureNetworkSpecific uint8 = 3
	NatureSubscriber      uint8 = 4
)

// NumberingPlanISDN is ISDN/Telephony (ITU-T E.164), bits 4321 = 0001.
const NumberingPlanISDN uint8 = 1

// NumberingPlanLandMobile is the land mobile numbering plan (ITU-T E.212),
// bits 4321 = 0110: an IMSI as an AddressString, e.g. a MAP-OpenInfo
// destination reference.
const NumberingPlanLandMobile uint8 = 6

// UpdateLocationRequest (3GPP TS 29.002 - MAP-MS-DataTypes)
type UpdateLocationRequest struct {
	IMSI                         IMSI
	MSCNumber                    *AddressString // HLR->MSC
	VLRNumber                    *AddressString // Current VLR
	LMSI                         []byte         // Local MSI
	VLRCapability                *VLRCapability
	InformPreviousNetworkEntity  bool
	CSGSubscriptionDataRequested bool
	SkipSubscriberDataUpdate     bool
	ExtensionContainer           []byte
}

// UpdateLocationResponse
type UpdateLocationResponse struct {
	HLRNumber          *AddressString
	ExtensionContainer []byte
}

// VLRCapability represents VLR capabilities
type VLRCapability struct {
	SupportedCamelPhases  []byte
	ExtensionContainer    []byte
	SolsaSupportIndicator bool
	ISTSupportIndicator   bool
	SuperChargerSupported bool
	LongFTNSupported      bool
}

// CancelLocationRequest (3GPP TS 29.002)
type CancelLocationRequest struct {
	IMSI                          IMSI
	IMSIWithLMSI                  *IMSIWithLMSI
	CancellationType              CancellationType
	ExtensionContainer            []byte
	TypeOfUpdate                  *TypeOfUpdate
	MTRFSupportedAndAuthorized    bool
	MTRFSupportedAndNotAuthorized bool
	NewMSCNumber                  *AddressString
	NewVLRNumber                  *AddressString
}

// CancelLocationResponse
type CancelLocationResponse struct {
	ExtensionContainer []byte
}

// CancellationType
type CancellationType uint8

const (
	CancelTypeUpdateProcedure      CancellationType = 0
	CancelTypeSubscriptionWithdraw CancellationType = 1
)

// TypeOfUpdate
type TypeOfUpdate uint8

const (
	UpdateTypeSGSNAttach TypeOfUpdate = 0
	UpdateTypeMMEAttach  TypeOfUpdate = 1
)

// IMSIWithLMSI
type IMSIWithLMSI struct {
	IMSI IMSI
	LMSI []byte
}

// SMS types (3GPP TS 29.002 v17 §17.7.13 / §17.7.5 / §17.7.10).
//
// SM-RP-DA / SM-RP-OA are CHOICEs on the wire. The Go side carries one
// non-nil variant; the codec validates exactly-one-of when encoding.

// SMRPDA — destination address of an SMS-relay-protocol message.
type SMRPDA struct {
	IMSI                 *IMSI          // [0] IMSI
	LMSI                 []byte         // [1] LMSI
	ServiceCentreAddress *AddressString // [4] AddressString (v1 only)
	NoSMRPDA             bool           // [5] NULL
}

// SMRPOA — originating address of an SMS-relay-protocol message.
type SMRPOA struct {
	MSISDN               *MSISDN        // [2] ISDN-AddressString
	ServiceCentreAddress *AddressString // [4] AddressString
	NoSMRPOA             bool           // [5] NULL
}

// LocationInfoWithLMSI — VLR/MSC routing info returned by SRI-for-SM.
type LocationInfoWithLMSI struct {
	NetworkNodeNumber *AddressString // [1] ISDN-AddressString — VMSC for MT delivery
	LMSI              []byte
	GPRSNodeIndicator bool // [5] NULL — SGSN vs MSC
	AdditionalNumber  *AddressString
}

// SendRoutingInfoForSMRequest (3GPP TS 29.002 §17.7.13).
type SendRoutingInfoForSMRequest struct {
	MSISDN               MSISDN
	SMRPPRI              bool          // [1] BOOLEAN — "priority"
	ServiceCentreAddress AddressString // [2] AddressString
	GPRSSupportIndicator bool          // [7] NULL OPTIONAL
	SMRPMTI              *uint8        // [8] SM-RP-MTI OPTIONAL
	SMRPSMEA             []byte        // [9] OCTET STRING OPTIONAL
	ExtensionContainer   []byte
}

// SendRoutingInfoForSMResponse (3GPP TS 29.002 §17.7.13).
type SendRoutingInfoForSMResponse struct {
	IMSI                 IMSI
	LocationInfoWithLMSI LocationInfoWithLMSI // [0]
	ExtensionContainer   []byte               // [4]
}

// MtForwardSMRequest (3GPP TS 29.002 §17.7.4).
type MtForwardSMRequest struct {
	SMRPDA             SMRPDA
	SMRPOA             SMRPOA
	SMRPUI             []byte // SignalInfo — TPDU bytes (TS 23.040)
	MoreMessagesToSend bool   // NULL OPTIONAL
	ExtensionContainer []byte
	SMDeliveryTimer    *int32 // OPTIONAL
}

// MtForwardSMResponse (3GPP TS 29.002 §17.7.4).
type MtForwardSMResponse struct {
	SMRPUI             []byte // SignalInfo OPTIONAL — RP-ACK / RP-ERROR TPDU
	ExtensionContainer []byte
}

// MoForwardSMRequest (3GPP TS 29.002 §17.7.5). Wire-shape identical to
// MtForwardSMRequest minus moreMessagesToSend; we share the carry types
// for symmetry with the response.
type MoForwardSMRequest struct {
	SMRPDA             SMRPDA
	SMRPOA             SMRPOA
	SMRPUI             []byte
	ExtensionContainer []byte
	IMSI               *IMSI // OPTIONAL — IMSI of the originator (post-Rel-99)
}

// MoForwardSMResponse (3GPP TS 29.002 §17.7.5).
type MoForwardSMResponse struct {
	SMRPUI             []byte
	ExtensionContainer []byte
}

// ProvideRoamingNumberRequest (3GPP TS 29.002)
type ProvideRoamingNumberRequest struct {
	IMSI                                    IMSI
	MSCNumber                               *AddressString
	MSISDN                                  *MSISDN
	LMSI                                    []byte
	GSMBearerCapability                     []byte
	NetworkSignalInfo                       []byte
	SuppressMTSS                            bool
	MTRoamingForwardingSupported            bool
	CallReferenceNumber                     []byte
	ExtensionContainer                      []byte
	AlertingPattern                         []byte
	CCBSCall                                bool
	SupportedCamelPhasesInInterrogatingNode []byte
	AdditionalSignalInfo                    []byte
	ORInterrogation                         bool
	ORCapability                            *int32
	ISTSupportIndicator                     bool
	PrePagingSupported                      bool
	LongFTNSupported                        bool
	SuppressVTCSI                           bool
	OfferedCamel4CSIs                       []byte
	MTRoamingRetrySupported                 bool
	PagingArea                              []byte
	EPS_Info                                []byte
	CSG_ID                                  []byte
}

// ProvideRoamingNumberResponse
type ProvideRoamingNumberResponse struct {
	RoamingNumber             AddressString
	ExtensionContainer        []byte
	ReleaseResourcesSupported bool
	VMSCAddress               *AddressString
}

// Handler manages MAP protocol operations
type Handler struct {
	config *Config

	// tcapOutbound is the channel the dispatcher pushes encoded
	// ReturnResult / ReturnError responses onto for TCAP delivery.
	// Installed via SetTCAPOutbound after both handlers are constructed.
	tcapOutbound chan<- *tcap.Message

	// responseSink delivers decoded inbound ReturnResult components to the
	// composition root (cmd/gateway), which republishes them on NATS using
	// the dialogue's NATSCorrelationID. Installed via SetResponseSink.
	// Op-specific response types passed as `any`; sink type-switches.
	responseSink func(natsCorrelationID string, op OperationCode, resp any) error

	// errorSink delivers inbound ReturnError / Reject components on
	// originated dialogues, so the NATS caller learns the outcome instead of
	// waiting out its deadline. Installed via SetErrorSink.
	errorSink ErrorSink

	// ussd serves USSD dialogues (ussd_session.go); nil: they are refused
	// like any other context without a handler.
	ussd USSDSessionHandler

	// Operation handlers
	updateLocationHandler       UpdateLocationHandler
	provideRoamingNumHandler    ProvideRoamingNumberHandler
	cancelLocationHandler       CancelLocationHandler
	sendRoutingInfoForSMHandler SendRoutingInfoForSMHandler
	mtForwardSMHandler          MtForwardSMHandler
	moForwardSMHandler          MoForwardSMHandler

	// Asynchronous error sink (currently unused — kept for future
	// background workers; consumers tolerate a never-written channel).
	errorCh chan error

	metrics *Metrics
	logger  *zap.Logger
}

// Operation handler interfaces
type UpdateLocationHandler interface {
	HandleUpdateLocation(req *UpdateLocationRequest) (*UpdateLocationResponse, *ErrorCode, error)
}

type ProvideRoamingNumberHandler interface {
	HandleProvideRoamingNumber(req *ProvideRoamingNumberRequest) (*ProvideRoamingNumberResponse, *ErrorCode, error)
}

type CancelLocationHandler interface {
	HandleCancelLocation(req *CancelLocationRequest) (*CancelLocationResponse, *ErrorCode, error)
}

// SendRoutingInfoForSMHandler — SRI-for-SM (op 45) inbound. SMSC asks the
// gateway to resolve MSISDN -> {IMSI, VMSC/SGSN address}. The application
// (typically a NATS bridge to a real SMSC stack) returns the routing answer.
type SendRoutingInfoForSMHandler interface {
	HandleSendRoutingInfoForSM(req *SendRoutingInfoForSMRequest) (*SendRoutingInfoForSMResponse, *ErrorCode, error)
}

// MtForwardSMHandler — MT-Fwd-SM (op 44) inbound. Foreign SMSC asks our
// MSC/gateway to deliver an SMS to a roamer. The application acks with the
// MT-Forward result.
type MtForwardSMHandler interface {
	HandleMtForwardSM(req *MtForwardSMRequest) (*MtForwardSMResponse, *ErrorCode, error)
}

// MoForwardSMHandler — MO-Fwd-SM (op 46) inbound. Visited MSC forwards an
// originating SMS to its home SMSC via the gateway.
type MoForwardSMHandler interface {
	HandleMoForwardSM(req *MoForwardSMRequest) (*MoForwardSMResponse, *ErrorCode, error)
}

// Config holds MAP handler configuration
type Config struct {
	// SRIRouting selects how SendRoutingInfoForSM is addressed when the
	// request names no HLR:
	//   - SRIRoutingMSISDN (default): the called party address is the
	//     subscriber's MSISDN as a global title with SSN 6, routed on GT, so
	//     the STP's translation finds the subscriber's HLR (number ranges,
	//     number portability) — how an SMS-GMSC addresses it;
	//   - SRIRoutingHLR: the configured HLR (HLRGlobalTitle / HLRPointCode),
	//     for a network with a single HLR reachable directly.
	SRIRouting string `yaml:"sri_routing" json:"sri_routing"`

	// HLR configuration, used with SRIRoutingHLR.
	HLRAddress     string `yaml:"hlr_address" json:"hlr_address"` // informational (logs) only
	HLRGlobalTitle string `yaml:"hlr_gt" json:"hlr_gt"`
	HLRPointCode   uint32 `yaml:"hlr_pc" json:"hlr_pc"`

	// GSMSCFAddress is the gsmSCF-Address (E.164 digits) AnyTimeInterrogation
	// carries for the HLR to screen, unless the request names one. Empty:
	// the cluster GT, else the node's own GT.
	GSMSCFAddress string `yaml:"gsmscf_address" json:"gsmscf_address"`

	// USSDSessionTimeout is the longest a USSD session waits — for the
	// subscriber's answer, or for the application's next command — before
	// the gateway ends it: the timer ml of unstructuredSS-Request and -Notify
	// (TS 29.002 §17.1.2: 1 to 10 minutes). Default 180 s.
	USSDSessionTimeout time.Duration `yaml:"ussd_session_timeout" json:"ussd_session_timeout"`
	// USSDRoutes send subscriber-initiated sessions to routes by what the
	// subscriber dialled: the longest matching prefix wins; no match goes to
	// the "default" route.
	USSDRoutes []USSDRoute `yaml:"ussd_routes" json:"ussd_routes"`

	// Performance
	ChannelBufferSize int           `yaml:"channel_buffer_size" json:"channel_buffer_size"`
	WorkerPoolSize    int           `yaml:"worker_pool_size" json:"worker_pool_size"`
	OperationTimeout  time.Duration `yaml:"operation_timeout" json:"operation_timeout"`
}

// USSDRoute is one entry of Config.USSDRoutes.
type USSDRoute struct {
	// Prefix of the dialled string, e.g. "*123".
	Prefix string `yaml:"prefix" json:"prefix" mapstructure:"prefix"`
	// Route names the event subject map.ussd.event.<route>.
	Route string `yaml:"route" json:"route" mapstructure:"route"`
}

// USSD session timeout bounds and default (Config.USSDSessionTimeout).
const (
	MinUSSDSessionTimeout     = time.Minute
	MaxUSSDSessionTimeout     = 10 * time.Minute
	DefaultUSSDSessionTimeout = 180 * time.Second
)

// SRIRouting values; see Config.SRIRouting.
const (
	SRIRoutingMSISDN = "msisdn"
	SRIRoutingHLR    = "hlr"
)

// Metrics tracks MAP statistics
type Metrics struct {
	OperationsReceived atomic.Uint64
	OperationsSent     atomic.Uint64
	Errors             atomic.Uint64

	// Per-operation counters
	UpdateLocationCount       atomic.Uint64
	CancelLocationCount       atomic.Uint64
	SendRoutingInfoCount      atomic.Uint64
	ProvideRoamingNumberCount atomic.Uint64
	AnyTimeInterrogationCount atomic.Uint64
	SendRoutingInfoForSMCount atomic.Uint64
	MtForwardSMCount          atomic.Uint64
	MoForwardSMCount          atomic.Uint64

	// Error counters
	UnknownSubscriberErrors atomic.Uint64
	RoamingNotAllowedErrors atomic.Uint64
	SystemFailureErrors     atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthStatus represents handler health
type HealthStatus struct {
	OperationsReceived   uint64    `json:"operations_received"`
	OperationsSent       uint64    `json:"operations_sent"`
	Errors               uint64    `json:"errors"`
	UpdateLocationCount  uint64    `json:"update_location_count"`
	SendRoutingInfoCount uint64    `json:"send_routing_info_count"`
	LastUpdate           time.Time `json:"last_update"`
}
