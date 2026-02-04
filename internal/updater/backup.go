// backup.go - Binary version backup and restore for rollback support.
// Maintains last N binary versions to enable rollback on failed updates.
package updater

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultMaxBackups is the default number of backup versions to keep.
// 7 days of backups allows for delayed bug discovery and rollback.
const DefaultMaxBackups = 7

// BackupManager handles binary version backups for rollback support.
type BackupManager struct {
	// backupDir is where backup binaries are stored
	backupDir string
	// maxBackups is the maximum number of backups to keep
	maxBackups int
	// logger for backup operations
	logger *slog.Logger
}

// NewBackupManager creates a new backup manager.
// backupDir is the directory to store backups (e.g., /var/lib/rmm-agent/backups).
// maxBackups is the number of backups to keep (default: 7).
func NewBackupManager(backupDir string, maxBackups int, logger *slog.Logger) (*BackupManager, error) {
	if maxBackups <= 0 {
		maxBackups = DefaultMaxBackups
	}

	// Ensure backup directory exists
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}

	return &BackupManager{
		backupDir:  backupDir,
		maxBackups: maxBackups,
		logger:     logger.With(slog.String("component", "backup")),
	}, nil
}

// BackupCurrent creates a backup of the current binary.
// version is the version string of the current binary (for naming).
// binaryPath is the path to the current binary to backup.
// Returns the path to the backup file.
func (m *BackupManager) BackupCurrent(version string, binaryPath string) (string, error) {
	// Create backup filename with version and timestamp
	// Format: rmm-agent.{version}.{timestamp}
	timestamp := time.Now().Unix()
	backupName := fmt.Sprintf("rmm-agent.%s.%d", sanitizeVersion(version), timestamp)
	backupPath := filepath.Join(m.backupDir, backupName)

	m.logger.Info("creating backup",
		slog.String("version", version),
		slog.String("source", binaryPath),
		slog.String("backup", backupPath),
	)

	// Copy current binary to backup location
	if err := copyFile(binaryPath, backupPath); err != nil {
		return "", fmt.Errorf("copy binary to backup: %w", err)
	}

	// Verify backup was created successfully
	info, err := os.Stat(backupPath)
	if err != nil {
		return "", fmt.Errorf("verify backup: %w", err)
	}

	m.logger.Info("backup created",
		slog.String("path", backupPath),
		slog.Int64("size", info.Size()),
	)

	// Prune old backups
	if err := m.pruneOldBackups(); err != nil {
		// Log warning but don't fail - backup was successful
		m.logger.Warn("failed to prune old backups",
			slog.String("error", err.Error()),
		)
	}

	return backupPath, nil
}

// RestoreLatest restores the most recent backup to the target path.
// This is used for rollback when a new version fails health checks.
func (m *BackupManager) RestoreLatest(targetPath string) error {
	backups, err := m.listBackups()
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}

	if len(backups) == 0 {
		return fmt.Errorf("no backups available for rollback")
	}

	// Latest backup is last in sorted list
	latestBackup := backups[len(backups)-1]

	m.logger.Info("restoring from backup",
		slog.String("backup", latestBackup),
		slog.String("target", targetPath),
	)

	if err := copyFile(latestBackup, targetPath); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}

	// Set executable permission
	if err := os.Chmod(targetPath, 0755); err != nil {
		return fmt.Errorf("set executable permission: %w", err)
	}

	m.logger.Info("backup restored successfully")
	return nil
}

// HasBackups returns true if there are any backups available.
func (m *BackupManager) HasBackups() bool {
	backups, err := m.listBackups()
	if err != nil {
		return false
	}
	return len(backups) > 0
}

// GetLatestBackupVersion returns the version of the latest backup.
// Returns empty string if no backups exist.
func (m *BackupManager) GetLatestBackupVersion() string {
	backups, err := m.listBackups()
	if err != nil || len(backups) == 0 {
		return ""
	}

	// Parse version from filename: rmm-agent.{version}.{timestamp}
	latest := filepath.Base(backups[len(backups)-1])
	parts := strings.Split(latest, ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// listBackups returns all backup files sorted by timestamp (oldest first).
func (m *BackupManager) listBackups() ([]string, error) {
	entries, err := os.ReadDir(m.backupDir)
	if err != nil {
		return nil, err
	}

	var backups []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), "rmm-agent.") {
			backups = append(backups, filepath.Join(m.backupDir, entry.Name()))
		}
	}

	// Sort by filename (which includes timestamp for chronological order)
	sort.Strings(backups)
	return backups, nil
}

// pruneOldBackups removes backups beyond the maxBackups limit.
func (m *BackupManager) pruneOldBackups() error {
	backups, err := m.listBackups()
	if err != nil {
		return err
	}

	if len(backups) <= m.maxBackups {
		return nil
	}

	// Remove oldest backups (at beginning of sorted list)
	toRemove := backups[:len(backups)-m.maxBackups]
	for _, path := range toRemove {
		m.logger.Debug("removing old backup",
			slog.String("path", path),
		)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove old backup %s: %w", path, err)
		}
	}

	m.logger.Info("pruned old backups",
		slog.Int("removed", len(toRemove)),
		slog.Int("remaining", m.maxBackups),
	)

	return nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	// Get source file permissions
	info, err := source.Stat()
	if err != nil {
		return err
	}

	// Create destination with same permissions
	dest, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer dest.Close()

	// Copy content
	if _, err := io.Copy(dest, source); err != nil {
		return err
	}

	// Sync to disk
	return dest.Sync()
}

// sanitizeVersion removes characters that aren't safe for filenames.
func sanitizeVersion(version string) string {
	// Replace unsafe characters with underscore
	unsafe := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	result := version
	for _, char := range unsafe {
		result = strings.ReplaceAll(result, char, "_")
	}
	return result
}
