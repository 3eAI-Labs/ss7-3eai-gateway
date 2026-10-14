// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/base64"
	"testing"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// Composition-root smoke tests for the NATS bridge. Verify the path
// `*NatsMessage → typed MAP request → BER encode → tcap.Originate(...)`
// without standing up a real NATS server or TCAP/SCCP/M3UA chain. The
// tcapOriginator interface lets a recording mock capture each Originate
// call so we can assert addresses, op code, and correlation flow.

type recordingOriginator struct {
	calls []originateCall
	dlg   *tcap.Dialogue // returned to caller; nil = synthesized
	err   error
}

type originateCall struct {
	ac        *tcap.ApplicationContext
	called    *sccp.SccpAddress
	calling   *sccp.SccpAddress
	opCode    int32
	parameter []byte
	corrID    string
}

func (r *recordingOriginator) Originate(
	ac *tcap.ApplicationContext,
	called, calling *sccp.SccpAddress,
	opCode int32,
	parameter []byte,
	corrID string,
) (*tcap.Dialogue, error) {
	r.calls = append(r.calls, originateCall{ac, called, calling, opCode, parameter, corrID})
	if r.err != nil {
		return nil, r.err
	}
	if r.dlg != nil {
		return r.dlg, nil
	}
	// Synthesize a minimal dialogue so callers that retain it (e.g. for
	// inspection) get a non-nil pointer with the originating context set.
	return &tcap.Dialogue{
		LocalTID:           0xDEADBEEF,
		ApplicationContext: ac,
		LocalAddress:       calling,
		RemoteAddress:      called,
		NATSCorrelationID:  corrID,
		OriginatingOpCode:  opCode,
	}, nil
}

func newBridgeUnderTest(t *testing.T) (*natsBridge, *recordingOriginator) {
	t.Helper()
	logger := zaptest.NewLogger(t)

	mapH, err := mapprotocol.NewHandler(
		&mapprotocol.Config{
			HLRAddress:     "127.0.0.1",
			HLRGlobalTitle: "905009990001",
			HLRPointCode:   1234,
		}, logger.Named("map"))
	require.NoError(t, err)

	rec := &recordingOriginator{}
	bridge := &natsBridge{
		mapHandler:  mapH,
		tcapHandler: rec,
		natsHandler: nil,
		mapCfg: &mapprotocol.Config{
			HLRAddress:     "127.0.0.1",
			HLRGlobalTitle: "905009990001",
			HLRPointCode:   1234,
		},
		sccpCfg: &sccp.Config{LocalPointCode: 9999},
		logger:  logger.Named("bridge"),
	}
	return bridge, rec
}

func TestBridge_SRIRequest_Originates(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)

	err := bridge.onSRIRequest(&natsbridge.NatsMessage{
		MessageID:     "msg-1",
		CorrelationID: "corr-sri-1",
		MSISDN:        "905321234567",
		Parameters: map[string]interface{}{
			paramServiceCentreAddress: "905001112233",
		},
	})
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)

	c := rec.calls[0]
	assert.Equal(t, tcap.ACShortMsgGatewayV3.OID, c.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpSendRoutingInfoForSM), c.opCode)
	assert.Equal(t, "corr-sri-1", c.corrID)

	// Default map.sri_routing "msisdn": the subscriber's MSISDN is the called
	// GT, routed on GT (the STP finds the HLR), SSN 6.
	require.NotNil(t, c.called.GlobalTitle)
	assert.Equal(t, "905321234567", c.called.GlobalTitle.Address)
	assert.Equal(t, sccp.SSNHLR, c.called.SubsystemNumber)
	assert.True(t, c.called.AddressIndicator.RoutesOnGT())
	assert.Zero(t, c.called.AddressIndicator&sccp.AIPCPresent)

	// Calling = local PC + SSN_MAP, no GT.
	assert.Equal(t, uint32(9999), c.calling.PointCode)
	assert.Equal(t, sccp.SSNMAP, c.calling.SubsystemNumber)
	assert.Nil(t, c.calling.GlobalTitle, "v1: own GT not configured, route on PC+SSN")

	// Parameter must be a non-empty BER blob; round-trip correctness is
	// covered exhaustively in mapprotocol/codec_sms_test.go.
	assert.NotEmpty(t, c.parameter, "encoded SRI-for-SM Arg must be non-empty")
}

