// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/base64"
	"errors"
	"fmt"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// NATS bridge wiring. Composition root only — translates between
// `*natsbridge.NatsMessage` (transport DTO) and `mapprotocol`/`tcap`
// typed calls. No protocol logic lives here; this file is the thin
// adapter that lets external SMSC / SCP / OCS systems drive the SS7
// stack via NATS.
//
// Addressing model:
//   - Called address (peer) = built from NatsMessage.GlobalTitle if
//     present, else falls back to the MAP config's HLR GT — appropriate
//     for SRI-for-SM toward the home HLR. MtForwardSM toward a roamer's
//     VMSC requires the caller to supply NetworkNodeNumber via
//     Parameters["network_node_number"], otherwise we reject the
//     request (we do not pre-resolve VMSC).
//   - Calling address (us) = local point code + SSN_MAP, routing on
//     SSN. Operators that need GT-based source addressing should set
//     gateway own GT in a future config field (`gateway.own_gt`) and
//     update buildOwnSCCPAddress accordingly.

// Parameter keys are part of the wire contract owned by ss7client.
const (
	paramServiceCentreAddress = ss7client.ParamServiceCentreAddress
	paramNetworkNodeNumber    = ss7client.ParamNetworkNodeNumber
	paramTPDUBase64           = ss7client.ParamTPDUBase64
)

// bindNATSBridge installs callbacks on natsHandler and a response sink on
// mapHandler so inbound NATS requests originate SS7 dialogues and inbound
// SS7 responses flow back to NATS. Must be called after MAP, TCAP, and
// NATS handlers are constructed and started.
func bindNATSBridge(handlers *Handlers, mapCfg *mapprotocol.Config, sccpCfg *sccp.Config, logger *zap.Logger) {
	bridge := &natsBridge{
		mapHandler:  handlers.MAP,
		tcapHandler: handlers.TCAP,
		natsHandler: handlers.NATS,
		mapCfg:      mapCfg,
		sccpCfg:     sccpCfg,
		logger:      logger.Named("nats-bridge"),
	}

	handlers.NATS.OnSRIRequest(bridge.onSRIRequest)
	handlers.NATS.OnMTSmsRequest(bridge.onMTSmsRequest)
	handlers.NATS.OnATIRequest(bridge.onATIRequest)
	handlers.NATS.OnPSIRequest(bridge.onPSIRequest)
	handlers.NATS.OnRoutingRequest(bridge.onRoutingRequest)
	handlers.NATS.OnCAPInitialDP(bridge.onCAPInitialDP)
	handlers.NATS.OnINAPInitialDP(bridge.onINAPInitialDP)

	handlers.MAP.SetResponseSink(bridge.publishMAPResponse)
	handlers.MAP.SetErrorSink(bridge.publishMAPError)
	handlers.TCAP.SetFailureSink(bridge.publishMAPFailure)

	// MO-Forward-SM (op 46) inbound: foreign MSC → our gateway. Bridge
	// implements `mapprotocol.MoForwardSMHandler`, publishing to NATS
	// (subject `map.mo.sms`) and returning empty MoForwardSMResponse to
	// the foreign MSC per TS 29.002 §17.7.4 (success = empty SEQUENCE,
	// SMSC takes ownership of delivery from this point).
	handlers.MAP.SetMoForwardSMHandler(bridge)

	logger.Info("NATS bridge bound to MAP/TCAP",
		zap.Strings("subjects", []string{
			natsbridge.SubjectMTSmsRequest,
			natsbridge.SubjectSRIRequest,
			natsbridge.SubjectATIRequest,
			natsbridge.SubjectPSIRequest,
			natsbridge.SubjectRoutingRequest,
			natsbridge.SubjectCAPInitialDP,
			natsbridge.SubjectINAPInitialDP,
		}))
}

// tcapOriginator is the subset of *tcap.Handler the bridge consumes.
// Defined here (consumer side) so tests can inject a minimal recording
// mock without spinning up a real tcap.Handler with SCCP/M3UA chains.
type tcapOriginator interface {
	Originate(
		ac *tcap.ApplicationContext,
		called, calling *sccp.SccpAddress,
		opCode int32,
		parameter []byte,
		natsCorrelationID string,
	) (*tcap.Dialogue, error)
}

