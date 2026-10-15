// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/api"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/cap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/config"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/firewall"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/ha"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/inap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/m3ua"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/mapprotocol"
	natsbridge "github.com/3eAI-Labs/ss7-3eai-gateway/internal/nats"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/opstrigger"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/sccp"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tcap"
	"github.com/3eAI-Labs/ss7-3eai-gateway/internal/tdr"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const AppName = "ss7-3eai-gateway"

// AppVersion is set at build time (-ldflags "-X main.AppVersion=…"; the release workflow
// passes the tag). A const cannot be set that way.
var AppVersion = "dev"

var (
	configFile     = flag.String("config", "config.yaml", "Path to configuration file")
	logLevel       = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	opstriggerFile = flag.String("opstrigger-file", "", "Path to the opsTrigger token file (default: <config dir>/opstrigger.yaml)")
)

func main() {
	flag.Parse()

	// Initialize logger
	logger, err := initLogger(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("Starting SS7 3EAI Gateway",
		zap.String("version", AppVersion),
		zap.String("config", *configFile))

	// Load configuration
	cfg, err := loadConfig(*configFile)
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize all handlers
	handlers, err := initializeHandlers(cfg, logger)
	if err != nil {
		logger.Fatal("Failed to initialize handlers", zap.Error(err))
	}

	// Start all services
	if err := startServices(ctx, handlers, logger); err != nil {
		logger.Fatal("Failed to start services", zap.Error(err))
	}

	logger.Info("SS7 3EAI Gateway started successfully")

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	logger.Info("Shutdown signal received, gracefully stopping services...")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := stopServices(shutdownCtx, handlers, logger); err != nil {
		logger.Error("Error during shutdown", zap.Error(err))
		os.Exit(1)
	}

	logger.Info("SS7 3EAI Gateway stopped gracefully")
}

// Handlers holds all initialized handlers
type Handlers struct {
	M3UA     *m3ua.Handler
	SCCP     *sccp.Handler
	TCAP     *tcap.Handler
	MAP      *mapprotocol.Handler
	CAP      *cap.Handler
	INAP     *inap.Handler
	NATS     *natsbridge.Handler
	Firewall *firewall.Handler
	TDR      *tdr.Handler
	HA       *ha.Handler
	API      *api.Handler
	Drain    *opstrigger.DrainState
	// USSD holds the node's USSD sessions (nil until the NATS bridge is
	// bound).
	USSD *ussdSessions
}

func initLogger(level string) (*zap.Logger, error) {
	// Parse log level
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level: %w", err)
	}

	// Create logger configuration
	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "timestamp",
			LevelKey:       "level",
			NameKey:        "logger",
			CallerKey:      "caller",
			MessageKey:     "message",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
	}

	return config.Build()
}

// envOverridableKeys are the config keys that MAY be injected via environment
// variables at runtime. Kept as an explicit allowlist (not blanket
// AutomaticEnv) so only the deployment-critical surface is env-overridable:
//   - secrets that must never live in a ConfigMap (Arinna packaging contract
//     §2.1 secretRefOnly) — injected from a K8s Secret via secretKeyRef;
//   - the per-pod identity a StatefulSet derives from its ordinal (node_id,
//     node_id_numeric, tcap.node_id, and the sticky-CgPA GT — ADR-006), which a
//     single Helm-rendered ConfigMap cannot vary across replicas.
//
// Env wins over the config file (Viper precedence: env > config), so the mounted
// config.yaml carries safe defaults and the chart overrides per pod. Binding is
// SS7_ prefix + dots→underscores, e.g.
// sccp.local_global_title.address → SS7_SCCP_LOCAL_GLOBAL_TITLE_ADDRESS.
var envOverridableKeys = []string{
	"node_id",
	"node_id_numeric",
	"cluster_name",
	"tcap.node_id",
	"sccp.local_global_title.address",
	"api.jwt_secret",
	"api.default_admin_password",
	"tdr.hmac_secret",
	"nats.url",
}

func loadConfig(configFile string) (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigFile(configFile)
	v.SetConfigType("yaml")

	// Set defaults
	v.SetDefault("node_id", "node-1")
	v.SetDefault("cluster_name", "ss7-cluster")

	// Environment overrides for secrets + per-pod identity (see
	// envOverridableKeys). BindEnv is called before ReadInConfig so the bound
	// keys resolve from the environment even if the file omits them.
	v.SetEnvPrefix("SS7")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	for _, key := range envOverridableKeys {
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("failed to bind env for %q: %w", key, err)
		}
	}

	// Read config file
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	return v, nil
}

