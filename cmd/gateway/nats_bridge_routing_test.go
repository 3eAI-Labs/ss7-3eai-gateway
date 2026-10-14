// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBridge_RoutingRequest_Originates(t *testing.T) {
	b, rec := locationBridge(t)
	b.sccpCfg.ClusterGlobalTitle.Address = "905900000000"
	require.NoError(t, b.onRoutingRequest(&natsbridge.NatsMessage{CorrelationID: "c1", MSISDN: "905321112233"}))
	require.Len(t, rec.calls, 1)
	c := rec.calls[0]
	assert.Equal(t, tcap.ACLocationInfoRetrievalV3.OID, c.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpSendRoutingInfo), c.opCode)
	assert.Equal(t, "905321112233", c.called.GlobalTitle.Address, "routed on the MSISDN (map.sri_routing default)")
	assert.Equal(t, sccp.SSNHLR, c.called.SubsystemNumber)
	assert.Equal(t, sccp.SSNMSC, c.calling.SubsystemNumber, "the HLR answers the GMSC")
	arg, err := mapprotocol.DecodeSendRoutingInfoArg(c.parameter)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.MSISDN("905321112233"), arg.MSISDN)
	assert.Equal(t, mapprotocol.InterrogationBasicCall, arg.InterrogationType)
	assert.Equal(t, "905900000000", arg.GMSCAddress.Digits, "the cluster GT")
	assert.Equal(t, mapprotocol.NatureInternational, arg.GMSCAddress.Nature)
}

// The request names only the subscriber and, optionally, the HLR: anything
// else it carries changes nothing on the wire.
func TestBridge_RoutingRequest_IgnoresOtherFields(t *testing.T) {
	b, rec := locationBridge(t)
	require.NoError(t, b.onRoutingRequest(&natsbridge.NatsMessage{
		MSISDN: "905321112233", GlobalTitle: "905320000009", DPC: 0x0102, SSN: 147,
		Parameters: map[string]interface{}{"interrogation_type": "forwarding", "gmsc_address": "905009990077"},
	}))
	c := rec.calls[0]
	assert.Equal(t, "905320000009", c.called.GlobalTitle.Address)
	assert.Equal(t, uint32(0x0102), c.called.PointCode)
	assert.Equal(t, sccp.SSNHLR, c.called.SubsystemNumber)
	arg, err := mapprotocol.DecodeSendRoutingInfoArg(c.parameter)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.InterrogationBasicCall, arg.InterrogationType)
	assert.Equal(t, "905900000001", arg.GMSCAddress.Digits, "no cluster GT: the node's GT")
}

func TestBridge_RoutingRequest_Rejections(t *testing.T) {
	cases := map[string]struct {
		msg   natsbridge.NatsMessage
		setup func(*natsBridge)
		want  string
	}{
		"no msisdn":         {msg: natsbridge.NatsMessage{IMSI: "286010000000123"}, want: "msisdn required"},
		"msisdn with +":     {msg: natsbridge.NatsMessage{MSISDN: "+905321112233"}, want: "not E.164"},
		"msisdn too long":   {msg: natsbridge.NatsMessage{MSISDN: "12345678901234567"}, want: "at most 16"},
		"HLR GT not digits": {msg: natsbridge.NatsMessage{MSISDN: "1", GlobalTitle: "90 53"}, want: "global_title"},
		"MSC SSN not local": {msg: natsbridge.NatsMessage{MSISDN: "1"},
			setup: func(b *natsBridge) { b.sccpCfg.LocalSubsystems = []sccp.SubsystemNumber{sccp.SSNHLR} }, want: "lacks 8"},
		"no GMSC address": {msg: natsbridge.NatsMessage{MSISDN: "1"},
			setup: func(b *natsBridge) { b.sccpCfg.LocalGlobalTitle.Address = "" }, want: "GMSC address"},
	}
	for name, c := range cases {
		b, rec := locationBridge(t)
		if c.setup != nil {
			c.setup(b)
		}
		msg := c.msg
		err := b.onRoutingRequest(&msg)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), c.want, name)
		assert.Empty(t, rec.calls, name)
	}
}

func TestRoutingInfoWire(t *testing.T) {
	isdn := func(d string) *mapprotocol.AddressString {
		return &mapprotocol.AddressString{Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: d}
	}
	got := routingInfoWire(&mapprotocol.SendRoutingInfoResponse{
		IMSI: "286010000000123", RoamingNumber: isdn("905329990001"), VMSCAddress: isdn("905320000001"),
		SSList: []byte{0x21, 0x29}, BasicService: &mapprotocol.BasicServiceCode{Code: []byte{0x11}},
		NumberPortabilityStatus: "notKnownToBePorted", ForwardingInterrogationRequired: true,
	})
	assert.Equal(t, &ss7client.RoutingInfo{
		RoamingNumber: &ss7client.AddressString{Digits: "905329990001", Nature: 1, NumberingPlan: 1},
		VMSCAddress:   "905320000001", SSCodes: []string{"21", "29"},
		BasicService:            &ss7client.BasicService{Kind: "ext-Teleservice", Code: "11"},
		NumberPortabilityStatus: "notKnownToBePorted", ForwardingInterrogationRequired: true,
	}, got)

	// ForwardingOptions: bit 8 notify forwarding party, 7 redirecting
	// presentation, 6 notify calling party, 4-3 the reason.
	for opts, want := range map[byte]ss7client.ForwardingOptions{
		0x00: {Reason: "notReachable"},
		0x24: {NotifyCallingParty: true, Reason: "busy"},
		0xC8: {NotifyForwardingParty: true, RedirectingPresentation: true, Reason: "noReply"},
		0xEC: {NotifyForwardingParty: true, RedirectingPresentation: true, NotifyCallingParty: true, Reason: "unconditional"},
	} {
		o := opts
		f := routingInfoWire(&mapprotocol.SendRoutingInfoResponse{ForwardingData: &mapprotocol.ForwardingData{
			ForwardedToNumber:     isdn("905551112233"),
			LongForwardedToNumber: &mapprotocol.AddressString{Nature: 0, NumberPlan: 9, Digits: "12345678901234567890"},
			ForwardedToSubaddress: []byte{0xA0, 0x50}, ForwardingOptions: &o,
		}}).Forwarding
		assert.Equal(t, want, *f.Options, "options %#x", opts)
		assert.Equal(t, &ss7client.AddressString{Digits: "12345678901234567890", Nature: 0, NumberingPlan: 9},
			f.ForwardedToNumber, "the long form is the whole number")
		assert.Equal(t, "a050", f.ForwardedToSubaddress)
	}

	camel := routingInfoWire(&mapprotocol.SendRoutingInfoResponse{CamelRoutingInfo: &mapprotocol.CamelRoutingInfo{
		GMSCCamelSubscriptionInfo: []byte{0xA0, 0x03, 0x80, 0x01, 0x05},
	}})
	assert.Equal(t, &ss7client.CAMELRouting{SubscriptionInfo: "a003800105"}, camel.CAMEL)
	assert.Equal(t, "ext-BearerService", routingInfoWire(&mapprotocol.SendRoutingInfoResponse{
		BasicService: &mapprotocol.BasicServiceCode{Bearer: true, Code: []byte{0x10}}}).BasicService.Kind)
}