type natsBridge struct {
	mapHandler  *mapprotocol.Handler
	tcapHandler tcapOriginator
	natsHandler *natsbridge.Handler
	mapCfg      *mapprotocol.Config
	sccpCfg     *sccp.Config
	logger      *zap.Logger
}

// onSRIRequest: SMSC asks "where is MSISDN X registered?" → MAP SRI-for-SM.
func (b *natsBridge) onSRIRequest(msg *natsbridge.NatsMessage) error {
	if msg.MSISDN == "" {
		return errors.New("SRI request: msisdn required")
	}
	sca := paramAddressString(msg, paramServiceCentreAddress)
	if sca == nil {
		// Fall back to a SCA derived from the gateway's own GT — operators
		// running the gateway co-located with an SMSC sometimes accept this.
		sca = &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: msg.Sender}
	}
	if sca.Digits == "" {
		return errors.New("SRI request: service_centre_address required (Parameters or Sender)")
	}

	called, err := b.calledForHLR(msg, "SRI request")
	if err != nil {
		return err
	}
	calling := b.callingSCCP(sccp.SSNMAP)

	param, err := mapprotocol.EncodeSendRoutingInfoForSMArg(&mapprotocol.SendRoutingInfoForSMRequest{
		MSISDN:               mapprotocol.MSISDN(msg.MSISDN),
		SMRPPRI:              true,
		ServiceCentreAddress: *sca,
	})
	if err != nil {
		return fmt.Errorf("SRI encode: %w", err)
	}

	_, err = b.tcapHandler.Originate(
		&tcap.ACShortMsgGatewayV3, called, calling,
		int32(mapprotocol.OpSendRoutingInfoForSM), param,
		correlationID(msg),
	)
	if err != nil {
		return fmt.Errorf("SRI originate: %w", err)
	}
	b.logger.Debug("SRI-for-SM originated",
		zap.String("msisdn", msg.MSISDN),
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// onMTSmsRequest: SMSC asks gateway to deliver TPDU to a roamer's VMSC.
// Caller must supply network_node_number (VMSC GT, from a prior SRI) and
// the TPDU as base64 in tpdu_base64 (or raw in PayloadData).
func (b *natsBridge) onMTSmsRequest(msg *natsbridge.NatsMessage) error {
	tpdu, err := tpduFromMessage(msg)
	if err != nil {
		return err
	}
	if msg.IMSI == "" {
		return errors.New("MT SMS request: imsi required (run SRI-for-SM first)")
	}
	sca := paramAddressString(msg, paramServiceCentreAddress)
	if sca == nil {
		return errors.New("MT SMS request: service_centre_address required in Parameters")
	}
	nnn := paramAddressString(msg, paramNetworkNodeNumber)
	if nnn == nil {
		return errors.New("MT SMS request: network_node_number required in Parameters (from SRI response)")
	}
	if !isDigits(nnn.Digits) || len(nnn.Digits) > maxGTDigits {
		return fmt.Errorf("MT SMS request: network_node_number %q is not E.164 digits (at most %d)", nnn.Digits, maxGTDigits)
	}

	imsi := mapprotocol.IMSI(msg.IMSI)
	param, err := mapprotocol.EncodeMtForwardSMArg(&mapprotocol.MtForwardSMRequest{
		SMRPDA: mapprotocol.SMRPDA{IMSI: &imsi},
		SMRPOA: mapprotocol.SMRPOA{ServiceCentreAddress: sca},
		SMRPUI: tpdu,
	})
	if err != nil {
		return fmt.Errorf("MT-Fwd-SM encode: %w", err)
	}

	called := b.calledForGT(nnn.Digits, sccp.SSNMSC, msg.DPC)
	calling := b.callingSCCP(sccp.SSNMAP)
	_, err = b.tcapHandler.Originate(
		&tcap.ACShortMsgMTRelayV3, called, calling,
		int32(mapprotocol.OpMtForwardSM), param,
		correlationID(msg),
	)
	if err != nil {
		return fmt.Errorf("MT-Fwd-SM originate: %w", err)
	}
	b.logger.Debug("MT-Forward-SM originated",
		zap.String("imsi", msg.IMSI),
		zap.String("vmsc", nnn.Digits),
		zap.Int("tpdu_len", len(tpdu)),
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// onCAPInitialDP — gsmSCF → gsmSSF InitialDP origination. Non-standard
// (gsmSSF normally originates) but useful for test injection. CAP
// originate path is not yet wired (no cap.OperationCode encoder for this
// direction in the codec — caller must supply pre-encoded BER in
// PayloadData).
func (b *natsBridge) onCAPInitialDP(msg *natsbridge.NatsMessage) error {
	if len(msg.PayloadData) == 0 {
		return errors.New("CAP InitialDP origination requires pre-encoded BER in payload_data")
	}
	called := b.calledForGT(msg.GlobalTitle, sccp.SSNGSMSCF, msg.DPC)
	calling := b.callingSCCP(sccp.SSNCAP)

	const opInitialDP = int32(0) // CAP InitialDP op code
	_, err := b.tcapHandler.Originate(
		&tcap.ACCapV3, called, calling,
		opInitialDP, msg.PayloadData,
		correlationID(msg),
	)
	if err != nil {
		return fmt.Errorf("CAP InitialDP originate: %w", err)
	}
	b.logger.Debug("CAP InitialDP originated",
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// onINAPInitialDP — INAP is not implemented (ADR-002). Surface as an error
// so the SMSC sees the gap in its NATS reply path instead of a silent drop.
func (b *natsBridge) onINAPInitialDP(msg *natsbridge.NatsMessage) error {
	b.logger.Warn("INAP InitialDP rejected: INAP is not supported",
		zap.String("nats_correlation_id", correlationID(msg)))
	return errors.New("INAP is not supported")
}

// publishMAPResponse is the MAP response sink. Type-switches on the decoded
// response struct and republishes via the matching natsbridge.Publish*
// helper. Returning an error logs but does not abort downstream processing.
func (b *natsBridge) publishMAPResponse(corrID string, op mapprotocol.OperationCode, resp any) error {
	if corrID == "" {
		b.logger.Debug("MAP response without correlation id — not publishing",
			zap.Int32("op_code", int32(op)))
		return nil
	}
	switch r := resp.(type) {
	case *mapprotocol.SendRoutingInfoForSMResponse:
		vmsc := ""
		if r.LocationInfoWithLMSI.NetworkNodeNumber != nil {
			vmsc = r.LocationInfoWithLMSI.NetworkNodeNumber.Digits
		}
		return b.natsHandler.PublishSRIResponse(corrID, "", string(r.IMSI), vmsc)

	case *mapprotocol.MtForwardSMResponse:
		return b.natsHandler.PublishMTSmsResponse(corrID, ss7client.StatusDelivered, 0)

	case *mapprotocol.AnyTimeInterrogationResponse:
		return b.natsHandler.PublishSubscriberInfo(natsbridge.MsgTypeMAPATI, corrID, subscriberInfoWire(&r.SubscriberInfo))

	case *mapprotocol.ProvideSubscriberInfoResponse:
		return b.natsHandler.PublishSubscriberInfo(natsbridge.MsgTypeMAPPSI, corrID, subscriberInfoWire(&r.SubscriberInfo))

	case *mapprotocol.SendRoutingInfoResponse:
		return b.publishRoutingResponse(corrID, r)

	case *mapprotocol.MoForwardSMResponse:
		// MO response from VMSC — not part of the SMSC inbound flow today;
		// log for trace and treat as success.
		b.logger.Debug("MO-Forward-SM response received",
			zap.String("nats_correlation_id", corrID))
		return nil
	}
	b.logger.Warn("MAP response sink: no publisher for op",
		zap.Int32("op_code", int32(op)),
		zap.String("nats_correlation_id", corrID))
	return nil
}

// publishMAPFailure is the TCAP failure sink. Fires when an originated
// dialogue ends without a ReturnResult — i.e. local DialogueTimeout or
// inbound P/U-ABORT from peer. The upstream NATS waiter is keyed on
// corrID and would otherwise only see its own client-side timeout.
//
// Wire contract:
//   - Status uses the FailureReason token verbatim ("TIMEOUT" |
//     "ABORTED" | "REMOTE_ERROR") so SMSC retry logic can diverge per
//     GSMA SMS-IWMSC guidance (T-failure → retry with longer T_R;
//     remote error → consult code, often don't retry).
//   - ErrorCode uses a gateway-private negative range so it never
//     collides with the MAP 1..127 positive space. SMSCs with switch
//     tables on MAP error codes ignore negatives transparently.
//   - Failures publish on the same reply subject as successes so the
//     upstream waiter has one reply path per request — correlation by
//     corrID, no fragmented subscriber topology.
//
// The response subject follows the operation (responseTypeByOp), as for
// publishMAPResponse.
func (b *natsBridge) publishMAPFailure(corrID string, opCode int32, reason tcap.FailureReason, detail string) {
	if corrID == "" {
		return
	}
	b.logger.Warn("originated dialogue failed",
		zap.String("nats_correlation_id", corrID),
		zap.Int32("op_code", opCode),
		zap.String("reason", reason.String()),
		zap.String("detail", detail))

	b.publishFailure(corrID, mapprotocol.OperationCode(opCode), reason.String(), failureErrorCode(reason), detail)
}

// responseTypeByOp is the response message type — hence subject — of each
// operation a NATS request originates.
var responseTypeByOp = map[mapprotocol.OperationCode]natsbridge.MessageType{
	mapprotocol.OpSendRoutingInfoForSM:  natsbridge.MsgTypeMAPSRI,
	mapprotocol.OpMtForwardSM:           natsbridge.MsgTypeMAPMTSms,
	mapprotocol.OpAnyTimeInterrogation:  natsbridge.MsgTypeMAPATI,
	mapprotocol.OpProvideSubscriberInfo: natsbridge.MsgTypeMAPPSI,
	mapprotocol.OpSendRoutingInfo:       natsbridge.MsgTypeMAPRouting,
}

func (b *natsBridge) publishFailure(corrID string, op mapprotocol.OperationCode, status string, errCode int32, detail string) {
	msgType, ok := responseTypeByOp[op]
	if !ok {
		// MO-Forward-SM the gateway sends has no NATS waiter.
		b.logger.Warn("no response subject for op",
			zap.Int32("op_code", int32(op)),
			zap.String("nats_correlation_id", corrID))
		return
	}
	if err := b.natsHandler.PublishFailure(msgType, corrID, status, errCode, detail); err != nil {
		b.logger.Error("publish failure",
			zap.String("nats_correlation_id", corrID),
			zap.Int32("op_code", int32(op)),
			zap.Error(err))
	}
}

// publishMAPError is the MAP error sink: the peer answered an originated
// operation with a ReturnError or a Reject. Published on the operation's
// reply subject like every other outcome, with Status=REMOTE_ERROR and
// ErrorCode = the MAP error code (3GPP TS 29.002 MAP-ErrorCodes, positive),
// or ErrCodeRemoteError (-3) for a Reject / ReturnError without a code. The
// SMSC's retry decision depends on that code (absentSubscriberSM → retry
// later; unknownSubscriber → do not), which is why it is passed through
// rather than folded into a generic failure.
func (b *natsBridge) publishMAPError(corrID string, op mapprotocol.OperationCode, code mapprotocol.ErrorCode, detail string) {
	if corrID == "" {
		return
	}
	status, errCode := remoteErrorWire(code)
	b.logger.Warn("originated operation answered with an error",
		zap.String("nats_correlation_id", corrID),
		zap.Int32("op_code", int32(op)),
		zap.Int32("error_code", errCode),
		zap.String("detail", detail))

	b.publishFailure(corrID, op, status, errCode, detail)
}

// remoteErrorWire maps a peer's ReturnError / Reject to the wire status and
// code. code 0 means a Reject or a ReturnError without a local error code.
func remoteErrorWire(code mapprotocol.ErrorCode) (status string, errCode int32) {
	if code > 0 {
		return ss7client.StatusRemoteError, int32(code)
	}
	return ss7client.StatusRemoteError, ss7client.ErrCodeRemoteError
}

// HandleMoForwardSM implements `mapprotocol.MoForwardSMHandler`. Fires
// when a foreign MSC sends MO-Forward-SM (op 46) to our gateway —
// typically with CdPA = `sccp.cluster_global_title.address` (per
// ADR-006 §2 Model B), so any cluster node serving the loadshare can
// accept the dialogue.
//
// The SM-RP-DA / SM-RP-OA fields of the MO-Fwd-SM Arg are CHOICE-typed
// per TS 29.002 §17.7.4 (IMSI / LMSI / serviceCentreAddress for DA;
// MSISDN / serviceCentreAddressOA for OA). They are exposed by the
// codec as raw context-tagged primitives; we extract the digit strings
// via the typed helper accessors. When the originating network omits
// the IMSI extension, the IMSI field is empty — the SMSC consumer must
// be tolerant.
//
// On NATS publish failure (queue saturated), we return ErrSystemFailure
// to the foreign MSC. The MSC will retry per its own T-R; the gateway
// is allowed to refuse — preferable to silently dropping the SMS.
func (b *natsBridge) HandleMoForwardSM(req *mapprotocol.MoForwardSMRequest) (*mapprotocol.MoForwardSMResponse, *mapprotocol.ErrorCode, error) {
	mscGT := smRPOADigits(req.SMRPOA)
	smscGT := smRPDADigits(req.SMRPDA)
	imsi := ""
	if req.IMSI != nil {
		imsi = string(*req.IMSI)
	}
	// CorrelationID for inbound MO is gateway-minted: foreign MSCs do
	// not supply one (this is a network-originated transaction). The
	// SMSC consumer sees the corrID as the join-key for any subsequent
	// async events (delivery report, charging, etc.) it might publish
	// back. UUID is fine — uniqueness > readability for machine-to-
	// machine flows. Per-node attribution is conveyed via the
	// NatsMessage.NodeID field that PublishMOForwardSM populates from
	// the NATS handler's config; corrID itself is opaque.
	corrID := "mo-" + uuid.NewString()

	b.logger.Info("MO-Forward-SM received from peer",
		zap.String("nats_correlation_id", corrID),
		zap.String("msc_gt", mscGT),
		zap.String("smsc_gt", smscGT),
		zap.String("imsi", imsi),
		zap.Int("tpdu_len", len(req.SMRPUI)))

	if err := b.natsHandler.PublishMOForwardSM(corrID, mscGT, smscGT, imsi, req.SMRPUI); err != nil {
		b.logger.Error("publish MO-Forward-SM",
			zap.String("nats_correlation_id", corrID),
			zap.Error(err))
		// Tell the foreign MSC the SMS was not accepted — back-pressure
		// upstream is the operator-correct response under saturation.
		ec := mapprotocol.ErrSystemFailure
		return nil, &ec, err
	}

	// Empty success per TS 29.002 §17.7.4 — SMSC takes ownership; any
	// downstream delivery state is independent of the SS7 transaction.
	return &mapprotocol.MoForwardSMResponse{}, nil, nil
}

// smRPDADigits extracts the destination GT digits from a MO-Forward-SM
// SM-RP-DA per TS 29.002 §17.7.4. SM-RP-DA is a CHOICE; for MO-Fwd-SM
// the canonical alternative is `serviceCentreAddressDA` (the home
// SMSC's GT). Other alternatives (IMSI [0], LMSI [1], noSM-RP-DA [5])
// are valid in the abstract syntax but not used for MO routing — they
// yield "" so callers can tell "not the SMSC-address branch" apart
// from "empty digits".
func smRPDADigits(da mapprotocol.SMRPDA) string {
	if da.ServiceCentreAddress != nil {
		return da.ServiceCentreAddress.Digits
	}
	return ""
}

// smRPOADigits extracts the originator GT digits from SM-RP-OA per
// TS 29.002 §17.7.4. For MO-Fwd-SM the originating MSC's address is
// carried in the `msisdn` [2] alternative (which is an MSISDN-format
// AddressString containing the MSC's E.164 digits, NOT a subscriber's
// MSISDN — the spec is overloaded here). Returns "" when neither
// `msisdn` nor `serviceCentreAddressOA` is present.
func smRPOADigits(oa mapprotocol.SMRPOA) string {
	if oa.MSISDN != nil {
		return string(*oa.MSISDN)
	}
	if oa.ServiceCentreAddress != nil {
		return oa.ServiceCentreAddress.Digits
	}
	return ""
}

// failureErrorCode maps the failure reason to a gateway-private negative
// error code. Keep in sync with NatsMessage.ErrorCode consumers; positive
// codes remain reserved for MAP 3GPP TS 29.002 §17.6.
func failureErrorCode(r tcap.FailureReason) int32 {
	switch r {
	case tcap.FailureTimeout:
		return ss7client.ErrCodeTimeout
	case tcap.FailureAbortReceived, tcap.FailureNotDelivered:
		return ss7client.ErrCodeAborted
	case tcap.FailureRemoteError:
		return ss7client.ErrCodeRemoteError
	}
	return ss7client.ErrCodeUnclassified
}

// --- helpers -------------------------------------------------------------

func correlationID(msg *natsbridge.NatsMessage) string {
	if msg.CorrelationID != "" {
		return msg.CorrelationID
	}
	return msg.MessageID
}

// tpduFromMessage extracts the SM TPDU from either PayloadData (raw bytes)
// or Parameters[tpdu_base64]. The TPDU is opaque to the gateway — TS 23.040.
func tpduFromMessage(msg *natsbridge.NatsMessage) ([]byte, error) {
	if len(msg.PayloadData) > 0 {
		return msg.PayloadData, nil
	}
	v, ok := msg.Parameters[paramTPDUBase64]
	if !ok {
		return nil, errors.New("MT SMS: tpdu missing (set payload_data or parameters.tpdu_base64)")
	}
	s, ok := v.(string)
	if !ok {
		return nil, errors.New("MT SMS: tpdu_base64 must be a string")
	}
	return base64.StdEncoding.DecodeString(s)
}

// paramAddressString reads an AddressString from Parameters[key]. Accepts
// either a bare digit string (assumed International/E.164/ISDN) or a map
// with explicit nature/numbering_plan/digits — operators with non-default
// addressing can supply the map form.
func paramAddressString(msg *natsbridge.NatsMessage, key string) *mapprotocol.AddressString {
	v, ok := msg.Parameters[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: x}
	case map[string]interface{}:
		addr := &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN}
		if d, _ := x["digits"].(string); d != "" {
			addr.Digits = d
		} else {
			return nil
		}
		if n, ok := x["nature"].(float64); ok {
			addr.Nature = uint8(n)
		}
		if np, ok := x["numbering_plan"].(float64); ok {
			addr.NumberPlan = uint8(np)
		}
		return addr
	}
	return nil
}

// calledForHLR builds the SCCP called party address of a request to the
// subscriber's HLR (SendRoutingInfoForSM, AnyTimeInterrogation). An HLR named
// in the request wins. Otherwise, with map.sri_routing "msisdn" (the default)
// the subscriber's MSISDN is the GT, routed on GT so the STP finds the
// subscriber's HLR — the SMS-GMSC way; with "hlr" it is the configured
// map.hlr_gt / map.hlr_pc. A request naming the subscriber by IMSI alone has
// no MSISDN to route on.
func (b *natsBridge) calledForHLR(msg *natsbridge.NatsMessage, what string) (*sccp.SccpAddress, error) {
	if msg.GlobalTitle != "" || msg.DPC != 0 {
		if msg.GlobalTitle != "" && (!isDigits(msg.GlobalTitle) || len(msg.GlobalTitle) > maxGTDigits) {
			return nil, fmt.Errorf("%s: global_title %q is not E.164 digits (at most %d)", what, msg.GlobalTitle, maxGTDigits)
		}
		return b.calledForGT(msg.GlobalTitle, sccp.SSNHLR, msg.DPC), nil
	}
	if b.mapCfg.SRIRouting == mapprotocol.SRIRoutingHLR {
		if b.mapCfg.HLRGlobalTitle == "" && b.mapCfg.HLRPointCode == 0 {
			return nil, errors.New("HLR address: map.sri_routing is \"hlr\" but neither map.hlr_gt nor map.hlr_pc is set")
		}
		return b.calledForGT(b.mapCfg.HLRGlobalTitle, sccp.SSNHLR, b.mapCfg.HLRPointCode), nil
	}
	if msg.MSISDN == "" {
		return nil, fmt.Errorf("%s: by IMSI it needs the HLR's global_title or dpc, or map.sri_routing \"hlr\"", what)
	}
	if !isDigits(msg.MSISDN) || len(msg.MSISDN) > maxGTDigits {
		return nil, fmt.Errorf("%s: msisdn %q is not E.164 digits (at most %d)", what, msg.MSISDN, maxGTDigits)
	}
	return b.calledForGT(msg.MSISDN, sccp.SSNHLR, 0), nil
}

// maxGTDigits bounds the digits of a global title taken from a request: an
// E.164 number has at most 15, an ISDN-AddressString holds 16. The SCCP
// address encoder has a one-octet length, so an unbounded request field
// could otherwise corrupt the called party address.
const maxGTDigits = 16

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// calledForGT builds an SCCP CdPA for a peer GT + SSN (Q.713 §3.4.1).
// With a point code it routes on DPC/SSN to that point code, carrying the GT
// when there is one. Without a point code it routes on GT, and SCCP's GTT
// (sccp.gtt_rules) picks the DPC — normally the STP.
func (b *natsBridge) calledForGT(gt string, ssn sccp.SubsystemNumber, pc uint32) *sccp.SccpAddress {
	addr := &sccp.SccpAddress{
		AddressIndicator: sccp.AISSNPresent,
		PointCode:        pc,
		SubsystemNumber:  ssn,
	}
	if pc != 0 {
		addr.AddressIndicator |= sccp.AIPCPresent | sccp.AIRouteOnSSN
	}
	if gt != "" {
		addr.GlobalTitle = &sccp.GlobalTitle{
			GTI:             sccp.GTI0100,
			TranslationType: 0,
			NumberingPlan:   sccp.NPISDN,
			EncodingScheme:  sccp.ESBCD,
			NAI:             sccp.NAIInternational,
			Address:         gt,
		}
	}
	return addr
}

// callingSCCP builds the gateway's own SCCP address (CgPA). When
// sccp.local_global_title.address is set, attaches the cluster GT as
// AddressIndicator(GT+SSN+PC) — peers route responses back via the GT
// even when the active node migrates (HA failover is GT-based, not
// PC-based). Empty GT falls back to PC+SSN routing for in-network
// short-circuit operation.
func (b *natsBridge) callingSCCP(ssn sccp.SubsystemNumber) *sccp.SccpAddress {
	addr := &sccp.SccpAddress{
		AddressIndicator: sccp.AISSNPresent | sccp.AIPCPresent | sccp.AIRouteOnSSN,
		PointCode:        b.sccpCfg.LocalPointCode,
		SubsystemNumber:  ssn,
	}
	if gt := b.sccpCfg.BuildLocalGT(); gt != nil {
		// Route-on-GT tells the peer to answer to the GT, not the PC.
		addr.AddressIndicator = sccp.AISSNPresent | sccp.AIPCPresent | sccp.AIGTPresent
		addr.GlobalTitle = gt
	}
	return addr
}