func TestBridge_SRIRequest_MissingMSISDN(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	err := bridge.onSRIRequest(&natsbridge.NatsMessage{MessageID: "x"})
	assert.Error(t, err)
	assert.Empty(t, rec.calls, "no Originate call when validation fails")
}

func TestBridge_MTSmsRequest_RoutesToVMSC(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	tpdu := []byte{0x04, 0x0B, 0x91, 0x09, 0x53, 0x21, 0x43, 0x65, 0x87, 0x00, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'}

	err := bridge.onMTSmsRequest(&natsbridge.NatsMessage{
		MessageID:     "msg-mt-1",
		CorrelationID: "corr-mt-1",
		IMSI:          "286010123456789",
		PayloadData:   tpdu,
		Parameters: map[string]interface{}{
			paramServiceCentreAddress: "905001112233",
			paramNetworkNodeNumber:    "905304567890",
		},
	})
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)

	c := rec.calls[0]
	assert.Equal(t, tcap.ACShortMsgMTRelayV3.OID, c.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpMtForwardSM), c.opCode)
	assert.Equal(t, "corr-mt-1", c.corrID)
	require.NotNil(t, c.called.GlobalTitle)
	assert.Equal(t, "905304567890", c.called.GlobalTitle.Address,
		"MT-Fwd-SM routes to VMSC, not HLR")
	assert.Equal(t, sccp.SSNMSC, c.called.SubsystemNumber)
}

func TestBridge_MTSmsRequest_MissingNNN_Rejected(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	err := bridge.onMTSmsRequest(&natsbridge.NatsMessage{
		IMSI:        "286010123456789",
		PayloadData: []byte{0x01, 0x02},
		Parameters: map[string]interface{}{
			paramServiceCentreAddress: "905001112233",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network_node_number")
	assert.Empty(t, rec.calls)
}

func TestBridge_MTSmsRequest_TPDUFromBase64(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	tpdu := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	err := bridge.onMTSmsRequest(&natsbridge.NatsMessage{
		IMSI: "286010123456789",
		Parameters: map[string]interface{}{
			paramServiceCentreAddress: "905001112233",
			paramNetworkNodeNumber:    "905304567890",
			paramTPDUBase64:           base64.StdEncoding.EncodeToString(tpdu),
		},
	})
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)
}

func TestBridge_MTSmsRequest_MissingIMSI_Rejected(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)
	err := bridge.onMTSmsRequest(&natsbridge.NatsMessage{
		PayloadData: []byte{0x01},
		Parameters: map[string]interface{}{
			paramServiceCentreAddress: "905001112233",
			paramNetworkNodeNumber:    "905304567890",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "imsi")
}

func TestBridge_INAPInitialDP_Rejected(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	err := bridge.onINAPInitialDP(&natsbridge.NatsMessage{MessageID: "inap-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INAP is not supported")
	assert.Empty(t, rec.calls, "INAP must not originate any TCAP")
}

func TestBridge_PublishMAPResponse_NoCorrelationID_NoOp(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)
	// Empty correlation ID means in-process reply or test path; sink
	// should short-circuit without dereferencing the (nil) NATS handler.
	err := bridge.publishMAPResponse("", mapprotocol.OpSendRoutingInfoForSM,
		&mapprotocol.SendRoutingInfoForSMResponse{IMSI: "1234"})
	assert.NoError(t, err)
}

// callingSCCP contract: when sccp.local_global_title.address is empty,
// CgPA carries PC+SSN only (in-network short-circuit, no GT routing).
func TestBridge_CallingSCCP_NoOwnGT(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)
	addr := bridge.callingSCCP(sccp.SSNMAP)

	assert.Equal(t, sccp.AISSNPresent|sccp.AIPCPresent|sccp.AIRouteOnSSN, addr.AddressIndicator,
		"no own GT: route on PC+SSN, GT bit clear")
	assert.Nil(t, addr.GlobalTitle, "GT pointer nil when disabled")
	assert.Equal(t, uint32(9999), addr.PointCode)
	assert.Equal(t, sccp.SSNMAP, addr.SubsystemNumber)
}

// calledForGT contract (Q.713 §3.4.1 routing indicator): a point code means
// route on DPC/SSN to it; no point code means route on GT, left to GTT.
func TestBridge_CalledForGT_RoutingIndicator(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)

	onGT := bridge.calledForGT("905304445566", sccp.SSNMSC, 0)
	assert.True(t, onGT.AddressIndicator.RoutesOnGT())
	assert.Zero(t, onGT.AddressIndicator&sccp.AIPCPresent, "no PC to encode")
	require.NotNil(t, onGT.GlobalTitle)

	onSSN := bridge.calledForGT("905304445566", sccp.SSNMSC, 0x0202)
	assert.False(t, onSSN.AddressIndicator.RoutesOnGT())
	assert.NotZero(t, onSSN.AddressIndicator&sccp.AIPCPresent)
	assert.Equal(t, uint32(0x0202), onSSN.PointCode)
	require.NotNil(t, onSSN.GlobalTitle, "the GT travels along")

	pcOnly := bridge.calledForGT("", sccp.SSNHLR, 0x0202)
	assert.False(t, pcOnly.AddressIndicator.RoutesOnGT())
	assert.Nil(t, pcOnly.GlobalTitle)
}

// callingSCCP contract: when sccp.local_global_title.address is set,
// CgPA includes the cluster-shared GT so HLR/MSC peers can route the
// response back via GT (HA-friendly). Defaults: GTI0100, NPISDN, NAIIntl.
func TestBridge_CallingSCCP_WithOwnGT(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)
	bridge.sccpCfg.LocalGlobalTitle = sccp.GTConfig{Address: "905900000001"}

	addr := bridge.callingSCCP(sccp.SSNMAP)
	assert.Equal(t,
		sccp.AISSNPresent|sccp.AIPCPresent|sccp.AIGTPresent,
		addr.AddressIndicator,
		"own GT: route on GT (routing indicator clear) so peers answer to the GT")
	require.NotNil(t, addr.GlobalTitle)
	assert.Equal(t, "905900000001", addr.GlobalTitle.Address)
	assert.Equal(t, sccp.GTI0100, addr.GlobalTitle.GTI)
	assert.Equal(t, sccp.NPISDN, addr.GlobalTitle.NumberingPlan)
	assert.Equal(t, sccp.NAIInternational, addr.GlobalTitle.NAI)
}

