// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/cap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/firewall"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/ha"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/inap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tdr"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Role represents user role for RBAC
type Role string

const (
	RoleAdmin    Role = "admin"    // Full access
	RoleOperator Role = "operator" // Read + limited write (no delete, no security changes)
	RoleReadOnly Role = "readonly" // Read-only access
	RoleMonitor  Role = "monitor"  // Only metrics and health endpoints
)

// Permission represents a specific permission
type Permission string

const (
	// General
	PermViewHealth  Permission = "view:health"
	PermViewMetrics Permission = "view:metrics"

	// Configuration
	PermViewConfig   Permission = "view:config"
	PermUpdateConfig Permission = "update:config"

	// Routes
	PermViewRoutes   Permission = "view:routes"
	PermUpdateRoutes Permission = "update:routes"
	PermDeleteRoutes Permission = "delete:routes"

	// GTT Rules
	PermViewGTTRules   Permission = "view:gtt_rules"
	PermUpdateGTTRules Permission = "update:gtt_rules"
	PermDeleteGTTRules Permission = "delete:gtt_rules"

	// Firewall
	PermViewFirewall   Permission = "view:firewall"
	PermUpdateFirewall Permission = "update:firewall"
	PermDeleteFirewall Permission = "delete:firewall"

	// HA
	PermViewHA   Permission = "view:ha"
	PermManageHA Permission = "manage:ha"

	// TDR
	PermViewTDR   Permission = "view:tdr"
	PermSearchTDR Permission = "search:tdr"

	// Users
	PermViewUsers   Permission = "view:users"
	PermManageUsers Permission = "manage:users"
)

// RolePermissions defines permissions for each role
var RolePermissions = map[Role][]Permission{
	RoleAdmin: {
		PermViewHealth, PermViewMetrics, PermViewConfig, PermUpdateConfig,
		PermViewRoutes, PermUpdateRoutes, PermDeleteRoutes,
		PermViewGTTRules, PermUpdateGTTRules, PermDeleteGTTRules,
		PermViewFirewall, PermUpdateFirewall, PermDeleteFirewall,
		PermViewHA, PermManageHA,
		PermViewTDR, PermSearchTDR,
		PermViewUsers, PermManageUsers,
	},
	RoleOperator: {
		PermViewHealth, PermViewMetrics, PermViewConfig,
		PermViewRoutes, PermUpdateRoutes,
		PermViewGTTRules, PermUpdateGTTRules,
		PermViewFirewall, PermUpdateFirewall,
		PermViewHA,
		PermViewTDR, PermSearchTDR,
	},
	RoleReadOnly: {
		PermViewHealth, PermViewMetrics, PermViewConfig,
		PermViewRoutes, PermViewGTTRules, PermViewFirewall,
		PermViewHA, PermViewTDR,
	},
	RoleMonitor: {
		PermViewHealth, PermViewMetrics,
	},
}

// User represents an API user
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email,omitempty"`
	Role         Role      `json:"role"`
	Enabled      bool      `json:"enabled"`
	PasswordHash string    `json:"-"` // Never expose in JSON
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastLoginAt  time.Time `json:"last_login_at,omitempty"`
}

