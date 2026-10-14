// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	ss7client "github.com/3eAI-Labs/ss7-3eai-gateway-client"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"go.uber.org/zap"
)

// The MAP location interface (TS 29.002 §8.11): AnyTimeInterrogation, with
// the gateway as gsmSCF asking the subscriber's HLR, and
// ProvideSubscriberInfo, with the gateway in the HLR's role asking the
// serving VLR. Both are originated only; a peer cannot open either context
// with the gateway.

// onATIRequest originates AnyTimeInterrogation (op 71) under
// anyTimeInfoEnquiryContext-v3.
func (b *natsBridge) onATIRequest(msg *natsbridge.NatsMessage) error {
	identity := mapprotocol.SubscriberIdentity{}
	switch {
	case msg.MSISDN != "" && msg.IMSI == "":
		m := mapprotocol.MSISDN(msg.MSISDN)
		identity.MSISDN = &m
	case msg.IMSI != "" && msg.MSISDN == "":
		i := mapprotocol.IMSI(msg.IMSI)
		identity.IMSI = &i
	default:
		return errors.New("ATI request: exactly one of msisdn and imsi required")
	}
	requested, err := requestedInfo(msg, true)
	if err != nil {
		return fmt.Errorf("ATI request: %w", err)
	}
	// The gsmSCF-Address is the HLR's basis for admitting the request, so it
	// is the operator's configuration, never the requester's: a NATS
	// publisher cannot present itself as another gsmSCF.
	scf := b.gsmSCFAddress()
	if scf == nil {
		return errors.New("ATI request: no gsmSCF-Address (set map.gsmscf_address or an sccp global title)")
	}
	// The HLR answers to the calling address: SSN 147, gsmSCF (MAP),
	// TS 23.003 §8.2.
	if err := b.requireLocalSSN(sccp.SSNGSMSCF); err != nil {
		return fmt.Errorf("ATI request: %w", err)
	}
	called, err := b.calledForHLR(msg, "ATI request")
	if err != nil {
		return err
	}
	param, err := mapprotocol.EncodeAnyTimeInterrogationArg(&mapprotocol.AnyTimeInterrogationRequest{
		SubscriberIdentity: identity,
		RequestedInfo:      requested,
		GSMSCFAddress:      *scf,
	})
	if err != nil {
		return fmt.Errorf("ATI encode: %w", err)
	}
	if _, err := b.tcapHandler.Originate(
		&tcap.ACAnyTimeInfoEnquiryV3, called, b.callingSCCP(sccp.SSNGSMSCF),
		int32(mapprotocol.OpAnyTimeInterrogation), param, correlationID(msg),
	); err != nil {
		return fmt.Errorf("ATI originate: %w", err)
	}
	b.logger.Debug("AnyTimeInterrogation originated",
		zap.String("msisdn", msg.MSISDN), zap.String("imsi", msg.IMSI),
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// onPSIRequest originates ProvideSubscriberInfo (op 70) under
// subscriberInfoEnquiryContext-v3, addressed to the VLR number with SSN 7.
func (b *natsBridge) onPSIRequest(msg *natsbridge.NatsMessage) error {
	if msg.IMSI == "" {
		return errors.New("PSI request: imsi required")
	}
	vlr := paramAddressString(msg, ss7client.ParamVLRNumber)
	if vlr == nil {
		return errors.New("PSI request: vlr_number required in parameters")
	}
	if !isDigits(vlr.Digits) || len(vlr.Digits) > maxGTDigits {
		return fmt.Errorf("PSI request: vlr_number %q is not E.164 digits (at most %d)", vlr.Digits, maxGTDigits)
	}
	requested, err := requestedInfo(msg, false)
	if err != nil {
		return fmt.Errorf("PSI request: %w", err)
	}
	// The VLR answers to the HLR's subsystem, SSN 6.
	if err := b.requireLocalSSN(sccp.SSNHLR); err != nil {
		return fmt.Errorf("PSI request: %w", err)
	}
	param, err := mapprotocol.EncodeProvideSubscriberInfoArg(&mapprotocol.ProvideSubscriberInfoRequest{
		IMSI:          mapprotocol.IMSI(msg.IMSI),
		RequestedInfo: requested,
	})
	if err != nil {
		return fmt.Errorf("PSI encode: %w", err)
	}
	if _, err := b.tcapHandler.Originate(
		&tcap.ACSubscriberInfoEnquiryV3, b.calledForGT(vlr.Digits, sccp.SSNVLR, msg.DPC), b.callingSCCP(sccp.SSNHLR),
		int32(mapprotocol.OpProvideSubscriberInfo), param, correlationID(msg),
	); err != nil {
		return fmt.Errorf("PSI originate: %w", err)
	}
	b.logger.Debug("ProvideSubscriberInfo originated",
		zap.String("imsi", msg.IMSI), zap.String("vlr", vlr.Digits),
		zap.String("nats_correlation_id", correlationID(msg)))
	return nil
}

// requestedInfo validates the request's requested_info, as the client does,
// and maps it to the MAP RequestedInfo. MNP information is an
// AnyTimeInterrogation item only.
func requestedInfo(msg *natsbridge.NatsMessage, ati bool) (mapprotocol.RequestedInfo, error) {
	r := msg.RequestedInfo
	switch {
	case r == nil || !(r.Location || r.State || r.IMEI || r.MSClassmark || r.MNP):
		return mapprotocol.RequestedInfo{}, errors.New("requested_info selects nothing")
	case r.MNP && !ati:
		return mapprotocol.RequestedInfo{}, errors.New("requested_info.mnp is available through AnyTimeInterrogation only")
	case (r.CurrentLocation || r.EPSLocation) && !r.Location:
		return mapprotocol.RequestedInfo{}, errors.New("requested_info.current_location and eps_location require location")
	}
	out := mapprotocol.RequestedInfo{
		LocationInformation:             r.Location,
		SubscriberState:                 r.State,
		CurrentLocation:                 r.CurrentLocation,
		IMEI:                            r.IMEI,
		MSClassmark:                     r.MSClassmark,
		MNPRequestedInfo:                r.MNP,
		LocationInformationEPSSupported: r.EPSLocation,
	}
	switch r.Domain {
	case "":
	case ss7client.DomainCS:
		d := mapprotocol.DomainCS
		out.RequestedDomain = &d
	case ss7client.DomainPS:
		d := mapprotocol.DomainPS
		out.RequestedDomain = &d
	default:
		return mapprotocol.RequestedInfo{}, fmt.Errorf("requested_info.domain %q is not %q or %q", r.Domain, ss7client.DomainCS, ss7client.DomainPS)
	}
	return out, nil
}

// gsmSCFAddress is the gsmSCF-Address an AnyTimeInterrogation carries:
// map.gsmscf_address, else the cluster GT (one
// identity for the HLR to admit, whichever node asks), else this node's GT.
func (b *natsBridge) gsmSCFAddress() *mapprotocol.AddressString {
	for _, digits := range []string{
		b.mapCfg.GSMSCFAddress,
		b.sccpCfg.ClusterGlobalTitle.Address,
		b.sccpCfg.LocalGlobalTitle.Address,
	} {
		if digits != "" {
			return &mapprotocol.AddressString{
				Nature: mapprotocol.NatureInternational, NumberPlan: mapprotocol.NumberingPlanISDN, Digits: digits,
			}
		}
	}
	return nil
}

// requireLocalSSN refuses a request whose answer would come back to a
// subsystem this node does not serve: SCCP would return it with a UDTS
// instead of delivering it.
func (b *natsBridge) requireLocalSSN(ssn sccp.SubsystemNumber) error {
	if !slices.Contains(b.sccpCfg.LocalSubsystems, ssn) {
		return fmt.Errorf("sccp.local_subsystems lacks %d, the subsystem the answer is addressed to", ssn)
	}
	return nil
}

// subscriberInfoWire maps the decoded SubscriberInfo to the contract's.
func subscriberInfoWire(si *mapprotocol.SubscriberInfo) *ss7client.SubscriberInfo {
	out := &ss7client.SubscriberInfo{
		Location:     locationWire(si.LocationInformation),
		State:        stateWire(si.SubscriberState),
		LocationGPRS: locationWire(si.LocationInformationGPRS),
		PSState:      stateWire(si.PSSubscriberState),
		EPSState:     stateWire(si.EPSSubscriberState),
		LocationEPS:  locationEPSWire(si.LocationInformationEPS),
		IMEI:         si.IMEI,
		MSClassmark2: hex.EncodeToString(si.MSClassmark2),
	}
	if m := si.MNPInfoRes; m != nil {
		out.MNP = &ss7client.MNPInfo{
			RouteingNumber: m.RouteingNumber,
			IMSI:           string(m.IMSI),
			MSISDN:         digits(m.MSISDN),
			Status:         m.NumberPortabilityStatus,
		}
	}
	return out
}

func locationWire(li *mapprotocol.LocationInformation) *ss7client.Location {
	if li == nil {
		return nil
	}
	out := &ss7client.Location{
		AgeMinutes:               li.AgeOfLocationInformation,
		VLRNumber:                digits(li.VLRNumber),
		MSCNumber:                digits(li.MSCNumber),
		SGSNNumber:               digits(li.SGSNNumber),
		RoutingAreaID:            hex.EncodeToString(li.RouteingAreaIdentity),
		LocationNumber:           hex.EncodeToString(li.LocationNumber),
		GeographicalInformation:  hex.EncodeToString(li.GeographicalInformation),
		GeodeticInformation:      hex.EncodeToString(li.GeodeticInformation),
		CurrentLocationRetrieved: li.CurrentLocationRetrieved,
		EPS:                      locationEPSWire(li.EPS),
	}
	if c := li.CellGlobalID; c != nil {
		out.CellGlobalID = &ss7client.CellGlobalID{MCC: c.MCC, MNC: c.MNC, LAC: c.LAC, CI: c.CI}
	}
	if s := li.ServiceAreaID; s != nil {
		out.ServiceAreaID = &ss7client.ServiceAreaID{MCC: s.MCC, MNC: s.MNC, LAC: s.LAC, SAC: s.SAC}
	}
	if l := li.LAI; l != nil {
		out.LocationArea = &ss7client.LocationArea{MCC: l.MCC, MNC: l.MNC, LAC: l.LAC}
	}
	return out
}

func locationEPSWire(e *mapprotocol.LocationInformationEPS) *ss7client.LocationEPS {
	if e == nil {
		return nil
	}
	return &ss7client.LocationEPS{
		ECGI:                     hex.EncodeToString(e.EUTRANCGI),
		TAI:                      hex.EncodeToString(e.TrackingAreaIdentity),
		MMEName:                  e.MMEName,
		AgeMinutes:               e.AgeOfLocationInformation,
		GeographicalInformation:  hex.EncodeToString(e.GeographicalInformation),
		GeodeticInformation:      hex.EncodeToString(e.GeodeticInformation),
		CurrentLocationRetrieved: e.CurrentLocationRetrieved,
	}
}

func stateWire(s *mapprotocol.SubscriberState) *ss7client.SubscriberState {
	if s == nil {
		return nil
	}
	return &ss7client.SubscriberState{State: s.State, NotReachableReason: s.NotReachableReason}
}

func digits(a *mapprotocol.AddressString) string {
	if a == nil {
		return ""
	}
	return a.Digits
}
