// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

const (
	// MinSecretLength is the minimum length (in bytes) for sensitive
	// secrets such as JWT signing keys. Configs below this are rejected
	// at startup. Aligns with config.example.yaml's "min 32 chars" hint.
	MinSecretLength = 32
)

// placeholderSecretPrefixes lists prefixes that obviously identify
// example/template secret values shipped in config.example.yaml. A
// production deploy must rotate these before startup.
var placeholderSecretPrefixes = []string{
	"CHANGE_THIS",
	"REPLACE_ME",
	"PLACEHOLDER",
	"EXAMPLE_",
}

const (
	DefaultBindAddr      = "0.0.0.0:8080"
	DefaultJWTExpiration = 24 * time.Hour
	DefaultReadTimeout   = 15 * time.Second
	DefaultWriteTimeout  = 15 * time.Second
	DefaultIdleTimeout   = 60 * time.Second
	DefaultRateLimitRPS  = 100
)

// Version is the gateway build version reported by /api/v1/health; cmd/gateway sets it from
// its build-time version.
var Version = "dev"

// NewHandler creates a new API handler
func NewHandler(cfg *Config, deps *Dependencies, logger *zap.Logger) (*Handler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if deps == nil {
		return nil, fmt.Errorf("dependencies cannot be nil")
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	// Set Gin mode
	gin.SetMode(gin.ReleaseMode)

	handler := &Handler{
		config:       cfg,
		router:       gin.New(),
		users:        make(map[string]*User),
		jwtSecret:    []byte(cfg.JWTSecret),
		dependencies: deps,
		metrics:      &Metrics{},
		logger:       logger,
	}

	// Create default admin user
	if err := handler.createDefaultAdmin(); err != nil {
		return nil, fmt.Errorf("failed to create default admin: %w", err)
	}

	// Setup routes
	handler.setupRoutes()

	return handler, nil
}

// Start starts the API server
func (h *Handler) Start(ctx context.Context) error {
	h.logger.Info("Starting REST API Gateway",
		zap.String("bind_addr", h.config.BindAddr),
		zap.Bool("tls_enabled", h.config.TLSEnabled))

	srv := &http.Server{
		Addr:         h.config.BindAddr,
		Handler:      h.router,
		ReadTimeout:  h.config.ReadTimeout,
		WriteTimeout: h.config.WriteTimeout,
		IdleTimeout:  h.config.IdleTimeout,
	}

	// Start server in background
	go func() {
		var err error
		if h.config.TLSEnabled {
			err = srv.ListenAndServeTLS(h.config.TLSCertFile, h.config.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			h.logger.Error("API server error", zap.Error(err))
		}
	}()

	h.logger.Info("REST API Gateway started successfully")
	return nil
}

// Stop gracefully stops the API server
func (h *Handler) Stop(ctx context.Context) error {
	h.logger.Info("Stopping REST API Gateway")
	return nil
}

// UpdateConfigStore updates the ConfigStore dependency after initialization
func (h *Handler) UpdateConfigStore(configStore interface{}) {
	h.dependencies.ConfigStore = configStore
	h.logger.Info("ConfigStore updated in API handler")
}

// setupRoutes configures all API routes
func (h *Handler) setupRoutes() {
	// Middleware
	h.router.Use(gin.Recovery())
	h.router.Use(h.loggingMiddleware())
	h.router.Use(h.metricsMiddleware())

	// CORS
	if h.config.CORSEnabled {
		h.router.Use(h.corsMiddleware())
	}

	// Public routes (no auth)
	public := h.router.Group("/api/v1")
	{
		public.POST("/auth/login", h.handleLogin)
		public.GET("/health", h.handleHealth)
		public.GET("/ready", h.handleReady)
	}

	// Protected routes (require auth)
	protected := h.router.Group("/api/v1")
	protected.Use(h.authMiddleware())
	{
		// Metrics (all authenticated users)
		protected.GET("/metrics", h.requirePermission(PermViewMetrics), h.handleMetrics)

		// Configuration
		protected.GET("/config", h.requirePermission(PermViewConfig), h.handleGetConfig)
		protected.PUT("/config", h.requirePermission(PermUpdateConfig), h.handleUpdateConfig)

		// Routes
		protected.GET("/routes", h.requirePermission(PermViewRoutes), h.handleGetRoutes)
		protected.POST("/routes", h.requirePermission(PermUpdateRoutes), h.handleAddRoute)
		protected.DELETE("/routes/:id", h.requirePermission(PermDeleteRoutes), h.handleDeleteRoute)

		// GTT Rules
		protected.GET("/gtt/rules", h.requirePermission(PermViewGTTRules), h.handleGetGTTRules)
		protected.POST("/gtt/rules", h.requirePermission(PermUpdateGTTRules), h.handleAddGTTRule)
		protected.DELETE("/gtt/rules/:id", h.requirePermission(PermDeleteGTTRules), h.handleDeleteGTTRule)

		// Firewall
		protected.GET("/firewall/rules", h.requirePermission(PermViewFirewall), h.handleGetFirewallRules)
		protected.POST("/firewall/rules", h.requirePermission(PermUpdateFirewall), h.handleAddFirewallRule)
		protected.DELETE("/firewall/rules/:id", h.requirePermission(PermDeleteFirewall), h.handleDeleteFirewallRule)

		// HA Cluster
		protected.GET("/ha/status", h.requirePermission(PermViewHA), h.handleGetHAStatus)
		protected.POST("/ha/peers", h.requirePermission(PermManageHA), h.handleAddPeer)
		protected.DELETE("/ha/peers/:id", h.requirePermission(PermManageHA), h.handleRemovePeer)

		// TDR
		protected.GET("/tdr/status", h.requirePermission(PermViewTDR), h.handleGetTDRStatus)
		protected.POST("/tdr/search", h.requirePermission(PermSearchTDR), h.handleSearchTDR)

		// Users (admin only)
		protected.GET("/users", h.requirePermission(PermViewUsers), h.handleGetUsers)
		protected.POST("/users", h.requirePermission(PermManageUsers), h.handleCreateUser)
		protected.DELETE("/users/:id", h.requirePermission(PermManageUsers), h.handleDeleteUser)
	}

	h.logger.Info("API routes configured")
}

// Middleware

func (h *Handler) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		h.logger.Info("API request",
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.Int("status", statusCode),
			zap.Duration("latency", latency),
			zap.String("client_ip", c.ClientIP()))
	}
}

