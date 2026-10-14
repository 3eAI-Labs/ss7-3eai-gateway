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

func locationBridge(t *testing.T) (*natsBridge, *recordingOriginator) {
	b, rec := newBridgeUnderTest(t)
	b.sccpCfg.LocalSubsystems = []sccp.SubsystemNumber{sccp.SSNHLR, sccp.SSNMSC, sccp.SSNGSMSCF}
	b.sccpCfg.LocalGlobalTitle.Address = "905900000001"
	return b, rec
}

func TestBridge_ATIRequest_Originates(t *testing.T) {
	b, rec := locationBridge(t)
	b.sccpCfg.ClusterGlobalTitle.Address = "905900000000"
	err := b.onATIRequest(&natsbridge.NatsMessage{
		CorrelationID: "c1", MSISDN: "905321112233",
		RequestedInfo: &ss7client.RequestedInfo{Location: true, State: true, Domain: ss7client.DomainPS},
	})
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)
	c := rec.calls[0]
	assert.Equal(t, tcap.ACAnyTimeInfoEnquiryV3.OID, c.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpAnyTimeInterrogation), c.opCode)
	assert.Equal(t, "905321112233", c.called.GlobalTitle.Address, "routed on the MSISDN (map.sri_routing default)")
	assert.Equal(t, sccp.SSNHLR, c.called.SubsystemNumber)
	assert.Equal(t, sccp.SSNGSMSCF, c.calling.SubsystemNumber, "the HLR answers the gsmSCF")
	arg, err := mapprotocol.DecodeAnyTimeInterrogationArg(c.parameter)
	require.NoError(t, err)
	assert.Equal(t, "905900000000", arg.GSMSCFAddress.Digits, "no map.gsmscf_address: the cluster GT")
	require.NotNil(t, arg.RequestedInfo.RequestedDomain)
	assert.Equal(t, mapprotocol.DomainPS, *arg.RequestedInfo.RequestedDomain)
}

// The gsmSCF-Address is configuration: a request naming another one — an
// attempt to pass as a gsmSCF the HLR admits — does not change it.
func TestBridge_ATIRequest_GSMSCFAddressIsConfigured(t *testing.T) {
	b, rec := locationBridge(t)
	b.mapCfg.GSMSCFAddress = "905009990000"
	require.NoError(t, b.onATIRequest(&natsbridge.NatsMessage{
		MSISDN: "905321112233", RequestedInfo: &ss7client.RequestedInfo{State: true},
		Parameters: map[string]interface{}{"gsmscf_address": "905009990077"},
	}))
	arg, err := mapprotocol.DecodeAnyTimeInterrogationArg(rec.calls[0].parameter)
	require.NoError(t, err)
	assert.Equal(t, "905009990000", arg.GSMSCFAddress.Digits)
}

func TestBridge_ATIRequest_Rejections(t *testing.T) {
	state := &ss7client.RequestedInfo{State: true}
	cases := map[string]struct {
		msg   natsbridge.NatsMessage
		setup func(*natsBridge)
		want  string
	}{
		"no identity":     {msg: natsbridge.NatsMessage{RequestedInfo: state}, want: "exactly one"},
		"both identities": {msg: natsbridge.NatsMessage{MSISDN: "1", IMSI: "2", RequestedInfo: state}, want: "exactly one"},
		"nothing requested": {msg: natsbridge.NatsMessage{MSISDN: "1", RequestedInfo: &ss7client.RequestedInfo{}},
			want: "selects nothing"},
		"bad domain": {msg: natsbridge.NatsMessage{MSISDN: "1", RequestedInfo: &ss7client.RequestedInfo{State: true, Domain: "x"}},
			want: "domain"},
		"by IMSI, no HLR": {msg: natsbridge.NatsMessage{IMSI: "286010000000123", RequestedInfo: state}, want: "by IMSI"},
		"HLR GT not digits": {msg: natsbridge.NatsMessage{IMSI: "286010000000123", GlobalTitle: "+90530", RequestedInfo: state},
			want: "global_title"},
		"gsmSCF SSN not local": {msg: natsbridge.NatsMessage{MSISDN: "1", RequestedInfo: state},
			setup: func(b *natsBridge) { b.sccpCfg.LocalSubsystems = []sccp.SubsystemNumber{sccp.SSNHLR} }, want: "147"},
		"no gsmSCF address": {msg: natsbridge.NatsMessage{MSISDN: "1", RequestedInfo: state},
			setup: func(b *natsBridge) { b.sccpCfg.LocalGlobalTitle.Address = "" }, want: "gsmSCF-Address"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b, rec := locationBridge(t)
			if c.setup != nil {
				c.setup(b)
			}
			err := b.onATIRequest(&c.msg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			assert.Empty(t, rec.calls)
		})
	}
}

