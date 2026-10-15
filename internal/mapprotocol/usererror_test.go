// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
)

// Error parameters built by hand from TS 29.002 MAP-ER-DataTypes:
// UnknownSubscriberParam and RoamingNotAllowedParam are SEQUENCEs, their
// causes untagged ENUMERATEDs (0A), the additional roaming cause [0] (80).

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	require.NoError(t, err)
	return b
}

func TestUserErrorParameter(t *testing.T) {
	for name, c := range map[string]struct {
		e    mapprotocol.UserError
		want string
	}{
		"unknownSubscriber, no diagnostic":  {mapprotocol.UserError{Code: mapprotocol.ErrUnknownSubscriber}, ""},
		"unknownSubscriber, imsiUnknown":    {mapprotocol.UserError{mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseIMSIUnknown}, "30 03 0A 01 00"},
		"unknownSubscriber, gprs-eps":       {mapprotocol.UserError{mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseGPRSEPSSubscriptionUnknown}, "30 03 0A 01 01"},
		"roamingNotAllowed, no cause given": {mapprotocol.UserError{Code: mapprotocol.ErrRoamingNotAllowed}, "30 03 0A 01 00"},
		"roamingNotAllowed, PLMN":           {mapprotocol.UserError{mapprotocol.ErrRoamingNotAllowed, mapprotocol.CausePLMNRoamingNotAllowed}, "30 03 0A 01 00"},
		"roamingNotAllowed, ODB":            {mapprotocol.UserError{mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseOperatorDeterminedBarring}, "30 03 0A 01 03"},
		"roamingNotAllowed, RAT types":      {mapprotocol.UserError{mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseSupportedRATTypesNotAllowed}, "30 06 0A 01 00 80 01 00"},
		"systemFailure":                     {mapprotocol.UserError{Code: mapprotocol.ErrSystemFailure}, ""},
	} {
		got, err := c.e.Parameter()
		require.NoError(t, err, name)
		if c.want == "" {
			assert.Nil(t, got, name)
			continue
		}
		assert.Equal(t, hexBytes(t, c.want), got, name)
	}
	for _, e := range []mapprotocol.UserError{
		{mapprotocol.ErrSystemFailure, mapprotocol.CauseIMSIUnknown},
		{mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseOperatorDeterminedBarring},
		{mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseGPRSEPSSubscriptionUnknown},
		{mapprotocol.ErrUnknownSubscriber, mapprotocol.ErrorCause(99)},
	} {
		_, err := e.Parameter()
		assert.Error(t, err, "%s with %s", e.Code, e.Cause)
	}
}

func TestDecodeUserErrorCause(t *testing.T) {
	for name, c := range map[string]struct {
		code  mapprotocol.ErrorCode
		param string
		want  mapprotocol.ErrorCause
	}{
		"unknownSubscriber, no parameter":           {mapprotocol.ErrUnknownSubscriber, "", mapprotocol.CauseNone},
		"unknownSubscriber, empty":                  {mapprotocol.ErrUnknownSubscriber, "30 00", mapprotocol.CauseNone},
		"unknownSubscriber, gprs-eps":               {mapprotocol.ErrUnknownSubscriber, "30 03 0A 01 01", mapprotocol.CauseGPRSEPSSubscriptionUnknown},
		"unknownSubscriber, extension, imsiUnknown": {mapprotocol.ErrUnknownSubscriber, "30 05 30 00 0A 01 00", mapprotocol.CauseIMSIUnknown},
		"unknownSubscriber, unknown value":          {mapprotocol.ErrUnknownSubscriber, "30 03 0A 01 7F", mapprotocol.CauseNone}, // discarded, as the type says
		"roamingNotAllowed, PLMN":                   {mapprotocol.ErrRoamingNotAllowed, "30 03 0A 01 00", mapprotocol.CausePLMNRoamingNotAllowed},
		"roamingNotAllowed, ODB":                    {mapprotocol.ErrRoamingNotAllowed, "30 03 0A 01 03", mapprotocol.CauseOperatorDeterminedBarring},
		"roamingNotAllowed, additional overrides":   {mapprotocol.ErrRoamingNotAllowed, "30 08 0A 01 03 30 00 80 01 00", mapprotocol.CauseSupportedRATTypesNotAllowed},
		"roamingNotAllowed, unknown additional":     {mapprotocol.ErrRoamingNotAllowed, "30 06 0A 01 03 80 01 05", mapprotocol.CauseOperatorDeterminedBarring},
		"unknownSubscriber, a later addition":       {mapprotocol.ErrUnknownSubscriber, "30 06 0A 01 01 81 01 07", mapprotocol.CauseGPRSEPSSubscriptionUnknown},
		"roamingNotAllowed, a later addition":       {mapprotocol.ErrRoamingNotAllowed, "30 09 0A 01 00 80 01 00 81 01 07", mapprotocol.CauseSupportedRATTypesNotAllowed},
		"roamingNotAllowed, an addition, no [0]":    {mapprotocol.ErrRoamingNotAllowed, "30 06 0A 01 03 81 01 07", mapprotocol.CauseOperatorDeterminedBarring},
		"systemFailure, its parameter not read":     {mapprotocol.ErrSystemFailure, "30 03 0A 01 00", mapprotocol.CauseNone},
	} {
		got, err := mapprotocol.DecodeUserErrorCause(c.code, hexBytes(t, c.param))
		require.NoError(t, err, name)
		assert.Equal(t, c.want, got, name)
	}
	for name, c := range map[string]struct {
		code  mapprotocol.ErrorCode
		param string
	}{
		"roamingNotAllowed without its parameter": {mapprotocol.ErrRoamingNotAllowed, ""},
		"roamingNotAllowed without its cause":     {mapprotocol.ErrRoamingNotAllowed, "30 00"},
		"roamingNotAllowed, cause 1":              {mapprotocol.ErrRoamingNotAllowed, "30 03 0A 01 01"},
		"unknownSubscriber, an empty ENUMERATED":  {mapprotocol.ErrUnknownSubscriber, "30 02 0A 00"},
		"roamingNotAllowed, an empty additional":  {mapprotocol.ErrRoamingNotAllowed, "30 05 0A 01 03 80 00"},
		"unknownSubscriber, not a SEQUENCE":       {mapprotocol.ErrUnknownSubscriber, "0A 01 00"},
	} {
		_, err := mapprotocol.DecodeUserErrorCause(c.code, hexBytes(t, c.param))
		assert.Error(t, err, name)
	}
}

// What Parameter encodes, mapprotocol.DecodeUserErrorCause reads back.
func TestUserErrorRoundTrip(t *testing.T) {
	for _, e := range []mapprotocol.UserError{
		{mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseNone}, {mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseIMSIUnknown},
		{mapprotocol.ErrUnknownSubscriber, mapprotocol.CauseGPRSEPSSubscriptionUnknown}, {mapprotocol.ErrRoamingNotAllowed, mapprotocol.CausePLMNRoamingNotAllowed},
		{mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseOperatorDeterminedBarring}, {mapprotocol.ErrRoamingNotAllowed, mapprotocol.CauseSupportedRATTypesNotAllowed},
	} {
		p, err := e.Parameter()
		require.NoError(t, err)
		got, err := mapprotocol.DecodeUserErrorCause(e.Code, p)
		require.NoError(t, err)
		assert.Equal(t, e.Cause, got, "%s", e.Code)
	}
}
