// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package sccp

import (
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	ourPC = 0x0101
	stpPC = 0x0202
	mscPC = 0x0303
)

type routingRig struct {
	h   *Handler
	in  chan *m3ua.MTP3Message
	out chan *m3ua.MTP3Message
}

func newRoutingRig(t *testing.T, gttEnabled bool, rules ...GTTRuleConfig) *routingRig {
	t.Helper()
	in := make(chan *m3ua.MTP3Message, 4)
	out := make(chan *m3ua.MTP3Message, 4)
	h, err := NewHandler(&Config{
		LocalPointCode:   ourPC,
		LocalSubsystems:  []SubsystemNumber{SSNMSC},
		LocalGlobalTitle: GTConfig{Address: "905900000001"},
		GTTEnabled:       gttEnabled,
		GTTRules:         rules,
	}, in, out, zap.NewNop())
	require.NoError(t, err)
	return &routingRig{h: h, in: in, out: out}
}

func (r *routingRig) sent(t *testing.T) (*m3ua.MTP3Message, *SccpMessage) {
	t.Helper()
	select {
	case m := <-r.out:
		msg, err := ParseSCCP(m.PayloadData, VariantITU)
		require.NoError(t, err)
		return m, msg
	case <-time.After(time.Second):
		t.Fatal("nothing sent to M3UA")
		return nil, nil
	}
}

func (r *routingRig) nothingSent(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.out:
		t.Fatalf("unexpected message to DPC %d", m.DPC)
	default:
	}
}

func gtAddr(digits string, ssn SubsystemNumber) *SccpAddress {
	return &SccpAddress{
		AddressIndicator: AISSNPresent,
		SubsystemNumber:  ssn,
		GlobalTitle:      &GlobalTitle{GTI: GTI0100, NumberingPlan: NPISDN, EncodingScheme: ESBCD, NAI: NAIInternational, Address: digits},
	}
}

func ourAddr() *SccpAddress {
	a := gtAddr("905900000001", SSNMSC)
	a.AddressIndicator |= AIPCPresent
	a.PointCode = ourPC
	return a
}

func udt(called, calling *SccpAddress) *SccpMessage {
	return &SccpMessage{MessageType: MsgTypeUDT, CalledAddress: called, CallingAddress: calling, Data: []byte{0x62, 0x00}}
}

// Intermediate translation: the STP is the next hop and translates again,
// so the called address must still route on GT.
func TestRouteOutbound_IntermediateGTTKeepsGTRouting(t *testing.T) {
	r := newRoutingRig(t, true, GTTRuleConfig{Pattern: "90*", PointCode: stpPC})
	require.NoError(t, r.h.SendMessage(udt(gtAddr("905304445566", SSNMSC), ourAddr())))

	label, msg := r.sent(t)
	assert.Equal(t, uint32(stpPC), label.DPC)
	assert.Equal(t, uint32(ourPC), label.OPC)
	assert.True(t, msg.CalledAddress.AddressIndicator.RoutesOnGT())
	assert.Equal(t, "905304445566", msg.CalledAddress.GlobalTitle.Address)
	assert.True(t, msg.ReturnOnError, "originated messages ask for a UDTS on failure")
}

// Final translation: the called address is rewritten to route on PC+SSN.
func TestRouteOutbound_FinalGTTRoutesOnSSN(t *testing.T) {
	r := newRoutingRig(t, true, GTTRuleConfig{Pattern: "*", PointCode: mscPC, SSN: uint8(SSNMSC)})
	require.NoError(t, r.h.SendMessage(udt(gtAddr("905304445566", SSNMSC), ourAddr())))

	label, msg := r.sent(t)
	assert.Equal(t, uint32(mscPC), label.DPC)
	assert.False(t, msg.CalledAddress.AddressIndicator.RoutesOnGT())
	assert.Equal(t, uint32(mscPC), msg.CalledAddress.PointCode)
	assert.Equal(t, SSNMSC, msg.CalledAddress.SubsystemNumber)
}

func TestRouteOutbound_HigherPriorityRuleWins(t *testing.T) {
	r := newRoutingRig(t, true,
		GTTRuleConfig{Pattern: "*", PointCode: stpPC},
		GTTRuleConfig{Pattern: "9053*", PointCode: mscPC, Priority: 10},
	)
	require.NoError(t, r.h.SendMessage(udt(gtAddr("905304445566", SSNMSC), ourAddr())))
	label, _ := r.sent(t)
	assert.Equal(t, uint32(mscPC), label.DPC)
}

func TestRouteOutbound_NoRoute(t *testing.T) {
	t.Run("no matching rule", func(t *testing.T) {
		r := newRoutingRig(t, true, GTTRuleConfig{Pattern: "44*", PointCode: stpPC})
		assert.ErrorIs(t, r.h.SendMessage(udt(gtAddr("905304445566", SSNMSC), ourAddr())), ErrNoRoute)
		r.nothingSent(t)
	})
	t.Run("GTT disabled", func(t *testing.T) {
		r := newRoutingRig(t, false, GTTRuleConfig{Pattern: "*", PointCode: stpPC})
		assert.ErrorIs(t, r.h.SendMessage(udt(gtAddr("905304445566", SSNMSC), ourAddr())), ErrNoRoute)
		r.nothingSent(t)
	})
	t.Run("route on SSN without a point code", func(t *testing.T) {
		r := newRoutingRig(t, true)
		called := &SccpAddress{AddressIndicator: AISSNPresent | AIRouteOnSSN, SubsystemNumber: SSNHLR}
		assert.ErrorIs(t, r.h.SendMessage(udt(called, ourAddr())), ErrNoRoute)
		r.nothingSent(t)
	})
}