func (h *Handler) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		// Update metrics
		h.metrics.TotalRequests.Add(1)

		if statusCode >= 200 && statusCode < 300 {
			h.metrics.SuccessfulRequests.Add(1)
			h.metrics.Status2xx.Add(1)
		} else {
			h.metrics.FailedRequests.Add(1)
			if statusCode >= 300 && statusCode < 400 {
				h.metrics.Status3xx.Add(1)
			} else if statusCode >= 400 && statusCode < 500 {
				h.metrics.Status4xx.Add(1)
			} else if statusCode >= 500 {
				h.metrics.Status5xx.Add(1)
			}
		}

		h.metrics.RequestLatencySum.Add(uint64(latency.Nanoseconds()))
		h.metrics.RequestLatencyCount.Add(1)
	}
}

// corsMiddleware sets CORS headers per CORS spec.
//
// The CORS spec forbids `Access-Control-Allow-Origin: *` in combination
// with `Access-Control-Allow-Credentials: true`; browsers reject such
// responses. This middleware echoes the request's Origin only when it
// matches an entry in cfg.CORSOrigins, and emits Allow-Credentials only
// for matched origins. A literal `*` in CORSOrigins is honored only
// when credentials are not required (and we still echo the request
// origin instead of `*` so headers stay self-consistent).
func (h *Handler) corsMiddleware() gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(h.config.CORSOrigins))
	wildcard := false
	for _, o := range h.config.CORSOrigins {
		o = strings.TrimSpace(o)
		if o == "*" {
			wildcard = true
			continue
		}
		if o != "" {
			allowed[o] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		_, exact := allowed[origin]
		if origin != "" && (exact || wildcard) {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			if exact {
				// Credentials only flow back to explicitly-allowed origins,
				// never to wildcard matches — that combination is forbidden
				// by the CORS spec and rejected by browsers.
				c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func (h *Handler) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := c.GetHeader("Authorization")
		if tokenString == "" {
			c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "missing authorization token"})
			c.Abort()
			return
		}

		// Remove "Bearer " prefix
		if len(tokenString) > 7 && tokenString[:7] == "Bearer " {
			tokenString = tokenString[7:]
		}

		// Parse JWT
		claims, err := h.parseJWT(tokenString)
		if err != nil {
			c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid token"})
			c.Abort()
			return
		}

		// Check expiration
		if claims.ExpiresAt < time.Now().Unix() {
			c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "token expired"})
			c.Abort()
			return
		}

		// Get user
		h.usersMu.RLock()
		user, exists := h.users[claims.UserID]
		h.usersMu.RUnlock()

		if !exists || !user.Enabled {
			c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found or disabled"})
			c.Abort()
			return
		}

		// Store user in context
		c.Set("user", user)
		c.Set("claims", claims)

		c.Next()
	}
}