func TestBridge_ATIRequest_ByIMSIWithHLR(t *testing.T) {
	b, rec := locationBridge(t)
	require.NoError(t, b.onATIRequest(&natsbridge.NatsMessage{
		IMSI: "286010000000123", GlobalTitle: "905300000099", RequestedInfo: &ss7client.RequestedInfo{State: true},
	}))
	assert.Equal(t, "905300000099", rec.calls[0].called.GlobalTitle.Address)
}

func TestBridge_PSIRequest_Originates(t *testing.T) {
	b, rec := locationBridge(t)
	require.NoError(t, b.onPSIRequest(&natsbridge.NatsMessage{
		CorrelationID: "c2", IMSI: "286010000000123",
		Parameters:    map[string]interface{}{ss7client.ParamVLRNumber: "905304445566"},
		RequestedInfo: &ss7client.RequestedInfo{Location: true, CurrentLocation: true},
	}))
	c := rec.calls[0]
	assert.Equal(t, tcap.ACSubscriberInfoEnquiryV3.OID, c.ac.OID)
	assert.Equal(t, int32(mapprotocol.OpProvideSubscriberInfo), c.opCode)
	assert.Equal(t, "905304445566", c.called.GlobalTitle.Address)
	assert.Equal(t, sccp.SSNVLR, c.called.SubsystemNumber)
	assert.Equal(t, sccp.SSNHLR, c.calling.SubsystemNumber)
	arg, err := mapprotocol.DecodeProvideSubscriberInfoArg(c.parameter)
	require.NoError(t, err)
	assert.Equal(t, mapprotocol.IMSI("286010000000123"), arg.IMSI)
	assert.True(t, arg.RequestedInfo.CurrentLocation)
}

func TestBridge_PSIRequest_Rejections(t *testing.T) {
	vlr := map[string]interface{}{ss7client.ParamVLRNumber: "905304445566"}
	for name, msg := range map[string]natsbridge.NatsMessage{
		"no IMSI": {Parameters: vlr, RequestedInfo: &ss7client.RequestedInfo{State: true}},
		"no VLR":  {IMSI: "286010000000123", RequestedInfo: &ss7client.RequestedInfo{State: true}},
		"MNP":     {IMSI: "286010000000123", Parameters: vlr, RequestedInfo: &ss7client.RequestedInfo{MNP: true}},
		"VLR not digits": {IMSI: "286010000000123", Parameters: map[string]interface{}{ss7client.ParamVLRNumber: "+905304445566"},
			RequestedInfo: &ss7client.RequestedInfo{State: true}},
		"current location without location": {IMSI: "286010000000123", Parameters: vlr,
			RequestedInfo: &ss7client.RequestedInfo{State: true, CurrentLocation: true}},
	} {
		b, rec := locationBridge(t)
		require.Error(t, b.onPSIRequest(&msg), name)
		assert.Empty(t, rec.calls, name)
	}
}

func TestSubscriberInfoWire(t *testing.T) {
	age := 4
	got := subscriberInfoWire(&mapprotocol.SubscriberInfo{
		LocationInformation: &mapprotocol.LocationInformation{
			AgeOfLocationInformation: &age,
			VLRNumber:                &mapprotocol.AddressString{Nature: 1, NumberPlan: 1, Digits: "905304445566"},
			LAI:                      &mapprotocol.LAI{MCC: "286", MNC: "01", LAC: 3},
			GeographicalInformation:  []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80},
		},
		LocationInformationGPRS: &mapprotocol.LocationInformation{
			SGSNNumber: &mapprotocol.AddressString{Digits: "905304440000"}, RouteingAreaIdentity: []byte{0x82, 0xF6, 0x10, 0, 3, 7},
		},
		PSSubscriberState: &mapprotocol.SubscriberState{State: "ps-Detached"},
		MSClassmark2:      []byte{0x33, 0x19, 0xA2},
		MNPInfoRes:        &mapprotocol.MNPInfoRes{RouteingNumber: "1234", NumberPortabilityStatus: "ownNumberPortedOut"},
	})
	want := &ss7client.SubscriberInfo{
		Location: &ss7client.Location{
			AgeMinutes: &age, VLRNumber: "905304445566",
			LocationArea:            &ss7client.LocationArea{MCC: "286", MNC: "01", LAC: 3},
			GeographicalInformation: "1020304050607080",
		},
		LocationGPRS: &ss7client.Location{SGSNNumber: "905304440000", RoutingAreaID: "82f610000307"},
		PSState:      &ss7client.SubscriberState{State: "ps-Detached"},
		MSClassmark2: "3319a2",
		MNP:          &ss7client.MNPInfo{RouteingNumber: "1234", Status: "ownNumberPortedOut"},
	}
	assert.Equal(t, want, got)
}