// The failure Status on the wire is tcap.FailureReason.String(); the client
// module publishes those tokens and codes as its contract. Pin the two together
// so neither side can drift.
func TestBridge_FailureTokensMatchClientContract(t *testing.T) {
	cases := []struct {
		reason tcap.FailureReason
		status string
		code   int32
	}{
		{tcap.FailureTimeout, ss7client.StatusTimeout, ss7client.ErrCodeTimeout},
		{tcap.FailureAbortReceived, ss7client.StatusAborted, ss7client.ErrCodeAborted},
		{tcap.FailureRemoteError, ss7client.StatusRemoteError, ss7client.ErrCodeRemoteError},
	}
	for _, c := range cases {
		assert.Equal(t, c.status, c.reason.String())
		assert.Equal(t, c.code, failureErrorCode(c.reason))
	}
	assert.Equal(t, ss7client.ErrCodeUnclassified, failureErrorCode(tcap.FailureReason(255)))
}

// The client publishes the MAP error codes an SMSC acts on; they must be the
// codes the gateway's MAP layer passes through.
func TestBridge_ClientMAPErrorCodesMatchMAPLayer(t *testing.T) {
	pairs := map[int32]mapprotocol.ErrorCode{
		ss7client.MAPErrUnknownSubscriber:           mapprotocol.ErrUnknownSubscriber,
		ss7client.MAPErrUnidentifiedSubscriber:      mapprotocol.ErrUnidentifiedSubscriber,
		ss7client.MAPErrAbsentSubscriberSM:          mapprotocol.ErrAbsentSubscriberSM,
		ss7client.MAPErrIllegalSubscriber:           mapprotocol.ErrIllegalSubscriber,
		ss7client.MAPErrTeleserviceNotProvisioned:   mapprotocol.ErrTeleserviceNotProvisioned,
		ss7client.MAPErrIllegalEquipment:            mapprotocol.ErrIllegalEquipment,
		ss7client.MAPErrCallBarred:                  mapprotocol.ErrCallBarred,
		ss7client.MAPErrFacilityNotSupported:        mapprotocol.ErrFacilityNotSupported,
		ss7client.MAPErrSubscriberBusyForMTSMS:      mapprotocol.ErrSubscriberBusyForMTSMS,
		ss7client.MAPErrSMDeliveryFailure:           mapprotocol.ErrSMDeliveryFailure,
		ss7client.MAPErrSystemFailure:               mapprotocol.ErrSystemFailure,
		ss7client.MAPErrDataMissing:                 mapprotocol.ErrDataMissing,
		ss7client.MAPErrUnexpectedDataValue:         mapprotocol.ErrUnexpectedDataValue,
		ss7client.MAPErrATINotAllowed:               mapprotocol.ErrATINotAllowed,
		ss7client.MAPErrBearerServiceNotProvisioned: mapprotocol.ErrBearerServiceNotProvisioned,
		ss7client.MAPErrForwardingViolation:         mapprotocol.ErrForwardingViolation,
		ss7client.MAPErrCUGReject:                   mapprotocol.ErrCUGReject,
		ss7client.MAPErrAbsentSubscriber:            mapprotocol.ErrAbsentSubscriber,
		ss7client.MAPErrNumberChanged:               mapprotocol.ErrNumberChanged,
		ss7client.MAPErrBusySubscriber:              mapprotocol.ErrBusySubscriber,
		ss7client.MAPErrNoSubscriberReply:           mapprotocol.ErrNoSubscriberReply,
		ss7client.MAPErrORNotAllowed:                mapprotocol.ErrORNotAllowed,
	}
	for clientCode, mapCode := range pairs {
		assert.Equal(t, int32(mapCode), clientCode, mapCode.String())
	}
}