func (h *Handler) requirePermission(perm Permission) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, exists := c.Get("user")
		if !exists {
			c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
			c.Abort()
			return
		}

		u := user.(*User)

		// Check permission
		if !h.hasPermission(u.Role, perm) {
			h.metrics.AuthorizationDenied.Add(1)
			c.JSON(http.StatusForbidden, ErrorResponse{Error: "insufficient permissions"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// Handlers

func (h *Handler) handleLogin(c *gin.Context) {
	var req AuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request"})
		return
	}

	h.metrics.LoginAttempts.Add(1)

	// Find user
	h.usersMu.RLock()
	var user *User
	for _, u := range h.users {
		if u.Username == req.Username {
			user = u
			break
		}
	}
	h.usersMu.RUnlock()

	if user == nil {
		h.metrics.LoginFailures.Add(1)
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid credentials"})
		return
	}

	// Check password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		h.metrics.LoginFailures.Add(1)
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid credentials"})
		return
	}

	// Check if enabled
	if !user.Enabled {
		h.metrics.LoginFailures.Add(1)
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user disabled"})
		return
	}

	// Generate JWT
	token, expiresAt, err := h.generateJWT(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate token"})
		return
	}

	// Update last login
	h.usersMu.Lock()
	user.LastLoginAt = time.Now()
	h.usersMu.Unlock()

	h.metrics.LoginSuccesses.Add(1)

	c.JSON(http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      user,
	})
}

// handleReady is the readiness probe. It returns 503 while the node is draining
// (opsTrigger `drain`) so Kubernetes removes the pod from Service endpoints and
// Arinna sees it quiescing — while liveness (/health) stays up so the pod is not
// killed and in-flight dialogues finish. It also returns 503 while the M3UA
// association to the SGW is down: the node cannot carry a dialogue then, and
// the M3UA supervisor keeps re-dialling, so the pod stays alive and becomes
// ready when the link does.
func (h *Handler) handleReady(c *gin.Context) {
	if h.dependencies.DrainProvider != nil && h.dependencies.DrainProvider.Draining() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
		return
	}
	if m := h.dependencies.M3UAHandler; m != nil && !m.IsActive() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "signalling_link_down"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

func (h *Handler) handleHealth(c *gin.Context) {
	components := make(map[string]interface{})

	if h.dependencies.M3UAHandler != nil {
		components["m3ua"] = h.dependencies.M3UAHandler.GetHealthStatus()
	}
	if h.dependencies.SCCPHandler != nil {
		components["sccp"] = h.dependencies.SCCPHandler.GetHealthStatus()
	}
	if h.dependencies.TCAPHandler != nil {
		components["tcap"] = h.dependencies.TCAPHandler.GetHealthStatus()
	}
	if h.dependencies.MAPHandler != nil {
		components["map"] = h.dependencies.MAPHandler.GetHealthStatus()
	}
	if h.dependencies.CAPHandler != nil {
		components["cap"] = h.dependencies.CAPHandler.GetHealthStatus()
	}
	if h.dependencies.INAPHandler != nil {
		components["inap"] = h.dependencies.INAPHandler.GetHealthStatus()
	}
	if h.dependencies.FirewallHandler != nil {
		components["firewall"] = h.dependencies.FirewallHandler.GetHealthStatus()
	}
	if h.dependencies.TDRHandler != nil {
		components["tdr"] = h.dependencies.TDRHandler.GetHealthStatus()
	}
	if h.dependencies.NATSHandler != nil {
		components["nats"] = h.dependencies.NATSHandler.GetHealthStatus()
	}
	if h.dependencies.HAHandler != nil {
		components["ha"] = h.dependencies.HAHandler.GetHealthStatus()
	}

	nodeID := "ss7-3eai-gateway"
	if h.dependencies.HAHandler != nil {
		if id := h.dependencies.HAHandler.GetNodeID(); id != "" {
			nodeID = id
		}
	}

	c.JSON(http.StatusOK, HealthResponse{
		Status:     "healthy",
		NodeID:     nodeID,
		Version:    Version,
		Components: components,
		Timestamp:  time.Now(),
	})
}

