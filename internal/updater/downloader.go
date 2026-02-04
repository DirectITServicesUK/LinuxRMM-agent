// downloader.go - Binary download with atomic write and verification.
// Downloads to temp file on same filesystem for atomic rename.
package updater

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// Downloader handles downloading and verifying update binaries.
type Downloader struct {
	httpClient *http.Client
	logger     *slog.Logger
}

// NewDownloader creates a new binary downloader.
func NewDownloader(logger *slog.Logger) *Downloader {
	return &Downloader{
		httpClient: &http.Client{
			// No timeout here - large binaries may take time
			// Context handles cancellation
		},
		logger: logger.With(slog.String("component", "downloader")),
	}
}

// DownloadResult contains the result of a download operation.
type DownloadResult struct {
	// TempPath is the path to the downloaded temp file (verified)
	TempPath string
	// Size is the number of bytes downloaded
	Size int64
}

// Download downloads a binary from the given URL, verifies its checksum,
// and returns the path to the verified temp file.
//
// The temp file is created in the same directory as targetPath to ensure
// atomic rename will work (same filesystem requirement).
//
// Caller is responsible for either:
//   - Renaming the temp file to replace the target (on success)
//   - Removing the temp file (on failure or abort)
//
// CRITICAL: This function performs fsync before checksum verification
// to prevent partial write corruption from causing false verification.
func (d *Downloader) Download(ctx context.Context, url string, targetPath string, expectedSHA256 string, apiKey string) (*DownloadResult, error) {
	d.logger.Info("starting download",
		slog.String("url", url),
		slog.String("target", targetPath),
	)

	// Create temp file in same directory as target (critical for atomic rename)
	targetDir := filepath.Dir(targetPath)
	tmpFile, err := os.CreateTemp(targetDir, ".rmm-agent-update-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Cleanup on any error
	success := false
	defer func() {
		tmpFile.Close()
		if !success {
			os.Remove(tmpPath)
		}
	}()

	// Create HTTP request with context for cancellation
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set auth header if provided
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// Execute download
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Copy response body to temp file
	written, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("write to temp file: %w", err)
	}

	d.logger.Debug("download complete",
		slog.Int64("bytes", written),
		slog.String("temp_path", tmpPath),
	)

	// CRITICAL: Sync to disk before verification
	// Without this, power loss could corrupt the file and verification
	// might pass on cached (incomplete) data
	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync to disk: %w", err)
	}

	// Close before verification (releases file handle)
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}

	// Verify checksum
	d.logger.Debug("verifying checksum",
		slog.String("expected", expectedSHA256),
	)

	if err := VerifyChecksum(tmpPath, expectedSHA256); err != nil {
		return nil, fmt.Errorf("checksum verification failed: %w", err)
	}

	d.logger.Info("download verified successfully",
		slog.Int64("size", written),
		slog.String("sha256", expectedSHA256[:16]+"..."),
	)

	success = true
	return &DownloadResult{
		TempPath: tmpPath,
		Size:     written,
	}, nil
}

// DownloadWithRetry wraps Download with retry logic for transient failures.
func (d *Downloader) DownloadWithRetry(ctx context.Context, url string, targetPath string, expectedSHA256 string, apiKey string, maxRetries int) (*DownloadResult, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			d.logger.Warn("retrying download",
				slog.Int("attempt", attempt),
				slog.String("last_error", lastErr.Error()),
			)
		}

		result, err := d.Download(ctx, url, targetPath, expectedSHA256, apiKey)
		if err == nil {
			return result, nil
		}

		lastErr = err

		// Don't retry on checksum mismatch (likely corrupted server-side)
		if _, ok := err.(*ChecksumMismatchError); ok {
			return nil, err
		}

		// Don't retry on context cancellation
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("download failed after %d attempts: %w", maxRetries+1, lastErr)
}
