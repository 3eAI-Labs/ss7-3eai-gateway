// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Config API Handlers using NATS KV ConfigStore

// handleGetRoutes retrieves all routing rules
func (h *Handler) handleGetRoutes(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	rules, err := cs.GetRoutingRules(context.Background())
	if err != nil {
		h.logger.Error("Failed to get routing rules", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to get routing rules"})
		return
	}

	c.JSON(http.StatusOK, rules)
}

// handleAddRoute adds a new routing rule
func (h *Handler) handleAddRoute(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	var rule config.RoutingRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	// Generate ID if not provided
	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.AddRoutingRule(context.Background(), &rule); err != nil {
		h.logger.Error("Failed to add routing rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to add routing rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusCreated, SuccessResponse{
		Message: "routing rule added successfully",
		Data: map[string]interface{}{
			"id": rule.ID,
		},
	})
}

// handleDeleteRoute deletes a routing rule
func (h *Handler) handleDeleteRoute(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "missing route ID"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.DeleteRoutingRule(context.Background(), id); err != nil {
		h.logger.Error("Failed to delete routing rule", zap.Error(err), zap.String("id", id))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to delete routing rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusOK, SuccessResponse{
		Message: "routing rule deleted successfully",
		Data: map[string]interface{}{
			"id": id,
		},
	})
}

// handleGetGTTRules retrieves all GTT rules
func (h *Handler) handleGetGTTRules(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	rules, err := cs.GetGTTRules(context.Background())
	if err != nil {
		h.logger.Error("Failed to get GTT rules", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to get GTT rules"})
		return
	}

	c.JSON(http.StatusOK, rules)
}

// handleAddGTTRule adds a new GTT rule
func (h *Handler) handleAddGTTRule(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	var rule config.GTTRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.AddGTTRule(context.Background(), &rule); err != nil {
		h.logger.Error("Failed to add GTT rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to add GTT rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusCreated, SuccessResponse{
		Message: "GTT rule added successfully",
		Data: map[string]interface{}{
			"id": rule.ID,
		},
	})
}

// handleDeleteGTTRule deletes a GTT rule
func (h *Handler) handleDeleteGTTRule(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "missing GTT rule ID"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.DeleteGTTRule(context.Background(), id); err != nil {
		h.logger.Error("Failed to delete GTT rule", zap.Error(err), zap.String("id", id))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to delete GTT rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusOK, SuccessResponse{
		Message: "GTT rule deleted successfully",
		Data: map[string]interface{}{
			"id": id,
		},
	})
}

// handleGetFirewallRules retrieves all firewall rules
func (h *Handler) handleGetFirewallRules(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	rules, err := cs.GetFirewallRules(context.Background())
	if err != nil {
		h.logger.Error("Failed to get firewall rules", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to get firewall rules"})
		return
	}

	c.JSON(http.StatusOK, rules)
}

// handleAddFirewallRule adds a new firewall rule
func (h *Handler) handleAddFirewallRule(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	var rule config.FirewallRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.AddFirewallRule(context.Background(), &rule); err != nil {
		h.logger.Error("Failed to add firewall rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to add firewall rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusCreated, SuccessResponse{
		Message: "firewall rule added successfully",
		Data: map[string]interface{}{
			"id": rule.ID,
		},
	})
}

// handleDeleteFirewallRule deletes a firewall rule
func (h *Handler) handleDeleteFirewallRule(c *gin.Context) {
	if h.dependencies.ConfigStore == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "config store not available"})
		return
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "missing firewall rule ID"})
		return
	}

	cs := h.dependencies.ConfigStore.(*config.ConfigStore)
	if err := cs.DeleteFirewallRule(context.Background(), id); err != nil {
		h.logger.Error("Failed to delete firewall rule", zap.Error(err), zap.String("id", id))
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to delete firewall rule", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusOK, SuccessResponse{
		Message: "firewall rule deleted successfully",
		Data: map[string]interface{}{
			"id": id,
		},
	})
}
