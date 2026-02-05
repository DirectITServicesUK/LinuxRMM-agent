// RMM Agent - Entry Point
//
// This is the main entry point for the Linux RMM (Remote Monitoring and Management) agent.
// The agent runs as a systemd service on managed Linux hosts, responsible for:
//   - Reporting system statistics (CPU, memory, disk, network) to the central server
//   - Polling for and executing commands from operators
//   - Receiving urgent commands via WebSocket push
//   - Self-updating with verification and staged rollout
//
// Configuration is loaded from /etc/rmm-agent/config.yaml (or path specified by -config flag).
//
// Lifecycle:
//  1. Load configuration from YAML file
//  2. Setup structured JSON logger
//  3. Complete registration if needed (exchange device_token for api_key)
//  4. Notify systemd that service is ready (Type=notify)
//  5. Start watchdog goroutine if systemd provides WatchdogSec
//  6. Start polling loop (heartbeat + future command fetch)
//  7. Wait for shutdown signal (SIGTERM/SIGINT)
//  8. Notify systemd that service is stopping
//  9. Coordinated shutdown with timeout
//
// For more information, see the project documentation.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/client"
	"github.com/doughall/linuxrmm/agent/internal/commands"
	"github.com/doughall/linuxrmm/agent/internal/config"
	"github.com/doughall/linuxrmm/agent/internal/logging"
	natsinternal "github.com/doughall/linuxrmm/agent/internal/nats"
	"github.com/doughall/linuxrmm/agent/internal/poller"
	"github.com/doughall/linuxrmm/agent/internal/results"
	"github.com/doughall/linuxrmm/agent/internal/scheduler"
	"github.com/doughall/linuxrmm/agent/internal/shutdown"
	"github.com/doughall/linuxrmm/agent/internal/stats"
	"github.com/doughall/linuxrmm/agent/internal/sysinfo"
	"github.com/doughall/linuxrmm/agent/internal/systemd"
	"github.com/doughall/linuxrmm/agent/internal/updater"
	"github.com/doughall/linuxrmm/agent/internal/version"
	"github.com/doughall/linuxrmm/agent/internal/websocket"
)

// Default shutdown timeout - how long to wait for graceful shutdown
const shutdownTimeout = 30 * time.Second

// Stats reporting interval - how often to collect and send system metrics
const statsInterval = 60 * time.Second