func (h *Handler) handleMetrics(c *gin.Context) {
	response := MetricsResponse{
		API: h.metrics,
	}

	if h.dependencies.M3UAHandler != nil {
		response.M3UA = h.dependencies.M3UAHandler.GetMetrics()
	}
	if h.dependencies.SCCPHandler != nil {
		response.SCCP = h.dependencies.SCCPHandler.GetMetrics()
	}
	if h.dependencies.TCAPHandler != nil {
		response.TCAP = h.dependencies.TCAPHandler.GetMetrics()
	}
	if h.dependencies.MAPHandler != nil {
		response.MAP = h.dependencies.MAPHandler.GetMetrics()
	}
	if h.dependencies.CAPHandler != nil {
		response.CAP = h.dependencies.CAPHandler.GetMetrics()
	}
	if h.dependencies.INAPHandler != nil {
		response.INAP = h.dependencies.INAPHandler.GetMetrics()
	}
	if h.dependencies.FirewallHandler != nil {
		response.Firewall = h.dependencies.FirewallHandler.GetMetrics()
	}
	if h.dependencies.TDRHandler != nil {
		response.TDR = h.dependencies.TDRHandler.GetMetrics()
	}
	if h.dependencies.NATSHandler != nil {
		response.NATS = h.dependencies.NATSHandler.GetMetrics()
	}
	if h.dependencies.HAHandler != nil {
		response.HA = h.dependencies.HAHandler.GetHealthStatus()
	}

	c.JSON(http.StatusOK, response)
}

func (h *Handler) handleGetConfig(c *gin.Context) {
	if h.dependencies.HAHandler == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "HA not available"})
		return
	}

	state := h.dependencies.HAHandler.GetClusterState()
	c.JSON(http.StatusOK, state)
}

func (h *Handler) handleUpdateConfig(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

// Config handlers moved to config_handlers.go

func (h *Handler) handleGetHAStatus(c *gin.Context) {
	if h.dependencies.HAHandler == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "HA not available"})
		return
	}

	status := h.dependencies.HAHandler.GetHealthStatus()
	c.JSON(http.StatusOK, status)
}

func (h *Handler) handleAddPeer(c *gin.Context) {
	if h.dependencies.HAHandler == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "HA not available"})
		return
	}

	var req AddPeerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	// Add voter to cluster
	if err := h.dependencies.HAHandler.AddVoter(req.NodeID, req.Addr); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to add peer", Details: map[string]interface{}{"error": err.Error()}})
		return
	}

	c.JSON(http.StatusOK, SuccessResponse{
		Message: "peer added successfully",
		Data: map[string]interface{}{
			"node_id": req.NodeID,
			"addr":    req.Addr,
		},
	})
}

func (h *Handler) handleRemovePeer(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

func (h *Handler) handleGetTDRStatus(c *gin.Context) {
	if h.dependencies.TDRHandler == nil {
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "TDR not available"})
		return
	}

	status := h.dependencies.TDRHandler.GetHealthStatus()
	c.JSON(http.StatusOK, status)
}

