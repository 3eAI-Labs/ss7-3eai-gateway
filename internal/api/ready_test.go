// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readyStatus(t *testing.T, deps *Dependencies) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &Handler{dependencies: deps}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil)
	h.handleReady(c)
	return w.Code
}

// Without a live SGW association the node cannot carry a dialogue, so it is
// not ready; it is not killed either (liveness is separate).
func TestReady_FollowsSignallingLink(t *testing.T) {
	m3uaH, err := m3ua.NewHandler(&m3ua.Config{
		SGWAddress: "127.0.0.1", SGWPort: 2905, TrafficMode: 2,
		NetworkIndicator: 2, ASPUPACKTimeout: time.Second,
	}, nil)
	require.NoError(t, err)

	assert.Equal(t, http.StatusServiceUnavailable, readyStatus(t, &Dependencies{M3UAHandler: m3uaH}))
	assert.Equal(t, http.StatusOK, readyStatus(t, &Dependencies{}), "no M3UA wired (API-only test setups)")
}