func main() {
	// Parse command-line flags
	configPath := flag.String("config", config.DefaultConfigPath, "path to configuration file")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	// Handle --version flag
	if *showVersion {
		fmt.Println(version.Info())
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		// Use basic stderr logging before logger is configured
		fmt.Fprintf(os.Stderr, "ERROR: failed to load configuration from %s: %v\n", *configPath, err)
		os.Exit(1)
	}

	// Setup structured logger based on config
	logger := logging.SetupLogger(cfg.LogLevel)

	// Log startup information
	logger.Info("agent starting",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit),
		slog.String("build_time", version.BuildTime),
		slog.String("config_path", *configPath),
		slog.String("server_url", cfg.ServerURL),
		slog.String("group", cfg.Group),
		slog.Int("poll_interval", cfg.PollInterval),
		slog.Bool("registered", cfg.IsRegistered()),
	)

	// Create shutdown context that listens for SIGTERM and SIGINT
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Handle registration if needed
	if cfg.NeedsRegistration() {
		logger.Info("agent needs registration, starting registration process")

		hostname, err := os.Hostname()
		if err != nil {
			logger.Error("failed to get hostname", "error", err)
			os.Exit(1)
		}

		// Collect system information to send during registration
		sysInfo, err := sysinfo.Collect(ctx)
		if err != nil {
			logger.Warn("failed to collect system info for registration",
				slog.String("error", err.Error()),
			)
			// Continue with nil sysInfo - registration can proceed without it
		} else {
			// Add agent version to system info
			sysInfo.AgentVersion = version.Version
			logger.Info("collected system info",
				slog.String("os", sysInfo.OS),
				slog.String("platform", sysInfo.Platform),
				slog.String("platform_version", sysInfo.PlatformVersion),
				slog.String("kernel", sysInfo.KernelVersion),
				slog.String("arch", sysInfo.Arch),
			)
		}

		// Register with server using device token (use RegisterFull for NATS creds)
		regResult, err := client.RegisterFull(ctx, cfg.ServerURL, cfg.DeviceToken, hostname, sysInfo, logger)
		if err != nil {
			logger.Error("registration failed", "error", err)
			os.Exit(1)
		}

		logger.Info("registration successful",
			slog.String("agent_id", regResult.AgentID),
			slog.Bool("nats_enabled", regResult.NATSServers != ""),
		)

		// CRITICAL: Save all credentials to config file immediately after registration
		// Without this, the agent would re-register on every restart
		cfg.APIKey = regResult.APIKey
		cfg.AgentID = regResult.AgentID
		cfg.TenantID = regResult.TenantID

		// Save NATS credentials if provided
		if regResult.NATSServers != "" {
			cfg.NATSServers = regResult.NATSServers
			cfg.NATSNKeySeed = regResult.NATSNKeySeed
		}

		if err := config.Save(*configPath, cfg); err != nil {
			logger.Error("failed to save config after registration", "error", err)
			// This is critical - we have credentials but can't persist them
			os.Exit(1)
		}

		logger.Info("credentials saved to config file",
			slog.String("config_path", *configPath),
		)
	}

	// Run startup health check for already-registered agents
	// This runs early to detect broken binaries after updates and trigger rollback
	if cfg.IsRegistered() {
		backupMgr, err := updater.NewBackupManager("/var/lib/rmm-agent/backups", updater.DefaultMaxBackups, logger)
		if err != nil {
			logger.Warn("failed to create backup manager for health check",
				slog.String("error", err.Error()),
			)
		} else {
			healthChecker := updater.NewHealthChecker(cfg.ServerURL, cfg.APIKey, backupMgr, logger)

			// Get current executable path
			execPath, err := os.Executable()
			if err != nil {
				logger.Error("failed to get executable path", "error", err)
				os.Exit(1)
			}

			if err := healthChecker.RunStartupHealthCheck(ctx, execPath); err != nil {
				logger.Error("startup health check failed", "error", err)
				// Don't exit here - health check handles rollback and exit if needed
				// If we reach here, it's a non-rollback failure (no recent update)
			}
		}
	}

	// Create HTTP client for server communication
	httpClient := client.NewClient(cfg.ServerURL, logger)
	httpClient.SetAPIKey(cfg.APIKey)
	httpClient.SetAgentID(cfg.AgentID)

	// Create shutdown coordinator for ordered component shutdown
	coordinator := shutdown.NewCoordinator(logger)

	// Initialize schedule cache and result queue for offline execution
	// Database paths: derive from config path or use /var/lib/rmm-agent
	dataDir := filepath.Dir(*configPath)
	if dataDir == "/etc/rmm-agent" {
		dataDir = "/var/lib/rmm-agent"
	}

	scheduleCachePath := filepath.Join(dataDir, "schedules.db")
	resultQueuePath := filepath.Join(dataDir, "results.db")

	// Create schedule cache
	var scheduleCache *scheduler.ScheduleCache
	var resultQueue *results.ResultQueue
	var localScheduler *scheduler.Scheduler

	scheduleCache, err = scheduler.NewScheduleCache(scheduleCachePath)
	if err != nil {
		logger.Warn("failed to initialize schedule cache, offline scheduling disabled",
			slog.String("path", scheduleCachePath),
			slog.String("error", err.Error()),
		)
		scheduleCache = nil
	} else {
		logger.Info("schedule cache initialized",
			slog.String("path", scheduleCachePath),
		)
	}

	// Create result queue
	resultQueue, err = results.NewResultQueue(resultQueuePath)
	if err != nil {
		logger.Warn("failed to initialize result queue, schedule results will not be queued",
			slog.String("path", resultQueuePath),
			slog.String("error", err.Error()),
		)
		resultQueue = nil
	} else {
		logger.Info("result queue initialized",
			slog.String("path", resultQueuePath),
		)
	}

	// Create local scheduler if cache and queue are available
	if scheduleCache != nil && resultQueue != nil {
		localScheduler = scheduler.NewScheduler(scheduleCache, resultQueue, logger)
		coordinator.Register("scheduler", localScheduler)
		logger.Info("local scheduler initialized")
	}

	// Create result uploader to sync queued results to server
	// The uploader periodically checks the result queue and uploads batches
	// to the server when connectivity is available
	var resultUploader *results.Uploader
	if resultQueue != nil {
		resultUploader = results.NewUploader(resultQueue, httpClient, logger)
		coordinator.Register("result-uploader", resultUploader)
		logger.Info("result uploader initialized")
	}

	// Create and configure the polling loop
	pollInterval := time.Duration(cfg.PollInterval) * time.Second
	pollJitter := time.Duration(cfg.JitterSeconds) * time.Second
	poll := poller.NewPoller(httpClient, pollInterval, pollJitter, logger)

	// Create and set command handler
	commandHandler := commands.NewHandler(httpClient, logger)
	poll.SetCommandHandler(commandHandler)

	// Register poller for shutdown (will be stopped during graceful shutdown)
	coordinator.Register("poller", poll)

	// Create stats collector and reporter
	// The reporter runs independently of the poller, collecting and sending
	// system metrics every 60 seconds
	collector := stats.NewCollector(logger)
	statsReporter := stats.NewReporter(collector, httpClient, logger, statsInterval)

	// Register stats reporter for shutdown (stats can stop before poller since not critical)
	coordinator.Register("stats-reporter", statsReporter)

	// Create system info reporter
	// Reports OS, hardware, platform info at startup and periodically (hourly)
	// to detect system changes like OS upgrades
	sysinfoReporter := sysinfo.NewReporter(
		cfg.ServerURL,
		cfg.APIKey,
		cfg.AgentID,
		version.Version,
		httpClient.HTTPClient(),
		logger,
		sysinfo.DefaultReportInterval,
	)
	coordinator.Register("sysinfo-reporter", sysinfoReporter)

	// Create deduplicator shared across all command delivery channels
	dedup := commandHandler.GetDeduplicator()

	// Initialize NATS client if configured (preferred over WebSocket)
	var natsClient *natsinternal.Client
	var natsPublisher *natsinternal.Publisher

	if cfg.NATSEnabled() {
		logger.Info("NATS enabled, initializing NATS client",
			slog.String("servers", cfg.NATSServers),
			slog.String("tenant_id", cfg.TenantID),
		)

		natsCfg := natsinternal.Config{
			Servers:  cfg.NATSServers,
			NKeySeed: cfg.NATSNKeySeed,
			TenantID: cfg.TenantID,
			AgentID:  cfg.AgentID,
			Group:    cfg.Group,
		}

		natsClient = natsinternal.NewClient(natsCfg, logger)

		if err := natsClient.Connect(ctx); err != nil {
			logger.Warn("NATS connection failed, falling back to WebSocket",
				slog.String("error", err.Error()),
			)
			natsClient = nil
		} else {
			// Create NATS publisher for sending results and stats
			natsPublisher = natsinternal.NewPublisher(natsClient, logger)

			// Wire stats reporter to use NATS for publishing
			statsReporter.SetStatsPublisher(natsPublisher)

			// Wire sysinfo reporter to use NATS for publishing
			sysinfoReporter.SetPublisher(natsPublisher)

			// Wire poller to use NATS for heartbeats
			poll.SetHeartbeatPublisher(natsPublisher)

			// Create NATS handler and set it
			natsHandler := natsinternal.NewHandler(httpClient, natsPublisher, dedup, scheduleCache, logger)
			natsClient.SetHandler(natsHandler)

			// Create terminal handler for interactive terminal sessions
			terminalHandler := natsinternal.NewTerminalHandler(natsPublisher, logger)
			natsClient.SetTerminalHandler(terminalHandler)

			coordinator.Register("nats", natsClient)
			logger.Info("NATS client initialized")
		}
	}

	// Create WebSocket client as fallback (or primary if NATS not configured)
	// WebSocket provides real-time delivery when NATS is unavailable
	var wsClient *websocket.Client
	if natsClient == nil {
		logger.Info("Using WebSocket for real-time communication")
		wsHandler := websocket.NewHandler(httpClient, dedup, scheduleCache, logger)
		wsClient = websocket.NewClient(cfg.ServerURL, cfg.APIKey, logger, wsHandler)

		// Register WebSocket for shutdown
		coordinator.Register("websocket", wsClient)
	}

	// Create updater if updates are enabled and agent is registered
	// The updater runs periodic checks for new versions and applies updates
	var updateChecker *updater.Updater
	if cfg.IsRegistered() && !isUpdateDisabled(cfg) {
		updaterCfg := &updater.Config{
			ServerURL:      cfg.ServerURL,
			AgentID:        cfg.AgentID,
			APIKey:         cfg.APIKey,
			CheckIntervalH: cfg.UpdateCheckInterval,
		}

		var err error
		updateChecker, err = updater.NewUpdater(updaterCfg, logger)
		if err != nil {
			logger.Warn("failed to create updater, updates disabled",
				slog.String("error", err.Error()),
			)
		} else {
			coordinator.Register("updater", updateChecker)
			logger.Info("updater initialized",
				slog.Int("check_interval_hours", cfg.UpdateCheckInterval),
			)
		}
	}

	// Notify systemd that we're ready
	systemd.NotifyReady()
	logger.Info("agent ready")

	// Start watchdog if systemd provides WatchdogSec
	// Health check uses poller's health status
	systemd.StartWatchdog(ctx, func() bool {
		return poll.IsHealthy()
	})

	// Start the polling loop in a goroutine
	go poll.Run(ctx)

	// Start the stats reporter in a goroutine
	// Reporter runs independently of poller - collects and sends system metrics
	go statsReporter.Run(ctx)

	// Start the sysinfo reporter in a goroutine
	// Reports system info at startup and hourly to detect OS/hardware changes
	go sysinfoReporter.Run(ctx)

	// Start real-time communication client (NATS or WebSocket)
	if natsClient != nil {
		go natsClient.Run(ctx)
	} else if wsClient != nil {
		go wsClient.Run(ctx)
	}

	// Start local scheduler if available
	if localScheduler != nil {
		go localScheduler.Run(ctx)
	}

	// Start result uploader if available
	// Uploader runs alongside scheduler to sync queued results to server
	if resultUploader != nil {
		go resultUploader.Run(ctx)
	}

	// Start updater if configured
	// Updater checks periodically for new versions and applies updates
	if updateChecker != nil {
		go updateChecker.Run(ctx)
	}

	// Wait for shutdown signal
	<-ctx.Done()
	logger.Info("shutdown signal received, starting graceful shutdown")

	// Notify systemd we're stopping
	systemd.NotifyStopping()

	// Create shutdown context with timeout
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Perform coordinated shutdown
	if err := coordinator.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	// Close schedule cache and result queue
	if scheduleCache != nil {
		if err := scheduleCache.Close(); err != nil {
			logger.Warn("failed to close schedule cache",
				slog.String("error", err.Error()),
			)
		}
	}
	if resultQueue != nil {
		if err := resultQueue.Close(); err != nil {
			logger.Warn("failed to close result queue",
				slog.String("error", err.Error()),
			)
		}
	}

	logger.Info("shutdown complete")
}

// isUpdateDisabled returns true if updates are explicitly disabled in config.
// Note: UpdateEnabled defaults to false (zero value) but we treat that as enabled.
// Users must explicitly set update_enabled: false to disable.
func isUpdateDisabled(cfg *config.Config) bool {
	// If UpdateEnabled field was explicitly set to false in the YAML, it will be false.
	// Since Go's zero value for bool is false, we need a way to distinguish
	// "not set" from "explicitly set to false".
	// The config package handles this by treating the zero value as enabled (default behavior).
	// So we only consider updates disabled if UpdateEnabled is explicitly false
	// AND the config was loaded with that explicit setting.
	// For simplicity, we use a pragmatic approach: check if both UpdateEnabled is false
	// AND UpdateCheckInterval is 0 (both defaults), which means "use defaults" = enabled.
	// If UpdateEnabled is false but interval is non-zero, user explicitly configured something.
	//
	// Actually, the cleanest approach is simpler: the config defaults UpdateEnabled to true
	// conceptually - we just check the field directly since koanf will parse explicit false.
	return !cfg.UpdateEnabled
}
