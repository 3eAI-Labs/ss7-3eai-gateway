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
)

// Mapping between the client contract's location management types and the
// MAP codec's. Names are the ASN.1 value names, in the client's tables
// (bit or value order); what comes from an application is checked here
// again, not trusted to its client.

// updateLocationWire is what a VLR's updateLocation carries, for the turns.
func updateLocationWire(arg *mapprotocol.UpdateLocationArg, callingGT string) *ss7client.LocationUpdate {
	u := &ss7client.LocationUpdate{
		MSCNumber:                   addressWire(&arg.MSCNumber),
		VLRNumber:                   addressWire(&arg.VLRNumber),
		LMSI:                        hex.EncodeToString(arg.LMSI),
		IMEISV:                      arg.IMEISV,
		SkipSubscriberDataUpdate:    arg.SkipSubscriberDataUpdate,
		RestorationIndicator:        arg.RestorationIndicator,
		InformPreviousNetworkEntity: arg.InformPreviousNetworkEntity,
		CSLCSNotSupportedByUE:       arg.CSLCSNotSupportedByUE,
		CallingGT:                   callingGT,
	}
	if c := arg.VLRCapability; c != nil {
		w := &ss7client.VLRCapability{
			SupportedCAMELPhases:         numbered(c.SupportedCAMELPhases, 4),
			OfferedCAMEL4CSIs:            named(c.OfferedCAMEL4CSIs, ss7client.CAMEL4CSINames()),
			SupportedRATTypes:            named(c.SupportedRATTypes, ss7client.RATTypeNames()),
			SupportedLCSCapabilitySets:   numbered(c.SupportedLCSCapabilitySets, 5),
			LongFTNSupported:             c.LongFTNSupported,
			MSISDNLessOperationSupported: c.MSISDNLessOperationSupported,
			MTRoamingForwardingSupported: c.MTRoamingForwardingSupported,
			SOLSASupported:               c.SOLSASupport,
			LongGroupIDSupported:         c.LongGroupIDSupported,
			ResetIDsSupported:            c.ResetIDsSupported,
		}
		if c.ISTSupport != nil {
			w.ISTSupport = [2]string{"basicISTSupported", "istCommandSupported"}[*c.ISTSupport]
		}
		if s := c.SuperCharger; s != nil {
			w.SuperCharger = &ss7client.SuperCharger{SendSubscriberData: s.SendSubscriberData,
				SubscriberDataAge: hex.EncodeToString(s.SubscriberDataAge)}
		}
		u.VLRCapability = w
	}
	return u
}

// numbered maps bits 0.. to the numbers 1.. they stand for (CAMEL phases,
// LCS capability sets), dropping bits past max: the ASN.1 says other bits
// are discarded.
func numbered(bits []int, max int) []int {
	var out []int
	for _, b := range bits {
		if b < max {
			out = append(out, b+1)
		}
	}
	return out
}

// named maps bits to their names, dropping bits without one.
func named(bits []int, names []string) []string {
	var out []string
	for _, b := range bits {
		if b < len(names) {
			out = append(out, names[b])
		}
	}
	return out
}

// bitsOf maps names to their bits; an unknown name is an error.
func bitsOf(what string, names, table []string) (mapprotocol.NamedBits, error) {
	bits := mapprotocol.NamedBits{}
	for _, n := range names {
		i := slices.Index(table, n)
		if i < 0 {
			return nil, fmt.Errorf("%s %q is not one of %v", what, n, table)
		}
		bits = append(bits, i)
	}
	return bits, nil
}

func ssStatusOctet(s ss7client.SSStatus) byte {
	var b byte
	if s.Quiescent {
		b |= 0x08
	}
	if s.Provisioned {
		b |= 0x04
	}
	if s.Registered {
		b |= 0x02
	}
	if s.Active {
		b |= 0x01
	}
	return b
}

func hexOctets(what, s string, min, max int) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) < min || len(b) > max {
		return nil, fmt.Errorf("%s %q is not hex of %d..%d octets", what, s, min, max)
	}
	return b, nil
}

func basicServiceMAP(b *ss7client.BasicService) (*mapprotocol.BasicServiceCode, error) {
	if b == nil {
		return nil, nil
	}
	var bearer bool
	switch b.Kind {
	case "ext-BearerService":
		bearer = true
	case "ext-Teleservice":
	default:
		return nil, fmt.Errorf("basic service kind %q", b.Kind)
	}
	code, err := hexOctets("basic service code", b.Code, 1, 5)
	if err != nil {
		return nil, err
	}
	return &mapprotocol.BasicServiceCode{Bearer: bearer, Code: code}, nil
}