// mapApplicationContexts are the MAP contexts a peer may open with the
// gateway: the three short-message contexts, and USSD (the HLR's
// processUnstructuredSS-Request to the gateway as gsmSCF), which the gateway
// also opens. A BEGIN under any other context (location
// update, …) is refused by the dispatcher instead of reaching a handler that
// would answer systemFailure.
var mapApplicationContexts = []tcap.ApplicationContext{
	tcap.ACShortMsgGatewayV3,
	tcap.ACShortMsgMORelayV3,
	tcap.ACShortMsgMTRelayV3,
	tcap.ACNetworkUnstructuredSsV2,
}

var capApplicationContexts = []tcap.ApplicationContext{tcap.ACCapV2, tcap.ACCapV3}

func initializeHandlers(cfg *viper.Viper, logger *zap.Logger) (*Handlers, error) {
	handlers := &Handlers{}

	// Initialize HA module. Cluster state is replicated via NATS JetStream KV
	// (see ADR-005); this module only reports node identity and health.
	logger.Info("Initializing HA module")
	haConfig := &ha.Config{
		NodeID:      cfg.GetString("node_id"),
		ClusterName: cfg.GetString("cluster_name"),
	}
	haHandler, err := ha.NewHandler(haConfig, logger.Named("ha"))
	if err != nil {
		return nil, fmt.Errorf("failed to create HA handler: %w", err)
	}
	handlers.HA = haHandler

	// SS7 protocol stack: M3UA → SCCP → TCAP → MAP/CAP. Construction follows
	// the bus topology pinned in Slice C: each upper layer takes its lower
	// layer's typed channels as constructor params (no interface{} seams).
	// The stack is M3UA/SCCP/TCAP/MAP/CAP; INAP is not implemented (ADR-002).

	logger.Info("Initializing M3UA handler")
	if !cfg.IsSet("m3ua.network_indicator") {
		return nil, fmt.Errorf("m3ua.network_indicator is required: the MTP3 network indicator " +
			"of this network (0 = international, 2 = national); the SG/STP discards a mismatch")
	}
	m3uaCfg := buildM3UAConfig(cfg)
	m3uaHandler, err := m3ua.NewHandler(m3uaCfg, logger.Named("m3ua"))
	if err != nil {
		return nil, fmt.Errorf("failed to create M3UA handler: %w", err)
	}
	handlers.M3UA = m3uaHandler

	logger.Info("Initializing SCCP handler")
	sccpCfg, err := buildSCCPConfig(cfg)
	if err != nil {
		return nil, err
	}
	sccpHandler, err := sccp.NewHandler(
		sccpCfg,
		m3uaHandler.IncomingMessages(),
		m3uaHandler.OutgoingMessages(),
		logger.Named("sccp"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create SCCP handler: %w", err)
	}
	handlers.SCCP = sccpHandler

	// Cross-config sanity checks that span SCCP + M3UA. Each individual
	// handler's validateConfig only sees its own slice, so misalignments
	// across the two layers must be caught here. Warnings only — operators
	// may have intentional reasons for a non-standard combination (e.g.
	// migration windows where cluster_gt is announced but loadshare not
	// yet enabled SG-side).
	warnHAConfigDrift(sccpCfg, m3uaCfg, logger)

	logger.Info("Initializing MAP handler")
	mapCfg, err := buildMAPConfig(cfg)
	if err != nil {
		return nil, err
	}
	capCfg := buildCAPConfig(cfg)
	for key, pc := range map[string]uint32{"map.hlr_pc": mapCfg.HLRPointCode, "cap.gsmscf_pc": capCfg.GSMSCFPointCode} {
		if !sccpCfg.NetworkVariant.ValidPointCode(pc) {
			return nil, fmt.Errorf("%s %d does not fit a %s point code (sccp.network_variant)", key, pc, sccpCfg.NetworkVariant)
		}
	}
	mapHandler, err := mapprotocol.NewHandler(mapCfg, logger.Named("map"))
	if err != nil {
		return nil, fmt.Errorf("failed to create MAP handler: %w", err)
	}
	handlers.MAP = mapHandler

	logger.Info("Initializing CAP handler")
	capHandler, err := cap.NewHandler(capCfg, logger.Named("cap"))
	if err != nil {
		return nil, fmt.Errorf("failed to create CAP handler: %w", err)
	}
	handlers.CAP = capHandler

	// INAP is not implemented (ADR-002). APIs that probe handlers.INAP
	// must already nil-guard.
	handlers.INAP = nil

	// TCAP application-context dispatcher (Slice 0). Strict-mode OID router
	// per GSMA FS.11: each (operation, version) pair must be explicitly
	// registered; unknown ACs trigger TCAP P-Abort no-common-dialogue-portion.
	dispatcher := tcap.NewDispatcher(logger.Named("tcap-dispatch"))
	for _, ac := range mapApplicationContexts {
		if err := dispatcher.Register(ac.OID, mapHandler); err != nil {
			return nil, fmt.Errorf("failed to register MAP for %s: %w", ac.Name, err)
		}
	}
	// Contexts only this node opens: answers to a version 2 retry (TS 29.002
	// §5.2.1), the location enquiries it sends as gsmSCF (to an HLR) or as
	// HLR (to a VLR), and the call routing interrogation it sends as GMSC.
	// A peer cannot open a dialogue under them.
	for _, ac := range []tcap.ApplicationContext{
		tcap.ACShortMsgGatewayV2, tcap.ACShortMsgMTRelayV2,
		tcap.ACAnyTimeInfoEnquiryV3, tcap.ACSubscriberInfoEnquiryV3,
		tcap.ACLocationInfoRetrievalV3,
	} {
		if err := dispatcher.RegisterInitiatorOnly(ac.OID, mapHandler); err != nil {
			return nil, fmt.Errorf("failed to register MAP for %s: %w", ac.Name, err)
		}
	}
	for _, ac := range capApplicationContexts {
		if err := dispatcher.Register(ac.OID, capHandler); err != nil {
			return nil, fmt.Errorf("failed to register CAP for %s: %w", ac.Name, err)
		}
	}

	logger.Info("Initializing TCAP handler")
	tcapHandler, err := tcap.NewHandler(
		buildTCAPConfig(cfg),
		sccpHandler.TcapIncomingChannel(),
		sccpHandler.TcapOutgoingChannel(),
		dispatcher,
		logger.Named("tcap"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create TCAP handler: %w", err)
	}
	handlers.TCAP = tcapHandler

	// Non-delivery of a message this node sent ends its dialogue at once:
	// M3UA reports what it could not send to SCCP, SCCP reports that and its
	// own routing failures and returned UDTSs to TCAP.
	m3uaHandler.OnSendFailure(sccpHandler.HandleMTP3SendFailure)
	sccpHandler.SetDeliveryFailureSink(tcapHandler.HandleDeliveryFailure)
	// Messages too long for one unitdata open with an empty Begin.
	tcapHandler.SetUserDataLimit(sccpHandler.MaxUserData)
	// A peer that refuses a version 3 context gets the request again in v2.
	tcapHandler.SetVersionFallback(mapprotocol.VersionFallback)

	// Wire MAP/CAP outbound TCAP emission. Now that tcapHandler exists,
	// hand its application-outbound channel to MAP and CAP so their
	// dispatchers can push ReturnResult / ReturnError / Reject responses
	// (Slice C wired the channel; this slice activates it).
	mapHandler.SetTCAPOutbound(tcapHandler.ApplicationOutbound())
	capHandler.SetTCAPOutbound(tcapHandler.ApplicationOutbound())

	// Initialize NATS Bridge — must exist before bindNATSBridge so the
	// inbound callbacks have a Handler to register against (handlers.NATS
	// is dereferenced inside bind).
	logger.Info("Initializing NATS bridge")
	natsConfig := &natsbridge.Config{
		ServerURL:         cfg.GetString("nats.url"),
		ClusterName:       cfg.GetString("cluster_name"),
		NodeID:            cfg.GetString("node_id"),
		MaxReconnects:     cfg.GetInt("nats.max_reconnects"),
		ReconnectWait:     cfg.GetDuration("nats.reconnect_wait"),
		PingInterval:      cfg.GetDuration("nats.ping_interval"),
		WorkerPoolSize:    cfg.GetInt("nats.worker_pool_size"),
		ChannelBufferSize: cfg.GetInt("nats.channel_buffer_size"),
		MessageTimeout:    cfg.GetDuration("nats.message_timeout"),
	}
	natsHandler, err := natsbridge.NewHandler(natsConfig, logger.Named("nats"))
	if err != nil {
		return nil, fmt.Errorf("failed to create NATS handler: %w", err)
	}
	handlers.NATS = natsHandler

	// NATS bridge bind: register inbound NATS callbacks (SRI, MT-SMS, CAP
	// InitialDP, INAP InitialDP) that originate TCAP dialogues, and the
	// MAP response sink that republishes ReturnResult components back onto
	// the matching NATS subjects. Done at construction time (not in
	// startServices) so callbacks are armed before any of the lifecycle
	// goroutines could fire — NATS subscribeAll happens in NATS.Start
	// downstream.
	handlers.USSD = bindNATSBridge(handlers, mapCfg, sccpCfg, logger).ussd

	// ConfigStore will be initialized after NATS is started (in startServices)

	// Initialize Firewall
	logger.Info("Initializing Firewall")
	firewallConfig := &firewall.Config{
		Enabled:                 cfg.GetBool("firewall.enabled"),
		Mode:                    firewall.FirewallMode(cfg.GetInt("firewall.mode")),
		DefaultAction:           firewall.Action(cfg.GetInt("firewall.default_action")),
		WorkerPoolSize:          cfg.GetInt("firewall.worker_pool_size"),
		InspectionTimeout:       cfg.GetDuration("firewall.inspection_timeout"),
		ChannelBufferSize:       cfg.GetInt("firewall.channel_buffer_size"),
		AnomalyDetectionEnabled: cfg.GetBool("firewall.anomaly_detection_enabled"),
		AnomalyThreshold:        cfg.GetFloat64("firewall.anomaly_threshold"),
		ThreatDBEnabled:         cfg.GetBool("firewall.threat_db_enabled"),
		ThreatDBUpdateInterval:  cfg.GetDuration("firewall.threat_db_update_interval"),
		LogBlocked:              cfg.GetBool("firewall.log_blocked"),
		LogAllowed:              cfg.GetBool("firewall.log_allowed"),
		AlertOnThreat:           cfg.GetBool("firewall.alert_on_threat"),
		AlertWebhookURL:         cfg.GetString("firewall.alert_webhook_url"),
	}
	firewallHandler, err := firewall.NewHandler(firewallConfig, logger.Named("firewall"))
	if err != nil {
		return nil, fmt.Errorf("failed to create Firewall handler: %w", err)
	}
	handlers.Firewall = firewallHandler

	// Initialize TDR Generator
	logger.Info("Initializing TDR generator")
	tdrConfig := &tdr.Config{
		Enabled:              cfg.GetBool("tdr.enabled"),
		NodeID:               cfg.GetString("node_id"),
		ClusterName:          cfg.GetString("cluster_name"),
		S3Bucket:             cfg.GetString("tdr.s3_bucket"),
		S3Region:             cfg.GetString("tdr.s3_region"),
		S3Prefix:             cfg.GetString("tdr.s3_prefix"),
		S3StorageClass:       cfg.GetString("tdr.s3_storage_class"),
		KMSKeyID:             cfg.GetString("tdr.kms_key_id"),
		KMSRegion:            cfg.GetString("tdr.kms_region"),
		EncryptionEnabled:    cfg.GetBool("tdr.encryption_enabled"),
		HMACSecret:           cfg.GetString("tdr.hmac_secret"),
		BufferSize:           cfg.GetInt("tdr.buffer_size"),
		FlushInterval:        cfg.GetDuration("tdr.flush_interval"),
		WorkerPoolSize:       cfg.GetInt("tdr.worker_pool_size"),
		ChannelBufferSize:    cfg.GetInt("tdr.channel_buffer_size"),
		CompressionEnabled:   cfg.GetBool("tdr.compression_enabled"),
		CompressionLevel:     cfg.GetInt("tdr.compression_level"),
		RetentionDays:        cfg.GetInt("tdr.retention_days"),
		ArchiveAfterDays:     cfg.GetInt("tdr.archive_after_days"),
		PIIEncryption:        cfg.GetBool("tdr.pii_encryption"),
		SearchableEncryption: cfg.GetBool("tdr.searchable_encryption"),
		ObjectLockEnabled:    cfg.GetBool("tdr.object_lock_enabled"),
		ObjectLockMode:       cfg.GetString("tdr.object_lock_mode"),
		ObjectLockDays:       cfg.GetInt("tdr.object_lock_days"),
	}
	tdrHandler, err := tdr.NewHandler(tdrConfig, logger.Named("tdr"))
	if err != nil {
		return nil, fmt.Errorf("failed to create TDR handler: %w", err)
	}
	handlers.TDR = tdrHandler

	// Initialize API Gateway
	logger.Info("Initializing API Gateway")
	apiConfig := &api.Config{
		BindAddr:             cfg.GetString("api.bind_addr"),
		TLSEnabled:           cfg.GetBool("api.tls_enabled"),
		TLSCertFile:          cfg.GetString("api.tls_cert_file"),
		TLSKeyFile:           cfg.GetString("api.tls_key_file"),
		JWTSecret:            cfg.GetString("api.jwt_secret"),
		JWTExpiration:        cfg.GetDuration("api.jwt_expiration"),
		CORSEnabled:          cfg.GetBool("api.cors_enabled"),
		CORSOrigins:          cfg.GetStringSlice("api.cors_origins"),
		RateLimitEnabled:     cfg.GetBool("api.rate_limit_enabled"),
		RateLimitRPS:         cfg.GetInt("api.rate_limit_rps"),
		ReadTimeout:          cfg.GetDuration("api.read_timeout"),
		WriteTimeout:         cfg.GetDuration("api.write_timeout"),
		IdleTimeout:          cfg.GetDuration("api.idle_timeout"),
		EnableSwagger:        cfg.GetBool("api.enable_swagger"),
		TrustedProxies:       cfg.GetStringSlice("api.trusted_proxies"),
		DefaultAdminUser:     cfg.GetString("api.default_admin_user"),
		DefaultAdminPassword: cfg.GetString("api.default_admin_password"),
		DevMode:              cfg.GetBool("api.dev_mode"),
	}

	// Drain state backs the opsTrigger `drain` action and the /api/v1/ready
	// probe. Created here so the API handler can read it from construction.
	handlers.Drain = opstrigger.NewDrainState()

	apiDeps := &api.Dependencies{
		HAHandler:       handlers.HA,
		M3UAHandler:     handlers.M3UA,
		SCCPHandler:     handlers.SCCP,
		TCAPHandler:     handlers.TCAP,
		MAPHandler:      handlers.MAP,
		CAPHandler:      handlers.CAP,
		INAPHandler:     handlers.INAP,
		FirewallHandler: handlers.Firewall,
		TDRHandler:      handlers.TDR,
		NATSHandler:     handlers.NATS,
		ConfigStore:     nil, // Will be initialized after NATS starts
		DrainProvider:   handlers.Drain,
	}

	api.Version = AppVersion
	apiHandler, err := api.NewHandler(apiConfig, apiDeps, logger.Named("api"))
	if err != nil {
		return nil, fmt.Errorf("failed to create API handler: %w", err)
	}
	handlers.API = apiHandler

	return handlers, nil
}

// startServices starts services in dependency order. SS7 stack starts
// bottom-up (M3UA → SCCP → MAP → CAP → TCAP) so that by the time TCAP begins
// dispatching, every layer below it is already draining its incoming channel
// and every application-context handler is ready to accept synchronous
// dispatch from the registry.
func startServices(ctx context.Context, handlers *Handlers, logger *zap.Logger) error {
	if err := handlers.HA.Start(ctx); err != nil {
		return fmt.Errorf("failed to start HA: %w", err)
	}
	if err := handlers.TDR.Start(ctx); err != nil {
		return fmt.Errorf("failed to start TDR: %w", err)
	}
	if err := handlers.Firewall.Start(ctx); err != nil {
		return fmt.Errorf("failed to start Firewall: %w", err)
	}
	if err := handlers.NATS.Start(ctx); err != nil {
		return fmt.Errorf("failed to start NATS: %w", err)
	}

	logger.Info("Initializing ConfigStore with NATS JetStream KV")
	configStore, err := config.NewConfigStore(handlers.NATS.Connection(), logger.Named("config"))
	if err != nil {
		return fmt.Errorf("failed to create ConfigStore: %w", err)
	}
	handlers.API.UpdateConfigStore(configStore)

	if err := handlers.M3UA.Start(ctx); err != nil {
		return fmt.Errorf("failed to start M3UA: %w", err)
	}
	if err := handlers.SCCP.Start(ctx); err != nil {
		return fmt.Errorf("failed to start SCCP: %w", err)
	}
	if err := handlers.MAP.Start(ctx); err != nil {
		return fmt.Errorf("failed to start MAP: %w", err)
	}
	if err := handlers.CAP.Start(ctx); err != nil {
		return fmt.Errorf("failed to start CAP: %w", err)
	}
	if err := handlers.TCAP.Start(ctx); err != nil {
		return fmt.Errorf("failed to start TCAP: %w", err)
	}

	if err := handlers.API.Start(ctx); err != nil {
		return fmt.Errorf("failed to start API: %w", err)
	}

	startOpsTriggerConsumer(ctx, handlers, logger)

	logger.Info("All services started successfully")
	return nil
}

// startOpsTriggerConsumer wires the declarative day-2 trigger consumer (Arinna
// packaging contract §5). It watches the mounted opsTrigger token file and maps
// each action to a real gateway capability, reporting every outcome on a K8s
// Event (falling back to log-only when not running in a cluster). The consumer
// baselines existing tokens at startup so a fresh pod does not re-run them.
func startOpsTriggerConsumer(ctx context.Context, handlers *Handlers, logger *zap.Logger) {
	log := logger.Named("opstrigger")

	// Outcome reporting: always log; add the K8s Event surface when in-cluster.
	reporters := []opstrigger.Reporter{opstrigger.NewLogReporter(log)}
	if evt, err := opstrigger.NewInClusterEventReporter(log); err != nil {
		log.Info("opsTrigger: K8s Event surface unavailable, logging outcomes only",
			zap.Error(err))
	} else {
		reporters = append(reporters, evt)
	}
	reporter := opstrigger.NewMultiReporter(reporters...)

	path := *opstriggerFile
	if path == "" {
		path = filepath.Join(filepath.Dir(*configFile), "opstrigger.yaml")
	}
	consumer := opstrigger.NewConsumer(path, 0, reporter, log)

	// reloadConfig: re-read + validate the on-disk config so a bad mount surfaces
	// as an OpsTriggerFailed Event. Live re-apply to running handlers is not yet
	// supported — config changes that must take effect roll the pod via the
	// chart's config checksum.
	consumer.Register("reloadConfig", func(_ context.Context, token string) error {
		if _, err := loadConfig(*configFile); err != nil {
			return err
		}
		log.Info("opsTrigger reloadConfig: config re-read and valid", zap.String("token", token))
		return nil
	})

	// resetPeer: drop + re-establish the M3UA/SCTP association.
	consumer.Register("resetPeer", func(_ context.Context, _ string) error {
		return handlers.M3UA.ResetAssociation()
	})

	// drain: fail readiness (/api/v1/ready) and stop NATS intake so no new
	// dialogues start; liveness stays up and in-flight work finishes.
	consumer.Register("drain", func(_ context.Context, _ string) error {
		handlers.Drain.SetDraining(true)
		return handlers.NATS.PauseIntake()
	})

	// flushTDR: force the TDR spool to S3 now.
	consumer.Register("flushTDR", func(_ context.Context, _ string) error {
		return handlers.TDR.Flush()
	})

	consumer.Start(ctx)
}

// stopServices stops services in reverse start order. Errors during shutdown
// are logged but do not short-circuit — every component must get a chance to
// drain so that no layer leaves goroutines logging into a closed test scribe
// or an already-flushed Loki shipper.
func stopServices(ctx context.Context, handlers *Handlers, logger *zap.Logger) error {
	if err := handlers.API.Stop(ctx); err != nil {
		logger.Error("Failed to stop API", zap.Error(err))
	}
	// Open USSD sessions are aborted while the stack can still send.
	if handlers.USSD != nil {
		handlers.USSD.close()
	}
	if err := handlers.TCAP.Stop(ctx); err != nil {
		logger.Error("Failed to stop TCAP", zap.Error(err))
	}
	if err := handlers.CAP.Stop(ctx); err != nil {
		logger.Error("Failed to stop CAP", zap.Error(err))
	}
	if err := handlers.MAP.Stop(ctx); err != nil {
		logger.Error("Failed to stop MAP", zap.Error(err))
	}
	if err := handlers.SCCP.Stop(ctx); err != nil {
		logger.Error("Failed to stop SCCP", zap.Error(err))
	}
	if err := handlers.M3UA.Stop(ctx); err != nil {
		logger.Error("Failed to stop M3UA", zap.Error(err))
	}
	if err := handlers.NATS.Stop(ctx); err != nil {
		logger.Error("Failed to stop NATS", zap.Error(err))
	}
	if err := handlers.Firewall.Stop(ctx); err != nil {
		logger.Error("Failed to stop Firewall", zap.Error(err))
	}
	if err := handlers.TDR.Stop(ctx); err != nil {
		logger.Error("Failed to stop TDR", zap.Error(err))
	}
	if err := handlers.HA.Stop(ctx); err != nil {
		logger.Error("Failed to stop HA", zap.Error(err))
	}

	logger.Info("All services stopped")
	return nil
}

// warnHAConfigDrift surfaces cross-layer misalignments between SCCP and
// M3UA that single-handler validateConfig cannot catch. Per ADR-006
// AA-loadshare topology, sccp.cluster_global_title set implies the
// operator wants AA cluster behavior — which requires
// m3ua.traffic_mode = 2 (Loadshare). With traffic_mode = 1 (Override)
// the SG sends ALL inbound to a single ASP, so the cluster GT
// effectively pins to one node and the loadshare benefit is lost.
//
// Warnings (not errors) — operators may have intentional reasons for
// the mismatch (migration scenarios, single-node deployment that still
// publishes a cluster_gt for future expansion).
func warnHAConfigDrift(sccpCfg *sccp.Config, m3uaCfg *m3ua.Config, logger *zap.Logger) {
	clusterGTSet := sccpCfg.ClusterGlobalTitle.Address != ""
	if clusterGTSet && m3uaCfg.TrafficMode == 1 {
		logger.Warn("HA config drift: cluster_global_title set but m3ua.traffic_mode is 1 (Override) — SG will not loadshare across cluster nodes; expected traffic_mode=2 (Loadshare) per ADR-006",
			zap.String("cluster_gt", sccpCfg.ClusterGlobalTitle.Address),
			zap.Uint32("traffic_mode", m3uaCfg.TrafficMode))
	}
	// Inverse direction: traffic_mode=2 without cluster_gt is also worth
	// flagging — loadshare against an unannounced cluster GT means the
	// operator likely forgot one half of the cluster setup.
	if !clusterGTSet && m3uaCfg.TrafficMode == 2 {
		logger.Warn("HA config drift: m3ua.traffic_mode is 2 (Loadshare) but sccp.cluster_global_title is empty — cluster has no shared inbound GT; SG loadshare will only see per-node sticky GTs",
			zap.Uint32("traffic_mode", m3uaCfg.TrafficMode))
	}
}

// buildM3UAConfig translates viper m3ua.* keys into m3ua.Config. Required
// fields (sgw_address/sgw_port/aspup_ack_timeout) are validated downstream by
// m3ua.NewHandler; this builder does not invent defaults for connection
// targets — operators must configure them.
func buildM3UAConfig(cfg *viper.Viper) *m3ua.Config {
	return &m3ua.Config{
		SGWAddress:           cfg.GetString("m3ua.sgw_address"),
		SGWPort:              cfg.GetInt("m3ua.sgw_port"),
		ASPIdentifier:        cfg.GetUint32("m3ua.asp_id"),
		RoutingContext:       cfg.GetUint32("m3ua.routing_context"),
		NetworkAppearance:    cfg.GetUint32("m3ua.network_appearance"),
		NetworkIndicator:     uint8(cfg.GetUint32("m3ua.network_indicator")),
		TrafficMode:          cfg.GetUint32("m3ua.traffic_mode"),
		MultiHomingEnabled:   cfg.GetBool("m3ua.multi_homing_enabled"),
		LocalIPs:             cfg.GetStringSlice("m3ua.local_ips"),
		HeartbeatInterval:    cfg.GetDuration("m3ua.heartbeat_interval"),
		PathMaxRetrans:       uint16(cfg.GetUint32("m3ua.path_max_retrans")),
		RTOMax:               cfg.GetDuration("m3ua.rto_max"),
		RTOInitial:           cfg.GetDuration("m3ua.rto_initial"),
		RTOMin:               cfg.GetDuration("m3ua.rto_min"),
		AssocMaxRetrans:      uint16(cfg.GetUint32("m3ua.assoc_max_retrans")),
		ASPUPACKTimeout:      cfg.GetDuration("m3ua.aspup_ack_timeout"),
		ASPACACKTimeout:      cfg.GetDuration("m3ua.aspac_ack_timeout"),
		ReconnectInterval:    cfg.GetDuration("m3ua.reconnect_interval"),
		ReconnectMaxAttempts: cfg.GetInt("m3ua.reconnect_max_attempts"),
	}
}

// buildSCCPConfig collects sccp.* keys. local_subsystems is read as an
// []int and narrowed to []sccp.SubsystemNumber; viper does not preserve
// the typed slice element through YAML. network_variant accepts the
// strings "itu" (default) or "ansi" — see sccp.NetworkVariant docs.
func buildSCCPConfig(cfg *viper.Viper) (*sccp.Config, error) {
	var gttRules []sccp.GTTRuleConfig
	if err := cfg.UnmarshalKey("sccp.gtt_rules", &gttRules); err != nil {
		return nil, fmt.Errorf("sccp.gtt_rules: %w", err)
	}
	rawSSNs := cfg.GetIntSlice("sccp.local_subsystems")
	ssns := make([]sccp.SubsystemNumber, 0, len(rawSSNs))
	for _, n := range rawSSNs {
		ssns = append(ssns, sccp.SubsystemNumber(n))
	}
	variant := sccp.VariantITU
	switch v := cfg.GetString("sccp.network_variant"); v {
	case "ansi", "ANSI":
		variant = sccp.VariantANSI
	case "itu", "ITU", "":
		variant = sccp.VariantITU
	}
	return &sccp.Config{
		LocalPointCode:  cfg.GetUint32("sccp.local_point_code"),
		LocalSubsystems: ssns,
		NetworkVariant:  variant,
		LocalGlobalTitle: sccp.GTConfig{
			Address:         cfg.GetString("sccp.local_global_title.address"),
			GTI:             uint8(cfg.GetUint32("sccp.local_global_title.gti")),
			TranslationType: uint8(cfg.GetUint32("sccp.local_global_title.translation_type")),
			NumberingPlan:   uint8(cfg.GetUint32("sccp.local_global_title.numbering_plan")),
			NatureOfAddress: uint8(cfg.GetUint32("sccp.local_global_title.nature_of_address")),
		},
		ClusterGlobalTitle: sccp.GTConfig{
			Address:         cfg.GetString("sccp.cluster_global_title.address"),
			GTI:             uint8(cfg.GetUint32("sccp.cluster_global_title.gti")),
			TranslationType: uint8(cfg.GetUint32("sccp.cluster_global_title.translation_type")),
			NumberingPlan:   uint8(cfg.GetUint32("sccp.cluster_global_title.numbering_plan")),
			NatureOfAddress: uint8(cfg.GetUint32("sccp.cluster_global_title.nature_of_address")),
		},
		GTTEnabled:          cfg.GetBool("sccp.gtt_enabled"),
		GTTRules:            gttRules,
		GTTCacheEnabled:     cfg.GetBool("sccp.gtt_cache_enabled"),
		GTTCacheTTL:         cfg.GetDuration("sccp.gtt_cache_ttl"),
		GTTCacheMaxSize:     cfg.GetInt("sccp.gtt_cache_max_size"),
		MaxHopCounter:       uint8(cfg.GetUint32("sccp.max_hop_counter")),
		SegmentationEnabled: cfg.GetBool("sccp.segmentation_enabled"),
		MaxSegmentSize:      cfg.GetInt("sccp.max_segment_size"),
		ChannelBufferSize:   cfg.GetInt("sccp.channel_buffer_size"),
		WorkerPoolSize:      cfg.GetInt("sccp.worker_pool_size"),
	}, nil
}

// buildTCAPConfig collects tcap.* keys. node_id falls back to top-level
// node_id_numeric for distributed-TID uniqueness across the cluster.
func buildTCAPConfig(cfg *viper.Viper) *tcap.Config {
	nodeID := uint8(cfg.GetUint32("tcap.node_id"))
	if nodeID == 0 {
		nodeID = uint8(cfg.GetUint32("node_id_numeric"))
	}
	return &tcap.Config{
		NodeID:                  nodeID,
		DialogueTimeout:         cfg.GetDuration("tcap.dialogue_timeout"),
		InvokeTimeout:           cfg.GetDuration("tcap.invoke_timeout"),
		IdleTimeout:             cfg.GetDuration("tcap.idle_timeout"),
		MaxDialogues:            cfg.GetInt("tcap.max_dialogues"),
		MaxComponentsPerMessage: cfg.GetInt("tcap.max_components_per_message"),
		MaxInvokesPerDialogue:   cfg.GetInt("tcap.max_invokes_per_dialogue"),
		ChannelBufferSize:       cfg.GetInt("tcap.channel_buffer_size"),
		WorkerPoolSize:          cfg.GetInt("tcap.worker_pool_size"),
		TimerResolution:         cfg.GetDuration("tcap.timer_resolution"),
	}
}

func buildMAPConfig(cfg *viper.Viper) (*mapprotocol.Config, error) {
	var routes []mapprotocol.USSDRoute
	if err := cfg.UnmarshalKey("map.ussd_routes", &routes); err != nil {
		return nil, fmt.Errorf("map.ussd_routes: %w", err)
	}
	return &mapprotocol.Config{
		SRIRouting:         cfg.GetString("map.sri_routing"),
		HLRAddress:         cfg.GetString("map.hlr_address"),
		HLRGlobalTitle:     cfg.GetString("map.hlr_gt"),
		HLRPointCode:       cfg.GetUint32("map.hlr_pc"),
		GSMSCFAddress:      cfg.GetString("map.gsmscf_address"),
		USSDSessionTimeout: cfg.GetDuration("map.ussd_session_timeout"),
		USSDRoutes:         routes,
		ChannelBufferSize:  cfg.GetInt("map.channel_buffer_size"),
		WorkerPoolSize:     cfg.GetInt("map.worker_pool_size"),
		OperationTimeout:   cfg.GetDuration("map.operation_timeout"),
	}, nil
}

// buildCAPConfig collects cap.* keys. service_keys is a YAML map of
// CAMEL service-key ID → human-readable name; viper exposes it via
// GetStringMapString which we narrow back to map[uint32]string.
func buildCAPConfig(cfg *viper.Viper) *cap.Config {
	rawKeys := cfg.GetStringMapString("cap.service_keys")
	keys := make(map[uint32]string, len(rawKeys))
	for k, v := range rawKeys {
		var n uint32
		if _, err := fmt.Sscanf(k, "%d", &n); err == nil {
			keys[n] = v
		}
	}
	return &cap.Config{
		GSMSCFAddress:     cfg.GetString("cap.gsmscf_address"),
		GSMSCFGlobalTitle: cfg.GetString("cap.gsmscf_gt"),
		GSMSCFPointCode:   cfg.GetUint32("cap.gsmscf_pc"),
		ServiceKeys:       keys,
		ChannelBufferSize: cfg.GetInt("cap.channel_buffer_size"),
		WorkerPoolSize:    cfg.GetInt("cap.worker_pool_size"),
		OperationTimeout:  cfg.GetDuration("cap.operation_timeout"),
	}
}