func (h *Handler) handleSearchTDR(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

func (h *Handler) handleGetUsers(c *gin.Context) {
	h.usersMu.RLock()
	users := make([]*User, 0, len(h.users))
	for _, u := range h.users {
		users = append(users, u)
	}
	h.usersMu.RUnlock()

	c.JSON(http.StatusOK, users)
}

func (h *Handler) handleCreateUser(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

func (h *Handler) handleDeleteUser(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, ErrorResponse{Error: "not implemented"})
}

// Helper functions

// createDefaultAdmin bootstraps the initial admin user.
//
// Behaviour matrix:
//   - Both username and password set     → use them.
//   - Username set, password empty       → generate a random password
//     and log it once at INFO; admin can rotate via POST /api/v1/users.
//   - Both empty, DevMode=true           → generate username "admin" +
//     random password, log both once.
//   - Both empty, DevMode=false (prod)   → return error. Booting with
//     no users wedges the system because POST /users is currently 501
//     and there is no recovery path.
func (h *Handler) createDefaultAdmin() error {
	username := h.config.DefaultAdminUser
	password := h.config.DefaultAdminPassword
	generated := false

	if username == "" && password == "" {
		if !h.config.DevMode {
			return fmt.Errorf("bootstrap admin not configured (default_admin_user / default_admin_password) — required when dev_mode=false; the system would otherwise boot with zero users and POST /users is not yet implemented")
		}
		username = "admin"
	}

	if password == "" {
		p, err := generateRandomSecret(24)
		if err != nil {
			return fmt.Errorf("failed to generate random admin password: %w", err)
		}
		password = p
		generated = true
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	adminUser := &User{
		ID:           uuid.New().String(),
		Username:     username,
		Role:         RoleAdmin,
		Enabled:      true,
		PasswordHash: string(passwordHash),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	h.users[adminUser.ID] = adminUser

	if generated {
		// Logged once at startup; operator must rotate via API.
		h.logger.Warn("Default admin user created with auto-generated password — rotate via POST /api/v1/users",
			zap.String("username", username),
			zap.String("password", password))
	} else {
		h.logger.Info("Default admin user created", zap.String("username", username))
	}
	return nil
}

// generateRandomSecret returns a base64-encoded cryptographically
// random secret of the given byte length. The returned string is
// longer than n (base64 encoding inflation) but contains exactly n
// bytes of entropy.
func generateRandomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *Handler) generateJWT(user *User) (string, time.Time, error) {
	expiresAt := time.Now().Add(h.config.JWTExpiration)

	claims := jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     user.Role,
		"iat":      time.Now().Unix(),
		"exp":      expiresAt.Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(h.jwtSecret)
	if err != nil {
		return "", time.Time{}, err
	}

	return tokenString, expiresAt, nil
}

func (h *Handler) parseJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			h.logger.Warn("Unexpected signing method", zap.String("method", fmt.Sprintf("%v", token.Method)))
			return nil, fmt.Errorf("unexpected signing method")
		}
		return h.jwtSecret, nil
	})

	if err != nil {
		h.logger.Warn("JWT parse error", zap.Error(err))
		return nil, err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return &JWTClaims{
			UserID:    claims["user_id"].(string),
			Username:  claims["username"].(string),
			Role:      Role(claims["role"].(string)),
			IssuedAt:  int64(claims["iat"].(float64)),
			ExpiresAt: int64(claims["exp"].(float64)),
		}, nil
	}

	h.logger.Warn("Invalid token claims or token not valid", zap.Bool("valid", token.Valid), zap.Any("claims", token.Claims))
	return nil, fmt.Errorf("invalid token")
}

func (h *Handler) hasPermission(role Role, perm Permission) bool {
	permissions, exists := RolePermissions[role]
	if !exists {
		return false
	}

	for _, p := range permissions {
		if p == perm {
			return true
		}
	}

	return false
}

func (h *Handler) hashPassword(password string) string {
	hash := sha256.Sum256([]byte(password))
	return hex.EncodeToString(hash[:])
}

func validateConfig(cfg *Config) error {
	if cfg.BindAddr == "" {
		cfg.BindAddr = DefaultBindAddr
	}
	if err := validateSecret("api.jwt_secret", cfg.JWTSecret, cfg.DevMode); err != nil {
		return err
	}
	if cfg.JWTExpiration == 0 {
		cfg.JWTExpiration = DefaultJWTExpiration
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.RateLimitRPS == 0 {
		cfg.RateLimitRPS = DefaultRateLimitRPS
	}
	if cfg.CORSEnabled {
		for _, o := range cfg.CORSOrigins {
			if strings.TrimSpace(o) == "*" && !cfg.DevMode {
				return fmt.Errorf("api.cors_origins: wildcard '*' is not allowed when cors_enabled=true and dev_mode=false; list explicit origins instead")
			}
		}
	}

	return nil
}

// ValidateSecret is the exported validator other packages (e.g. tdr)
// can call to enforce the same policy on secret-holding config fields.
func ValidateSecret(field, value string, devMode bool) error {
	return validateSecret(field, value, devMode)
}

func validateSecret(field, value string, devMode bool) error {
	if value == "" {
		return fmt.Errorf("%s: required", field)
	}
	for _, prefix := range placeholderSecretPrefixes {
		if strings.HasPrefix(value, prefix) {
			if devMode {
				// Allowed for local dev convenience but never silently in prod.
				return nil
			}
			return fmt.Errorf("%s: value starts with placeholder prefix %q — rotate before deploying with dev_mode=false", field, prefix)
		}
	}
	if len(value) < MinSecretLength {
		if devMode {
			return nil
		}
		return fmt.Errorf("%s: length %d is below the %d-byte minimum required when dev_mode=false", field, len(value), MinSecretLength)
	}
	return nil
}
