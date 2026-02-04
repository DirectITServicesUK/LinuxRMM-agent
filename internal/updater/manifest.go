// Package updater provides self-update functionality for the RMM agent.
// It implements the client-side of the update system, checking for available
// updates from the server and coordinating the update process.
package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// Manifest represents an update manifest response from the server.
// It contains information about whether an update is available and
// the details needed to download and verify the update binary.
type Manifest struct {
	// UpdateAvailable indicates if an update is available for this agent.
	// The server determines this based on version comparison, group targeting,
	// and percentage-based staged rollout.
	UpdateAvailable bool `json:"updateAvailable"`

	// Version is the semantic version of the available update (e.g., "1.2.3").
	// Empty if UpdateAvailable is false.
	Version string `json:"version"`

	// DownloadURL is the full URL to download the update binary.
	// Empty if UpdateAvailable is false.
	DownloadURL string `json:"downloadUrl"`

	// SHA256 is the hex-encoded SHA256 checksum of the update binary.
	// Used to verify integrity of the downloaded file.
	// Empty if UpdateAvailable is false.
	SHA256 string `json:"sha256"`
}

// SemVer represents a semantic version (major.minor.patch).
// It provides parsing and comparison functionality for version strings.
type SemVer struct {
	Major int
	Minor int
	Patch int
}

// ParseSemVer parses a semantic version string like "1.2.3" into a SemVer.
// Returns an error if the string is not a valid semantic version.
func ParseSemVer(version string) (SemVer, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("invalid semver format: %s (expected major.minor.patch)", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid major version: %s", parts[0])
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid minor version: %s", parts[1])
	}

	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid patch version: %s", parts[2])
	}

	return SemVer{
		Major: major,
		Minor: minor,
		Patch: patch,
	}, nil
}

// Compare compares this version to another version.
// Returns:
//   -1 if this version is less than the other version
//    0 if this version equals the other version
//    1 if this version is greater than the other version
func (v SemVer) Compare(other SemVer) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}

	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}

	if v.Patch != other.Patch {
		if v.Patch < other.Patch {
			return -1
		}
		return 1
	}

	return 0
}

// String returns the semantic version as a string (e.g., "1.2.3").
func (v SemVer) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// NeedsUpdate safely compares the current version against the manifest version
// and returns true if an update should be applied.
// It returns false if:
//   - UpdateAvailable is false in the manifest
//   - The manifest version is invalid
//   - The current version is invalid
//   - The current version is >= the manifest version (prevents downgrades)
func NeedsUpdate(currentVersion string, manifest Manifest) bool {
	// Server says no update available
	if !manifest.UpdateAvailable {
		return false
	}

	// Parse versions
	current, err := ParseSemVer(currentVersion)
	if err != nil {
		// Can't parse current version - don't update
		return false
	}

	target, err := ParseSemVer(manifest.Version)
	if err != nil {
		// Can't parse target version - don't update
		return false
	}

	// Only update if target version is newer
	// This prevents downgrade attacks where a compromised server
	// tries to push an older vulnerable version
	return current.Compare(target) < 0
}
