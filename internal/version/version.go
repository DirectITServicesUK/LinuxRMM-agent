// Package version provides build-time version information for the RMM agent.
// Version, Commit, and BuildTime are populated via ldflags during the build process.
// For development builds, default values are used.
package version

// Build information variables, set via ldflags at build time:
//
//	go build -ldflags "-X github.com/doughall/linuxrmm/agent/internal/version.Version=1.0.0 \
//	                   -X github.com/doughall/linuxrmm/agent/internal/version.Commit=abc123 \
//	                   -X github.com/doughall/linuxrmm/agent/internal/version.BuildTime=2025-01-29T12:00:00Z"
var (
	// Version is the semantic version of the agent (e.g., "1.0.0", "dev").
	Version = "dev"

	// Commit is the git commit hash from which the binary was built.
	Commit = "unknown"

	// BuildTime is the timestamp when the binary was built (RFC3339 format).
	BuildTime = "unknown"
)

// Info returns a formatted string with all version information.
func Info() string {
	return "rmm-agent " + Version + " (commit: " + Commit + ", built: " + BuildTime + ")"
}