func TestRouteOutbound_RouteOnSSNUsesCalledPC(t *testing.T) {
	r := newRoutingRig(t, false)
	called := &SccpAddress{AddressIndicator: AISSNPresent | AIPCPresent | AIRouteOnSSN, PointCode: mscPC, SubsystemNumber: SSNHLR}
	require.NoError(t, r.h.SendMessage(udt(called, ourAddr())))
	label, _ := r.sent(t)
	assert.Equal(t, uint32(mscPC), label.DPC)
}

// An end node never relays: a message for someone else is refused with a
// UDTS (when asked for) and is not sent anywhere else.
func TestIncoming_NotForUsIsRefusedNotRelayed(t *testing.T) {
	r := newRoutingRig(t, true, GTTRuleConfig{Pattern: "*", PointCode: stpPC})
	foreign := udt(gtAddr("447700900000", SSNMSC), gtAddr("905304445566", SSNMSC))
	foreign.ReturnOnError = true
	payload, err := EncodeSCCP(foreign, VariantITU)
	require.NoError(t, err)

	r.h.handleIncomingMessage(&m3ua.MTP3Message{OPC: stpPC, DPC: ourPC, SI: 3, PayloadData: payload})

	label, udts := r.sent(t)
	assert.Equal(t, MsgTypeUDTS, udts.MessageType)
	assert.Equal(t, CauseNoTranslationForAddress, udts.ReturnCause)
	assert.Equal(t, uint32(stpPC), label.DPC, "back to the node that handed it over")
	assert.Equal(t, "905304445566", udts.CalledAddress.GlobalTitle.Address, "addressed to the originator")
	r.nothingSent(t)
}

func TestIncoming_NoUDTSWithoutReturnOption(t *testing.T) {
	r := newRoutingRig(t, true, GTTRuleConfig{Pattern: "*", PointCode: stpPC})
	payload, err := EncodeSCCP(udt(gtAddr("447700900000", SSNMSC), gtAddr("905304445566", SSNMSC)), VariantITU)
	require.NoError(t, err)
	r.h.handleIncomingMessage(&m3ua.MTP3Message{OPC: stpPC, DPC: ourPC, SI: 3, PayloadData: payload})
	r.nothingSent(t)
}

// A UDTS for one of our messages is reported, not delivered to TCAP as
// data and never answered with another UDTS.
func TestIncoming_UDTSIsNotDelivered(t *testing.T) {
	r := newRoutingRig(t, true)
	payload, err := EncodeSCCP(&SccpMessage{
		MessageType: MsgTypeUDTS, ReturnCause: CauseNoTranslationForAddress,
		CalledAddress: ourAddr(), CallingAddress: gtAddr("905304445566", SSNMSC), Data: []byte{0x62, 0x00},
	}, VariantITU)
	require.NoError(t, err)
	r.h.handleIncomingMessage(&m3ua.MTP3Message{OPC: stpPC, DPC: ourPC, SI: 3, PayloadData: payload})

	select {
	case m := <-r.h.TcapIncomingChannel():
		t.Fatalf("UDTS delivered to TCAP: %+v", m)
	default:
	}
	r.nothingSent(t)
}

func TestIncoming_ForUsIsDelivered(t *testing.T) {
	r := newRoutingRig(t, true)
	payload, err := EncodeSCCP(udt(gtAddr("905900000001", SSNMSC), gtAddr("905304445566", SSNMSC)), VariantITU)
	require.NoError(t, err)
	r.h.handleIncomingMessage(&m3ua.MTP3Message{OPC: stpPC, DPC: ourPC, SI: 3, PayloadData: payload})
	select {
	case <-r.h.TcapIncomingChannel():
	case <-time.After(time.Second):
		t.Fatal("not delivered")
	}
}

func TestValidateGTTRule(t *testing.T) {
	for _, ok := range []GTTRuleConfig{
		{Pattern: "*", PointCode: 1},
		{Pattern: "90*", PointCode: 0x3FFF},
		{Pattern: "905304445566", PointCode: 7, SSN: 8},
	} {
		assert.NoError(t, validateGTTRule(ok, VariantITU), "%+v", ok)
	}
	for _, bad := range []GTTRuleConfig{
		{Pattern: "", PointCode: 1},
		{Pattern: "9*0*", PointCode: 1},
		{Pattern: "+90*", PointCode: 1},
		{Pattern: "*", PointCode: 0},
		{Pattern: "*", PointCode: 0x4000}, // over 14 bits
	} {
		assert.Error(t, validateGTTRule(bad, VariantITU), "%+v", bad)
	}
	assert.NoError(t, validateGTTRule(GTTRuleConfig{Pattern: "*", PointCode: 0x4000}, VariantANSI))
}
