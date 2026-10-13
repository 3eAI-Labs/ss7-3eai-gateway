// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// Setup test environment with minimal router (no auth, no metrics)
func setupTestAPI(t *testing.T) (*Handler, *config.ConfigStore, *gin.Engine) {
	t.Helper()

	// Start NATS server
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)

	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)

	t.Cleanup(func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	})

	// Create ConfigStore
	logger := zaptest.NewLogger(t)
	cs, err := config.NewConfigStore(nc, logger)
	require.NoError(t, err)

	// Create API handler
	deps := &Dependencies{
		ConfigStore: cs,
	}

	// Setup minimal Gin router for testing (skip auth/metrics)
	gin.SetMode(gin.TestMode)
	router := gin.New()

	handler := &Handler{
		dependencies: deps,
		logger:       logger,
		config:       &Config{BindAddr: "0.0.0.0:8080"},
		router:       router,
	}

	// Setup test routes (bypassing auth middleware)
	router.GET("/api/v1/routes", handler.handleGetRoutes)
	router.POST("/api/v1/routes", handler.handleAddRoute)
	router.DELETE("/api/v1/routes/:id", handler.handleDeleteRoute)
	router.GET("/api/v1/gtt/rules", handler.handleGetGTTRules)
	router.POST("/api/v1/gtt/rules", handler.handleAddGTTRule)
	router.DELETE("/api/v1/gtt/rules/:id", handler.handleDeleteGTTRule)
	router.GET("/api/v1/firewall/rules", handler.handleGetFirewallRules)
	router.POST("/api/v1/firewall/rules", handler.handleAddFirewallRule)
	router.DELETE("/api/v1/firewall/rules/:id", handler.handleDeleteFirewallRule)

	return handler, cs, router
}

func TestHandleGetRoutes_Empty(t *testing.T) {
	_, _, router := setupTestAPI(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/routes", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var routes []config.RoutingRule
	err := json.Unmarshal(w.Body.Bytes(), &routes)
	require.NoError(t, err)
	assert.Empty(t, routes)
}

func TestHandleAddRoute_Success(t *testing.T) {
	_, _, router := setupTestAPI(t)

	newRule := config.RoutingRule{
		Pattern:     "^1234.*",
		Destination: "10.5.0.100",
		Priority:    100,
		Enabled:     true,
	}

	body, _ := json.Marshal(newRule)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/routes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp SuccessResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "routing rule added successfully", resp.Message)
	assert.NotEmpty(t, resp.Data["id"])
}

func TestHandleAddRoute_InvalidJSON(t *testing.T) {
	_, _, router := setupTestAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/routes", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "invalid request", resp.Error)
}

func TestHandleGetRoutes_AfterAdd(t *testing.T) {
	_, cs, router := setupTestAPI(t)

	// Add a route directly via ConfigStore
	rule := &config.RoutingRule{
		ID:          "test-route-1",
		Pattern:     "^9999.*",
		Destination: "10.5.0.99",
		Priority:    50,
		Enabled:     true,
	}
	err := cs.AddRoutingRule(context.Background(), rule)
	require.NoError(t, err)

	// GET routes
	req := httptest.NewRequest(http.MethodGet, "/api/v1/routes", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var routes []config.RoutingRule
	err = json.Unmarshal(w.Body.Bytes(), &routes)
	require.NoError(t, err)
	assert.Len(t, routes, 1)
	assert.Equal(t, "test-route-1", routes[0].ID)
	assert.Equal(t, "^9999.*", routes[0].Pattern)
}

func TestHandleDeleteRoute_Success(t *testing.T) {
	_, cs, router := setupTestAPI(t)

	// Add a route first
	rule := &config.RoutingRule{
		ID:          "delete-me",
		Pattern:     "^8888.*",
		Destination: "10.5.0.88",
		Priority:    88,
		Enabled:     true,
	}
	err := cs.AddRoutingRule(context.Background(), rule)
	require.NoError(t, err)

	// DELETE route
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/routes/delete-me", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp SuccessResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "routing rule deleted successfully", resp.Message)

	// Verify deletion
	routes, _ := cs.GetRoutingRules(context.Background())
	assert.Empty(t, routes)
}

func TestHandleDeleteRoute_NotFound(t *testing.T) {
	_, _, router := setupTestAPI(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/routes/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var resp ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "failed to delete routing rule", resp.Error)
}

func TestHandleGTTRules_CRUD(t *testing.T) {
	_, _, router := setupTestAPI(t)

	// Test: GET empty GTT rules
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gtt/rules", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Test: POST new GTT rule
	newRule := config.GTTRule{
		Input:       "1234567890",
		Output:      "0987654321",
		Translation: "swap",
		Priority:    100,
		Enabled:     true,
	}
	body, _ := json.Marshal(newRule)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/gtt/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var addResp SuccessResponse
	json.Unmarshal(w.Body.Bytes(), &addResp)
	ruleID := addResp.Data["id"].(string)

	// Test: GET GTT rules after add
	req = httptest.NewRequest(http.MethodGet, "/api/v1/gtt/rules", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var rules []config.GTTRule
	json.Unmarshal(w.Body.Bytes(), &rules)
	assert.Len(t, rules, 1)
	assert.Equal(t, "1234567890", rules[0].Input)

	// Test: DELETE GTT rule
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/gtt/rules/"+ruleID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHandleFirewallRules_CRUD(t *testing.T) {
	_, _, router := setupTestAPI(t)

	// Test: GET empty firewall rules
	req := httptest.NewRequest(http.MethodGet, "/api/v1/firewall/rules", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Test: POST new firewall rule
	newRule := config.FirewallRule{
		Type:     "blacklist",
		Pattern:  "^666.*",
		Action:   "block",
		Priority: 100,
		Enabled:  true,
	}
	body, _ := json.Marshal(newRule)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/firewall/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var addResp SuccessResponse
	json.Unmarshal(w.Body.Bytes(), &addResp)
	ruleID := addResp.Data["id"].(string)

	// Test: GET firewall rules after add
	req = httptest.NewRequest(http.MethodGet, "/api/v1/firewall/rules", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var rules []config.FirewallRule
	json.Unmarshal(w.Body.Bytes(), &rules)
	assert.Len(t, rules, 1)
	assert.Equal(t, "block", rules[0].Action)

	// Test: DELETE firewall rule
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/firewall/rules/"+ruleID, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestConfigStore_Nil(t *testing.T) {
	// Test handlers when ConfigStore is nil
	logger := zaptest.NewLogger(t)
	deps := &Dependencies{
		ConfigStore: nil, // Intentionally nil
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()

	handler := &Handler{
		dependencies: deps,
		logger:       logger,
		config:       &Config{BindAddr: "0.0.0.0:8080"},
		router:       router,
	}

	// Setup test route
	router.GET("/api/v1/routes", handler.handleGetRoutes)

	// Test GET routes with nil ConfigStore
	req := httptest.NewRequest(http.MethodGet, "/api/v1/routes", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "config store not available", resp.Error)
}