func addressMAP(what string, a *ss7client.AddressString) (*mapprotocol.AddressString, error) {
	if a == nil {
		return nil, nil
	}
	if a.Digits == "" {
		return nil, fmt.Errorf("%s without digits", what)
	}
	return &mapprotocol.AddressString{Nature: a.Nature, NumberPlan: a.NumberingPlan, Digits: a.Digits}, nil
}

var forwardingReasons = forwardingReasonNames[:]

// subscriberDataMAP checks an application's subscriber data and maps it to
// the codec's; the codec checks sizes again as it encodes.
func subscriberDataMAP(d *ss7client.SubscriberData) (*mapprotocol.SubscriberData, error) {
	if d == nil {
		return nil, errors.New("subscriber data missing")
	}
	out := &mapprotocol.SubscriberData{RoamingRestrictionDueToUnsupportedFeature: d.RoamingRestrictedUnsupportedFeature}
	var err error
	if out.MSISDN, err = addressMAP("msisdn", d.MSISDN); err != nil {
		return nil, err
	}
	if d.Category != "" {
		if out.Category, err = hexOctets("category", d.Category, 1, 1); err != nil {
			return nil, err
		}
	}
	if d.SubscriberStatus != "" {
		i := slices.Index(ss7client.SubscriberStatusNames(), d.SubscriberStatus)
		if i < 0 {
			return nil, fmt.Errorf("subscriber_status %q", d.SubscriberStatus)
		}
		s := mapprotocol.SubscriberStatus(i)
		out.SubscriberStatus = &s
	}
	if (d.SubscriberStatus == "operatorDeterminedBarring") != (d.ODB != nil) {
		return nil, errors.New("odb goes with subscriber_status operatorDeterminedBarring, and only with it (TS 29.002 §8.8.1.3)")
	}
	for _, l := range []struct {
		what  string
		codes []string
		into  *[][]byte
	}{{"bearer service", d.BearerServices, &out.BearerServices}, {"teleservice", d.Teleservices, &out.Teleservices}} {
		for _, c := range l.codes {
			b, err := hexOctets(l.what, c, 1, 5)
			if err != nil {
				return nil, err
			}
			*l.into = append(*l.into, b)
		}
	}
	ssCode := func(c string) (byte, error) {
		b, err := hexOctets("ss_code", c, 1, 1)
		if err != nil {
			return 0, err
		}
		return b[0], nil
	}
	for _, f := range d.Forwarding {
		code, err := ssCode(f.SSCode)
		if err != nil {
			return nil, err
		}
		fi := &mapprotocol.ForwardingInfo{SSCode: code}
		for _, ft := range f.Features {
			m := mapprotocol.ForwardingFeature{SSStatus: ssStatusOctet(ft.Status)}
			if m.BasicService, err = basicServiceMAP(ft.BasicService); err != nil {
				return nil, err
			}
			if m.ForwardedToNumber, err = addressMAP("forwarded_to_number", ft.ForwardedToNumber); err != nil {
				return nil, err
			}
			if ft.ForwardedToSubaddress != "" {
				if m.ForwardedToSubaddress, err = hexOctets("forwarded_to_subaddress", ft.ForwardedToSubaddress, 1, 21); err != nil {
					return nil, err
				}
			}
			if o := ft.Options; o != nil {
				reason := slices.Index(forwardingReasons, o.Reason)
				if reason < 0 {
					return nil, fmt.Errorf("forwarding options reason %q", o.Reason)
				}
				octet := byte(reason) << 2
				if o.NotifyForwardingParty {
					octet |= 0x80
				}
				if o.RedirectingPresentation {
					octet |= 0x40
				}
				if o.NotifyCallingParty {
					octet |= 0x20
				}
				m.ForwardingOptions = []byte{octet}
			}
			if ft.NoReplyTime != 0 {
				t := ft.NoReplyTime
				m.NoReplyConditionTime = &t
			}
			fi.Features = append(fi.Features, m)
		}
		out.ProvisionedSS = append(out.ProvisionedSS, mapprotocol.SSInfo{Forwarding: fi})
	}
	for _, c := range d.CallBarring {
		code, err := ssCode(c.SSCode)
		if err != nil {
			return nil, err
		}
		cb := &mapprotocol.CallBarringInfo{SSCode: code}
		for _, ft := range c.Features {
			bs, err := basicServiceMAP(ft.BasicService)
			if err != nil {
				return nil, err
			}
			cb.Features = append(cb.Features, mapprotocol.CallBarringFeature{BasicService: bs, SSStatus: ssStatusOctet(ft.Status)})
		}
		out.ProvisionedSS = append(out.ProvisionedSS, mapprotocol.SSInfo{CallBarring: cb})
	}
	for _, s := range d.SSData {
		code, err := ssCode(s.SSCode)
		if err != nil {
			return nil, err
		}
		sd := &mapprotocol.SSData{SSCode: code, SSStatus: ssStatusOctet(s.Status)}
		if s.CLIRestriction != "" {
			i := slices.Index(ss7client.CLIRestrictionNames(), s.CLIRestriction)
			if i < 0 {
				return nil, fmt.Errorf("cli_restriction %q", s.CLIRestriction)
			}
			sd.CLIRestrictionOption = &i
		}
		if s.OverrideCategory != "" {
			i := slices.Index(ss7client.OverrideCategoryNames(), s.OverrideCategory)
			if i < 0 {
				return nil, fmt.Errorf("override_category %q", s.OverrideCategory)
			}
			sd.OverrideCategory = &i
		}
		for i := range s.BasicServices {
			bs, err := basicServiceMAP(&s.BasicServices[i])
			if err != nil {
				return nil, err
			}
			sd.BasicServices = append(sd.BasicServices, *bs)
		}
		out.ProvisionedSS = append(out.ProvisionedSS, mapprotocol.SSInfo{SSData: sd})
	}
	if o := d.ODB; o != nil {
		odb := &mapprotocol.ODBData{}
		if odb.General, err = bitsOf("odb general category", o.General, ss7client.ODBGeneralNames()); err != nil {
			return nil, err
		}
		if len(o.HPLMN) > 0 {
			if odb.HPLMN, err = bitsOf("odb hplmn category", o.HPLMN, ss7client.ODBHPLMNNames()); err != nil {
				return nil, err
			}
		}
		out.ODB = odb
	}
	for _, z := range d.RegionalSubscription {
		b, err := hexOctets("zone code", z, 2, 2)
		if err != nil {
			return nil, err
		}
		out.RegionalSubscription = append(out.RegionalSubscription, b)
	}
	if d.NetworkAccessMode != "" {
		i := slices.Index(ss7client.NetworkAccessModeNames(), d.NetworkAccessMode)
		if i < 0 {
			return nil, fmt.Errorf("network_access_mode %q", d.NetworkAccessMode)
		}
		m := mapprotocol.NetworkAccessMode(i)
		out.NetworkAccessMode = &m
	}
	if len(d.AccessRestriction) > 0 {
		if out.AccessRestrictionData, err = bitsOf("access restriction", d.AccessRestriction, ss7client.AccessRestrictionNames()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// insertResultWire maps a VLR's InsertSubscriberDataRes.
func insertResultWire(r *mapprotocol.InsertSubscriberDataRes) *ss7client.InsertResult {
	hexes := func(codes [][]byte) []string {
		var out []string
		for _, c := range codes {
			out = append(out, hex.EncodeToString(c))
		}
		return out
	}
	w := &ss7client.InsertResult{
		UnsupportedTeleservices:   hexes(r.Teleservices),
		UnsupportedBearerServices: hexes(r.BearerServices),
		UnsupportedODB:            named(r.ODBGeneralData, ss7client.ODBGeneralNames()),
		SupportedCAMELPhases:      numbered(r.SupportedCAMELPhases, 4),
		OfferedCAMEL4CSIs:         named(r.OfferedCAMEL4CSIs, ss7client.CAMEL4CSINames()),
	}
	for _, c := range r.SSCodes {
		w.UnsupportedSSCodes = append(w.UnsupportedSSCodes, hex.EncodeToString([]byte{c}))
	}
	if v := r.RegionalSubscriptionResponse; v != nil {
		w.RegionalSubscriptionResponse = ss7client.RegionalSubscriptionResponseNames()[*v]
	}
	return w
}

// mergeInsertResults is one result for data sent in parts: what any part
// did not take, the last word on the rest.
func mergeInsertResults(into, r *mapprotocol.InsertSubscriberDataRes) {
	into.Teleservices = append(into.Teleservices, r.Teleservices...)
	into.BearerServices = append(into.BearerServices, r.BearerServices...)
	into.SSCodes = append(into.SSCodes, r.SSCodes...)
	for _, b := range r.ODBGeneralData {
		if !slices.Contains(into.ODBGeneralData, b) {
			into.ODBGeneralData = append(into.ODBGeneralData, b)
		}
	}
	if r.RegionalSubscriptionResponse != nil {
		into.RegionalSubscriptionResponse = r.RegionalSubscriptionResponse
	}
	if r.SupportedCAMELPhases != nil {
		into.SupportedCAMELPhases = r.SupportedCAMELPhases
	}
	if r.OfferedCAMEL4CSIs != nil {
		into.OfferedCAMEL4CSIs = r.OfferedCAMEL4CSIs
	}
}
