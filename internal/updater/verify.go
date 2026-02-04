// verify.go - SHA256 checksum verification for downloaded binaries.
// Critical for update security - prevents installation of corrupted or tampered binaries.
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// VerifyChecksum computes the SHA256 checksum of a file and compares it
// to the expected value. Returns nil if checksum matches, error otherwise.
//
// IMPORTANT: This function should only be called after the file has been
// completely written and fsynced to disk. Calling it on a file that's still
// being written may produce incorrect results.
func VerifyChecksum(filePath string, expectedSum string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}

	computedSum := hex.EncodeToString(h.Sum(nil))

	// Normalize both to lowercase for comparison
	expectedSum = strings.ToLower(strings.TrimSpace(expectedSum))
	computedSum = strings.ToLower(computedSum)

	if computedSum != expectedSum {
		return &ChecksumMismatchError{
			Expected: expectedSum,
			Computed: computedSum,
		}
	}

	return nil
}

// ChecksumMismatchError is returned when the computed checksum doesn't match expected.
type ChecksumMismatchError struct {
	Expected string
	Computed string
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("checksum mismatch: expected %s, got %s", e.Expected, e.Computed)
}

// ComputeChecksum calculates the SHA256 checksum of a file.
// Returns the checksum as a lowercase hex string.
func ComputeChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("compute checksum: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