func TestBridge_RemoteErrorWire(t *testing.T) {
	status, code := remoteErrorWire(mapprotocol.ErrAbsentSubscriberSM)
	assert.Equal(t, ss7client.StatusRemoteError, status)
	assert.Equal(t, ss7client.MAPErrAbsentSubscriberSM, code, "MAP error code passes through")

	status, code = remoteErrorWire(0) // Reject, or ReturnError without a code
	assert.Equal(t, ss7client.StatusRemoteError, status)
	assert.Equal(t, ss7client.ErrCodeRemoteError, code)
}

func TestBridge_PublishMAPError_NoCorrelationID_NoOp(t *testing.T) {
	bridge, _ := newBridgeUnderTest(t)
	// Must return before touching the (nil) NATS handler.
	bridge.publishMAPError("", mapprotocol.OpSendRoutingInfoForSM, mapprotocol.ErrUnknownSubscriber, "unknownSubscriber")
}

// map.sri_routing "hlr", and an HLR named in the request.
func TestBridge_SRIRequest_HLRRouting(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	sri := func(gt string, pc uint32) *sccp.SccpAddress {
		rec.calls = nil
		require.NoError(t, bridge.onSRIRequest(&natsbridge.NatsMessage{
			CorrelationID: "c", MSISDN: "905321234567", GlobalTitle: gt, DPC: pc,
			Parameters: map[string]interface{}{paramServiceCentreAddress: "905001112233"},
		}))
		require.Len(t, rec.calls, 1)
		return rec.calls[0].called
	}

	bridge.mapCfg.SRIRouting = mapprotocol.SRIRoutingHLR
	called := sri("", 0)
	assert.Equal(t, "905009990001", called.GlobalTitle.Address, "configured map.hlr_gt")
	assert.Equal(t, uint32(1234), called.PointCode, "configured map.hlr_pc")

	bridge.mapCfg.SRIRouting = mapprotocol.SRIRoutingMSISDN
	called = sri("447700900123", 0)
	assert.Equal(t, "447700900123", called.GlobalTitle.Address, "an HLR named in the request wins")
	assert.True(t, called.AddressIndicator.RoutesOnGT())
}

func TestBridge_SRIRequest_RejectsNonDigitMSISDN(t *testing.T) {
	bridge, rec := newBridgeUnderTest(t)
	err := bridge.onSRIRequest(&natsbridge.NatsMessage{
		CorrelationID: "c", MSISDN: "+90532",
		Parameters: map[string]interface{}{paramServiceCentreAddress: "905001112233"},
	})
	require.Error(t, err)
	assert.Empty(t, rec.calls)
}
