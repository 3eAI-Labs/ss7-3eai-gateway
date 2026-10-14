// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/hex"
	"errors"
	"fmt"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// Call routing (TS 29.002 §10.1): SendRoutingInfo, with the gateway as GMSC
// asking the subscriber's HLR. Originated only; a peer cannot open
// locationInfoRetrievalContext with the gateway.

// maxISDNDigits is the most digits an ISDN-AddressString holds (TS 29.002
// maxISDN-AddressLength 9 octets: the nature octet and eight of TBCD).
const maxISDNDigits = 16

// onRoutingRequest originates SendRoutingInfo (op 22), a basic call
// interrogation, under locationInfoRetrievalContext-v3.
func (b *natsBridge) onRoutingRequest(msg *natsbridge.NatsMessage) error {
	if msg.MSISDN == "" {
		return errors.New("SRI request: msisdn required")
	}
	if !isDigits(msg.MSISDN) || len(msg.MSISDN) > maxISDNDigits {
		return fmt.Errorf("SRI request: msisdn %q is not E.164 digits (at most %d)", msg.MSISDN, maxISDNDigits)
	}
	// The GMSC address tells the HLR — and, through ProvideRoamingNumber,
	// the VLR — which GMSC asks; it is this gateway's, never the
	// requester's.
	gmsc := b.gmscAddress()
	if gmsc == nil {
		return errors.New("SRI request: no GMSC address (set an sccp global title)")
	}
	// The HLR answers to the calling address: SSN 8, MSC (the GMSC
	// function), TS 23.003 §8.2.
	if err := b.requireLocalSSN(sccp.SSNMSC); err != nil {
		return fmt.Errorf("SRI request: %w", err)
	}
	called, err := b.calledForHLR(msg, "SRI request")
	if err != nil {
		return err
	}
	param, err := mapprotocol.EncodeSendRoutingInfoArg(&mapprotocol.SendRoutingInfoRequest{
		MSISDN:            mapprotocol.MSISDN(msg.MSISDN),
		InterrogationType: mapprotocol.InterrogationBasicCall,
		GMSCAddress:       *gmsc,
	})
	if err != nil {
		return fmt.Errorf("SRI encode: %w", err)
	}
	if _, err := b.tcapHandler.Originate(
		&tcap.ACLocationInfoRetrievalV3, called, b.callingSCCP(sccp.SSNMSC),
		int32(mapprotocol.OpSendRoutingInfo), param, correlationID(msg),
	); err != nil {
		return fmt.Errorf("SRI originate: %w", err)
	}
	b.logger.Debug("SendRoutingInfo originated",
		zap.String("msisdn", msg.MSISDN),
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// gmscAddress is the gmsc-OrGsmSCF-Address a SendRoutingInfo carries: the
// cluster GT (one GMSC identity whichever node asks), else this node's GT.
func (b *natsBridge) gmscAddress() *mapprotocol.AddressString {
	for _, digits := range []string{b.sccpCfg.ClusterGlobalTitle.Address, b.sccpCfg.LocalGlobalTitle.Address} {
		if digits != "" {
			return &mapprotocol.AddressString{
				Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: digits,
			}
		}
	}
	return nil
}

// forwardingReasonNames are the values of ForwardingOptions bits 4-3
// (TS 29.002 ForwardingOptions, as used in a SendRoutingInfo result).
var forwardingReasonNames = [4]string{"notReachable", "busy", "noReply", "unconditional"}

// routingInfoWire maps the decoded SendRoutingInfoRes to the contract's
// routing_info.
func routingInfoWire(r *mapprotocol.SendRoutingInfoResponse) *ss7client.RoutingInfo {
	out := &ss7client.RoutingInfo{
		RoamingNumber:                   addressWire(r.RoamingNumber),
		Forwarding:                      forwardingWire(r.ForwardingData),
		VMSCAddress:                     digits(r.VMSCAddress),
		MSISDN:                          digits(r.MSISDN),
		ForwardingInterrogationRequired: r.ForwardingInterrogationRequired,
		NumberPortabilityStatus:         r.NumberPortabilityStatus,
	}
	if c := r.CamelRoutingInfo; c != nil {
		out.CAMEL = &ss7client.CAMELRouting{
			Forwarding:       forwardingWire(c.ForwardingData),
			SubscriptionInfo: hex.EncodeToString(c.GMSCCamelSubscriptionInfo),
		}
	}
	if bs := r.BasicService; bs != nil {
		kind := "ext-Teleservice"
		if bs.Bearer {
			kind = "ext-BearerService"
		}
		out.BasicService = &ss7client.BasicService{Kind: kind, Code: hex.EncodeToString(bs.Code)}
	}
	for _, c := range r.SSList {
		out.SSCodes = append(out.SSCodes, hex.EncodeToString([]byte{c}))
	}
	return out
}

func forwardingWire(fd *mapprotocol.ForwardingData) *ss7client.ForwardingData {
	if fd == nil {
		return nil
	}
	out := &ss7client.ForwardingData{
		ForwardedToNumber:     addressWire(fd.ForwardedToNumber),
		ForwardedToSubaddress: hex.EncodeToString(fd.ForwardedToSubaddress),
	}
	// The long form, when present, is the complete number.
	if fd.LongForwardedToNumber != nil {
		out.ForwardedToNumber = addressWire(fd.LongForwardedToNumber)
	}
	if o := fd.ForwardingOptions; o != nil {
		out.Options = &ss7client.ForwardingOptions{
			NotifyForwardingParty:   *o&0x80 != 0,
			RedirectingPresentation: *o&0x40 != 0,
			NotifyCallingParty:      *o&0x20 != 0,
			Reason:                  forwardingReasonNames[*o>>2&0x03],
		}
	}
	return out
}

func addressWire(a *mapprotocol.AddressString) *ss7client.AddressString {
	if a == nil {
		return nil
	}
	return &ss7client.AddressString{Digits: a.Digits, Nature: a.Nature, NumberingPlan: a.NumberPlan}
}

// publishRoutingResponse publishes a SendRoutingInfo answer.
func (b *natsBridge) publishRoutingResponse(corrID string, r *mapprotocol.SendRoutingInfoResponse) error {
	var info *ss7client.SubscriberInfo
	if r.SubscriberInfo != nil {
		info = subscriberInfoWire(r.SubscriberInfo)
	}
	return b.natsHandler.PublishRoutingInfo(corrID, string(r.IMSI), routingInfoWire(r), info)
}