// JWTClaims represents JWT claims
type JWTClaims struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Role      Role   `json:"role"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// AuthRequest represents login request
type AuthRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse represents login response
type AuthResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      *User     `json:"user"`
}

// Handler manages REST API operations
type Handler struct {
	config *Config

	// Gin router
	router *gin.Engine

	// User management
	users   map[string]*User
	usersMu sync.RWMutex

	// JWT secret
	jwtSecret []byte

	// Dependencies (injected)
	dependencies *Dependencies

	// Metrics
	metrics *Metrics
	logger  *zap.Logger
}

// Dependencies holds injected dependencies (using concrete types)
type Dependencies struct {
	HAHandler       *ha.Handler
	M3UAHandler     *m3ua.Handler
	SCCPHandler     *sccp.Handler
	TCAPHandler     *tcap.Handler
	MAPHandler      *mapprotocol.Handler
	CAPHandler      *cap.Handler
	INAPHandler     *inap.Handler
	FirewallHandler *firewall.Handler
	TDRHandler      *tdr.Handler
	NATSHandler     *natsbridge.Handler
	ConfigStore     interface{}   // NATS KV-backed config store
	DrainProvider   DrainProvider // graceful-drain state for the readiness probe
}

// DrainProvider reports whether the node is draining (set by the opsTrigger
// `drain` action). Satisfied by *opstrigger.DrainState; kept as a local
// interface so the api package takes no dependency on opstrigger.
type DrainProvider interface {
	Draining() bool
}

// Config holds API configuration
type Config struct {
	// Server
	BindAddr    string `yaml:"bind_addr" json:"bind_addr"` // e.g., "0.0.0.0:8080"
	TLSEnabled  bool   `yaml:"tls_enabled" json:"tls_enabled"`
	TLSCertFile string `yaml:"tls_cert_file" json:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file" json:"tls_key_file"`

	// JWT
	JWTSecret     string        `yaml:"jwt_secret" json:"-"`
	JWTExpiration time.Duration `yaml:"jwt_expiration" json:"jwt_expiration"`

	// CORS
	CORSEnabled bool     `yaml:"cors_enabled" json:"cors_enabled"`
	CORSOrigins []string `yaml:"cors_origins" json:"cors_origins"`

	// Rate Limiting
	RateLimitEnabled bool `yaml:"rate_limit_enabled" json:"rate_limit_enabled"`
	RateLimitRPS     int  `yaml:"rate_limit_rps" json:"rate_limit_rps"`

	// Timeouts
	ReadTimeout  time.Duration `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout" json:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout" json:"idle_timeout"`

	// Security
	EnableSwagger  bool     `yaml:"enable_swagger" json:"enable_swagger"`
	TrustedProxies []string `yaml:"trusted_proxies" json:"trusted_proxies"`

	// Default admin user (for bootstrap)
	DefaultAdminUser     string `yaml:"default_admin_user" json:"default_admin_user"`
	DefaultAdminPassword string `yaml:"default_admin_password" json:"-"`

	// DevMode loosens production safety checks (placeholder secrets,
	// missing bootstrap admin password). Must be false for any
	// production deployment.
	DevMode bool `yaml:"dev_mode" json:"dev_mode"`
}

// Metrics tracks API metrics
type Metrics struct {
	// Requests
	TotalRequests      atomic.Uint64
	SuccessfulRequests atomic.Uint64
	FailedRequests     atomic.Uint64

	// By status code
	Status2xx atomic.Uint64
	Status3xx atomic.Uint64
	Status4xx atomic.Uint64
	Status5xx atomic.Uint64

	// Authentication
	LoginAttempts       atomic.Uint64
	LoginSuccesses      atomic.Uint64
	LoginFailures       atomic.Uint64
	AuthorizationDenied atomic.Uint64

	// Performance
	RequestLatencySum   atomic.Uint64 // Nanoseconds
	RequestLatencyCount atomic.Uint64

	// Rate limiting
	RateLimitHits atomic.Uint64

	mu         sync.RWMutex
	LastUpdate time.Time
}

// HealthResponse represents health check response
type HealthResponse struct {
	Status     string                 `json:"status"` // "healthy", "degraded", "unhealthy"
	NodeID     string                 `json:"node_id"`
	Version    string                 `json:"version"`
	Uptime     time.Duration          `json:"uptime"`
	Components map[string]interface{} `json:"components"`
	Timestamp  time.Time              `json:"timestamp"`
}

// MetricsResponse represents aggregated metrics response
type MetricsResponse struct {
	M3UA     interface{} `json:"m3ua"`
	SCCP     interface{} `json:"sccp"`
	TCAP     interface{} `json:"tcap"`
	MAP      interface{} `json:"map"`
	CAP      interface{} `json:"cap"`
	INAP     interface{} `json:"inap"`
	Firewall interface{} `json:"firewall"`
	TDR      interface{} `json:"tdr"`
	NATS     interface{} `json:"nats"`
	HA       interface{} `json:"ha"`
	API      interface{} `json:"api"`
}

// ErrorResponse represents error response
type ErrorResponse struct {
	Error   string                 `json:"error"`
	Code    string                 `json:"code,omitempty"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// SuccessResponse represents success response
type SuccessResponse struct {
	Message string                 `json:"message"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

// AddPeerRequest represents add peer request
type AddPeerRequest struct {
	NodeID string `json:"node_id" binding:"required"`
	Addr   string `json:"addr" binding:"required"`
}
