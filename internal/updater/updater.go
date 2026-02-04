// updater.go - Complete update orchestration for self-update functionality.
// Handles the full update lifecycle: check -> download -> backup -> replace -> restart.
// The Updater coordinates all update components and manages the complete update flow.
package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/version"
)

// Note: UpdateMarkerPath constant is defined in health.go and shared across the package.

// Updater orchestrates the complete self-update process.
type Updater struct {
	serverURL  string
	agentID    string
	apiKey     string
	binaryPath string

	checker    *Checker
	downloader *Downloader
	backup     *BackupManager

	logger *slog.Logger
}

// Config holds configuration for the updater.
type Config struct {
	ServerURL      string
	AgentID        string
	APIKey         string
	BinaryPath     string
	BackupDir      string
	CheckIntervalH int
	MaxBackups     int
}

// NewUpdater creates a new updater with all components initialized.
func NewUpdater(cfg *Config, logger *slog.Logger) (*Updater, error) {
	if cfg.BinaryPath == "" {
		// Default to current executable path
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("get executable path: %w", err)
		}
		cfg.BinaryPath = exe
	}

	if cfg.BackupDir == "" {
		cfg.BackupDir = "/var/lib/rmm-agent/backups"
	}

	backup, err := NewBackupManager(cfg.BackupDir, cfg.MaxBackups, logger)
	if err != nil {
		return nil, fmt.Errorf("create backup manager: %w", err)
	}

	downloader := NewDownloader(logger)

	u := &Updater{
		serverURL:  cfg.ServerURL,
		agentID:    cfg.AgentID,
		apiKey:     cfg.APIKey,
		binaryPath: cfg.BinaryPath,
		downloader: downloader,
		backup:     backup,
		logger:     logger.With(slog.String("component", "updater")),
	}

	// Create checker with callback that triggers update application
	u.checker = NewChecker(
		cfg.ServerURL,
		cfg.AgentID,
		cfg.APIKey,
		version.Version,
		cfg.CheckIntervalH,
		func(manifest Manifest) {
			// Run update in background - don't block the checker
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				if err := u.ApplyUpdate(ctx, &manifest); err != nil {
					u.logger.Error("failed to apply update",
						slog.String("version", manifest.Version),
						slog.String("error", err.Error()),
					)
				}
			}()
		},
		logger,
	)

	return u, nil
}

// Run starts the update checker loop.
func (u *Updater) Run(ctx context.Context) {
	u.checker.Run(ctx)
}

// Shutdown implements shutdown.Shutdowner for coordinated shutdown.
func (u *Updater) Shutdown(ctx context.Context) error {
	u.checker.Stop()
	return nil
}

// ApplyUpdate performs the complete update process:
// 1. Download and verify new binary
// 2. Backup current binary
// 3. Atomic replacement
// 4. Write update marker
// 5. Exit for systemd restart
func (u *Updater) ApplyUpdate(ctx context.Context, manifest *Manifest) error {
	u.logger.Info("starting update",
		slog.String("current_version", version.Version),
		slog.String("new_version", manifest.Version),
	)

	// Build full download URL
	downloadURL := u.serverURL + manifest.DownloadURL

	// 1. Download and verify new binary
	result, err := u.downloader.DownloadWithRetry(
		ctx,
		downloadURL,
		u.binaryPath,
		manifest.SHA256,
		u.apiKey,
		3, // max retries
	)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() {
		// Clean up temp file if we don't succeed
		if result != nil && result.TempPath != "" {
			os.Remove(result.TempPath)
		}
	}()

	u.logger.Info("binary downloaded and verified",
		slog.Int64("size", result.Size),
		slog.String("temp_path", result.TempPath),
	)

	// 2. Backup current binary
	backupPath, err := u.backup.BackupCurrent(version.Version, u.binaryPath)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	u.logger.Info("current binary backed up",
		slog.String("backup_path", backupPath),
	)

	// 3. Set executable permission on temp file
	if err := os.Chmod(result.TempPath, 0755); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// 4. Replace binary
	// Try atomic rename first (works on same filesystem)
	// Fall back to copy if rename fails (cross-device link error)
	if err := os.Rename(result.TempPath, u.binaryPath); err != nil {
		u.logger.Debug("rename failed, falling back to copy",
			slog.String("error", err.Error()),
		)
		if err := copyFile(result.TempPath, u.binaryPath); err != nil {
			return fmt.Errorf("replace binary failed: %w", err)
		}
		// Remove temp file after successful copy
		os.Remove(result.TempPath)
	}

	// Clear temp path so defer doesn't try to remove it
	result.TempPath = ""

	u.logger.Info("binary replaced successfully",
		slog.String("path", u.binaryPath),
	)

	// 5. Write update marker file for health check system
	if err := u.writeUpdateMarker(manifest.Version); err != nil {
		u.logger.Warn("failed to write update marker",
			slog.String("error", err.Error()),
		)
		// Don't fail the update for this - it's not critical
	}

	// 6. Log success and exit for systemd restart
	u.logger.Info("update complete, exiting for restart",
		slog.String("new_version", manifest.Version),
	)

	// Give logs time to flush
	time.Sleep(100 * time.Millisecond)

	// Exit cleanly - systemd will restart with new binary
	// Exit code 0 indicates clean shutdown, systemd Restart=on-failure
	// won't trigger unless we exit with non-zero
	// Use exit code 0 so systemd knows this is intentional
	os.Exit(0)

	return nil // Never reached, but required for compilation
}

// writeUpdateMarker writes a marker file indicating a recent update.
// The health checker uses this to determine if startup failure should trigger rollback.
func (u *Updater) writeUpdateMarker(newVersion string) error {
	// Ensure directory exists
	dir := filepath.Dir(UpdateMarkerPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write marker with timestamp and version
	content := fmt.Sprintf("%s\n%s\n", time.Now().Format(time.RFC3339), newVersion)
	return os.WriteFile(UpdateMarkerPath, []byte(content), 0644)
}

// GetBackupManager returns the backup manager for health check rollback.
func (u *Updater) GetBackupManager() *BackupManager {
	return u.backup
}

// GetChecker returns the update checker for manual checks.
func (u *Updater) GetChecker() *Checker {
	return u.checker
}

// Note: WasRecentUpdate and ClearUpdateMarker are defined in health.go

// copyFile copies src to dst, preserving permissions.
// Used as fallback when rename fails due to cross-device link.
func copyFile(src, dst string) error {
	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	// Get source file info for permissions
	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Create destination file (truncate if exists)
	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer dstFile.Close()

	// Copy contents
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("copy contents: %w", err)
	}

	// Sync to disk
	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("sync destination: %w", err)
	}

	return nil
}
