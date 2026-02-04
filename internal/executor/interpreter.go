// interpreter.go verifies that required script interpreters exist on the system.
// It uses exec.LookPath to search $PATH for interpreter binaries, failing fast
// if a script's interpreter is not available rather than failing at execution time.
package executor

import (
	"fmt"
	"os/exec"
	"sync"
)

// ValidInterpreters defines the allowlist of supported script interpreters.
// Only these interpreters can be used for script execution.
var ValidInterpreters = []string{"bash", "sh", "python", "perl"}

// InterpreterCache caches interpreter paths to avoid repeated lookups.
type InterpreterCache struct {
	mu    sync.RWMutex
	cache map[string]string
}

// NewInterpreterCache creates a new interpreter path cache.
func NewInterpreterCache() *InterpreterCache {
	return &InterpreterCache{
		cache: make(map[string]string),
	}
}

// VerifyInterpreter checks if the specified interpreter exists and returns its absolute path.
// Returns error if interpreter is not in allowlist or not found in PATH.
func (c *InterpreterCache) VerifyInterpreter(interpreter string) (string, error) {
	// Validate interpreter is in allowlist
	if !isValidInterpreter(interpreter) {
		return "", fmt.Errorf("invalid interpreter: %s (allowed: bash, sh, python, perl)", interpreter)
	}

	// Check cache first (read lock)
	c.mu.RLock()
	if path, ok := c.cache[interpreter]; ok {
		c.mu.RUnlock()
		return path, nil
	}
	c.mu.RUnlock()

	// Use exec.LookPath to search $PATH
	path, err := exec.LookPath(interpreter)
	if err != nil {
		return "", fmt.Errorf("interpreter '%s' not found in PATH: %w", interpreter, err)
	}

	// Cache for future use (write lock)
	c.mu.Lock()
	c.cache[interpreter] = path
	c.mu.Unlock()

	return path, nil
}

// isValidInterpreter checks if interpreter is in the allowlist.
func isValidInterpreter(interpreter string) bool {
	for _, valid := range ValidInterpreters {
		if interpreter == valid {
			return true
		}
	}
	return false
}

// globalCache is the default interpreter cache for the package-level function.
var globalCache = NewInterpreterCache()

// VerifyInterpreter is a convenience function using a global cache.
// For production use with custom lifecycle requirements, prefer creating an InterpreterCache instance.
func VerifyInterpreter(interpreter string) (string, error) {
	return globalCache.VerifyInterpreter(interpreter)
}
