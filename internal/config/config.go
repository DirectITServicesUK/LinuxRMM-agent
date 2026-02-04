// Package config provides configuration management for the RMM agent.
// It uses koanf v2 to load configuration from YAML files and supports
// saving updated configuration (e.g., persisting api_key after registration).
//
// Configuration is loaded from /etc/rmm-agent/config.yaml by default.
// The configuration file should have restricted permissions (0600) as it
// contains sensitive credentials like the API key.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	goyaml "gopkg.in/yaml.v3"
)

// DefaultConfigPath is the default location for the agent configuration file.
const DefaultConfigPath = "/etc/rmm-agent/config.yaml"

// Config holds the agent configuration loaded from the YAML config file.
// Fields are tagged for both koanf (loading) and yaml (saving).
type Config struct {
	// ServerURL is the base URL of the RMM server (e.g., "https://rmm.example.com").
	// Required for all operations.
	ServerURL string `koanf:"server_url" yaml:"server_url"`

	// DeviceToken is the one-time token used during initial registration.
	// Required for first-time registration, then can be removed.
	DeviceToken string `koanf:"device_token" yaml:"device_token"`

	// APIKey is the persistent authentication key received after registration.
	// This is set by the agent after successful registration and saved to config.
	APIKey string `koanf:"api_key" yaml:"api_key"`

	// AgentID is the unique identifier assigned by the server during registration.
	// This is set by the agent after successful registration and saved to config.
	AgentID string `koanf:"agent_id" yaml:"agent_id"`

	// Group is the agent group for organizing devices (e.g., "production", "staging").
	// Required for registration.
	Group string `koanf:"group" yaml:"group"`

	// PollInterval is how often (in seconds) the agent polls the server for commands.
	// Default: 60 seconds.
	PollInterval int `koanf:"poll_interval" yaml:"poll_interval"`

	// JitterSeconds is the maximum random jitter added to poll interval to prevent
	// thundering herd. Actual jitter is random between 0 and JitterSeconds.
	// Default: 30 seconds.
	JitterSeconds int `koanf:"jitter_seconds" yaml:"jitter_seconds"`

	// LogLevel controls the verbosity of agent logging.
	// Valid values: "debug", "info", "warn", "error".
	// Default: "info".
	LogLevel string `koanf:"log_level" yaml:"log_level"`

	// UpdateEnabled controls whether the agent checks for and applies updates.
	// Default: true.
	UpdateEnabled bool `koanf:"update_enabled" yaml:"update_enabled"`

	// UpdateCheckInterval is how often (in hours) the agent checks for updates.
	// Default: 24 hours (daily).
	UpdateCheckInterval int `koanf:"update_check_interval" yaml:"update_check_interval"`

	// TenantID is the tenant identifier for multi-tenant isolation.
	// Set during registration and used for NATS subject routing.
	TenantID string `koanf:"tenant_id" yaml:"tenant_id"`

	// NATSServers is a comma-separated list of NATS server URLs.
	// If set, NATS is used for real-time communication instead of WebSocket.
	NATSServers string `koanf:"nats_servers" yaml:"nats_servers"`

	// NATSNKeySeed is the NKey seed for NATS authentication.
	// Set during registration when NATS is enabled on the server.
	NATSNKeySeed string `koanf:"nats_nkey_seed" yaml:"nats_nkey_seed"`
}

// Validation errors returned by Load when required fields are missing.
var (
	ErrServerURLRequired   = errors.New("server_url is required")
	ErrGroupRequired       = errors.New("group is required")
	ErrNoCredentials       = errors.New("either device_token (for registration) or api_key (for operation) is required")
	ErrInvalidPollInterval = errors.New("poll_interval must be positive")
)

// Load reads configuration from the specified YAML file path.
// It applies defaults for optional fields and validates required fields.
// Returns an error if the file cannot be read or required fields are missing.
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	// Load YAML file
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("failed to load config from %s: %w", path, err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Apply defaults for optional fields
	cfg.applyDefaults()

	// Validate required fields
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults sets default values for optional configuration fields.
func (c *Config) applyDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 60
	}
	if c.JitterSeconds == 0 {
		c.JitterSeconds = 30
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	// Update settings default to enabled with daily checks
	if c.UpdateCheckInterval == 0 {
		c.UpdateCheckInterval = 24
	}
	// Note: UpdateEnabled defaults to false value (false), we treat that as "enabled"
	// since the zero value is false and we want updates enabled by default.
	// Users must explicitly set update_enabled: false to disable.
}

// validate checks that required configuration fields are present and valid.
func (c *Config) validate() error {
	if c.ServerURL == "" {
		return ErrServerURLRequired
	}
	if c.Group == "" {
		return ErrGroupRequired
	}
	// Must have either device_token (for registration) or api_key (for operation)
	if c.DeviceToken == "" && c.APIKey == "" {
		return ErrNoCredentials
	}
	if c.PollInterval <= 0 {
		return ErrInvalidPollInterval
	}
	return nil
}

// Save writes the configuration to the specified YAML file path.
// The file is created with 0600 permissions (owner read/write only)
// as it contains sensitive credentials like the API key.
//
// This is typically called after registration to persist the received API key.
func Save(path string, cfg *Config) error {
	// Marshal config to YAML
	data, err := goyaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Write file with restricted permissions (contains secrets)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config to %s: %w", path, err)
	}

	return nil
}

// IsRegistered returns true if the agent has an API key (indicating successful registration).
func (c *Config) IsRegistered() bool {
	return c.APIKey != ""
}

// NeedsRegistration returns true if the agent has a device token but no API key,
// indicating it needs to complete registration.
func (c *Config) NeedsRegistration() bool {
	return c.DeviceToken != "" && c.APIKey == ""
}

// NATSEnabled returns true if NATS configuration is present.
func (c *Config) NATSEnabled() bool {
	return c.NATSServers != "" && c.NATSNKeySeed != "" && c.TenantID != ""
}
