// Package logging provides structured logging configuration for the RMM agent.
//
// Logging Strategy:
// - JSON format for systemd journald compatibility and easy parsing
// - Source locations included for debugging (file:line)
// - Log levels configurable via config file (debug, info, warn, error)
// - Default logger set globally for convenience, also returned for explicit passing
//
// Usage:
//
//	logger := logging.SetupLogger("info")
//	logger.Info("action description", "key", value, "component", "poller")
//
// Context-aware logging:
//
//	logger.InfoContext(ctx, "processing request", "request_id", reqID)
package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// SetupLogger creates and configures a structured JSON logger.
// The level parameter accepts: "debug", "info", "warn", "error" (case-insensitive).
// Invalid levels default to "info".
//
// The logger is configured to:
// - Output JSON to stdout (journald compatible)
// - Include source file and line numbers
// - Use the specified log level
//
// The logger is also set as the default via slog.SetDefault, allowing
// use of the global slog.Info(), slog.Error(), etc. functions.
func SetupLogger(level string) *slog.Logger {
	slogLevel := parseLevel(level)

	opts := &slog.HandlerOptions{
		Level:     slogLevel,
		AddSource: true, // Include file:line for debugging
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Shorten source paths by removing the module prefix
			if a.Key == slog.SourceKey {
				if source, ok := a.Value.Any().(*slog.Source); ok {
					// Shorten file path: extract from internal/ onwards
					if idx := strings.Index(source.File, "internal/"); idx != -1 {
						source.File = source.File[idx:]
					} else {
						source.File = filepath.Base(source.File)
					}
					// Shorten function name: extract from internal/ onwards
					if idx := strings.Index(source.Function, "internal/"); idx != -1 {
						source.Function = source.Function[idx:]
					}
				}
			}
			return a
		},
	}

	// JSON handler for structured logging (journald compatible)
	handler := slog.NewJSONHandler(os.Stdout, opts)
	logger := slog.New(handler)

	// Set as default for global access via slog.Info(), slog.Error(), etc.
	slog.SetDefault(logger)

	return logger
}

// parseLevel converts a string log level to slog.Level.
// Accepts: "debug", "info", "warn", "error" (case-insensitive).
// Returns slog.LevelInfo for unrecognized values.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithComponent returns a logger with a pre-set component attribute.
// Useful for tagging all logs from a specific subsystem.
//
// Usage:
//
//	pollerLog := logging.WithComponent(logger, "poller")
//	pollerLog.Info("polling started") // includes "component": "poller"
func WithComponent(logger *slog.Logger, component string) *slog.Logger {
	return logger.With(slog.String("component", component))
}
