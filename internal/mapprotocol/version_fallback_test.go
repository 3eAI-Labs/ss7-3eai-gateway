// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package mapprotocol

import (
	"testing"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionFallback(t *testing.T) {
	ac, op, ok := VersionFallback(tcap.ACShortMsgGatewayV3.OID, int32(OpSendRoutingInfoForSM), tcap.ACShortMsgGatewayV2.OID)
	require.True(t, ok)
	assert.Equal(t, tcap.ACShortMsgGatewayV2.OID, ac.OID)
	assert.Equal(t, int32(OpSendRoutingInfoForSM), op)

	ac, op, ok = VersionFallback(tcap.ACShortMsgMTRelayV3.OID, int32(OpMtForwardSM), tcap.ACShortMsgMTRelayV2.OID)
	require.True(t, ok)
	assert.Equal(t, tcap.ACShortMsgMTRelayV2.OID, ac.OID)
	assert.Equal(t, int32(46), op, "MT delivery is forwardSM in version 2")

	_, _, ok = VersionFallback(tcap.ACShortMsgMTRelayV3.OID, int32(OpMtForwardSM), []int{0, 4, 0, 0, 1, 0, 25, 1})
	assert.False(t, ok, "no version 1")
	_, _, ok = VersionFallback(tcap.ACShortMsgGatewayV3.OID, int32(OpMtForwardSM), tcap.ACShortMsgGatewayV2.OID)
	assert.False(t, ok, "operation does not belong to the context")
}

// mt-ForwardSM's result is optional, and v2 forwardSM has none.
func TestDecodeMtForwardSMRes_Empty(t *testing.T) {
	r, err := decodeMtForwardSMRes(nil)
	require.NoError(t, err)
	assert.NotNil(t, r)
}
