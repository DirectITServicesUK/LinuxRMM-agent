// interpreter_test.go tests interpreter verification logic.
// It validates allowlist enforcement, LookPath behavior, caching, and thread safety.
package executor

import (
	"strings"
	"sync"
	"testing"
)

func TestVerifyInterpreter_ValidBash(t *testing.T) {
	// bash should exist on any Linux/macOS system
	path, err := VerifyInterpreter("bash")
	if err != nil {
		t.Fatalf("expected bash to be found, got error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	if !strings.HasSuffix(path, "bash") {
		t.Errorf("expected path ending in bash, got: %s", path)
	}
}

func TestVerifyInterpreter_ValidSh(t *testing.T) {
	// sh should exist on any POSIX system
	path, err := VerifyInterpreter("sh")
	if err != nil {
		t.Fatalf("expected sh to be found, got error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
}

func TestVerifyInterpreter_InvalidInterpreter(t *testing.T) {
	// Invalid interpreter should be rejected
	_, err := VerifyInterpreter("ruby")
	if err == nil {
		t.Fatal("expected error for invalid interpreter")
	}
	if !strings.Contains(err.Error(), "invalid interpreter") {
		t.Errorf("expected 'invalid interpreter' error, got: %v", err)
	}
}

func TestVerifyInterpreter_EmptyString(t *testing.T) {
	// Empty string should be rejected
	_, err := VerifyInterpreter("")
	if err == nil {
		t.Fatal("expected error for empty interpreter")
	}
	if !strings.Contains(err.Error(), "invalid interpreter") {
		t.Errorf("expected 'invalid interpreter' error, got: %v", err)
	}
}

func TestVerifyInterpreter_CaseSensitive(t *testing.T) {
	// Interpreter names are case-sensitive (BASH is not bash)
	_, err := VerifyInterpreter("BASH")
	if err == nil {
		t.Fatal("expected error for case-mismatched interpreter")
	}
	if !strings.Contains(err.Error(), "invalid interpreter") {
		t.Errorf("expected 'invalid interpreter' error, got: %v", err)
	}
}

func TestInterpreterCache_Caching(t *testing.T) {
	cache := NewInterpreterCache()

	// First lookup should use LookPath
	path1, err := cache.VerifyInterpreter("bash")
	if err != nil {
		t.Fatalf("first lookup failed: %v", err)
	}

	// Second lookup should use cache and return same result
	path2, err := cache.VerifyInterpreter("bash")
	if err != nil {
		t.Fatalf("cached lookup failed: %v", err)
	}
	if path1 != path2 {
		t.Errorf("cache returned different path: %s vs %s", path1, path2)
	}
}

func TestInterpreterCache_Concurrent(t *testing.T) {
	cache := NewInterpreterCache()

	// Run concurrent lookups to test thread safety
	var wg sync.WaitGroup
	errors := make(chan error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := cache.VerifyInterpreter("bash")
			if err != nil {
				errors <- err
				return
			}
			if path == "" {
				errors <- err
			}
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Errorf("concurrent access error: %v", err)
		}
	}
}

func TestIsValidInterpreter(t *testing.T) {
	tests := []struct {
		interpreter string
		want        bool
	}{
		{"bash", true},
		{"sh", true},
		{"python", true},
		{"perl", true},
		{"ruby", false},
		{"node", false},
		{"", false},
		{"BASH", false},   // case sensitive
		{"Bash", false},   // case sensitive
		{"bash ", false},  // no trailing space normalization
		{" bash", false},  // no leading space normalization
	}

	for _, tt := range tests {
		got := isValidInterpreter(tt.interpreter)
		if got != tt.want {
			t.Errorf("isValidInterpreter(%q) = %v, want %v", tt.interpreter, got, tt.want)
		}
	}
}

func TestValidInterpreters(t *testing.T) {
	// Verify the allowlist contains expected interpreters
	expected := map[string]bool{
		"bash":   false,
		"sh":     false,
		"python": false,
		"perl":   false,
	}

	for _, interp := range ValidInterpreters {
		if _, ok := expected[interp]; !ok {
			t.Errorf("unexpected interpreter in allowlist: %s", interp)
		}
		expected[interp] = true
	}

	for interp, found := range expected {
		if !found {
			t.Errorf("expected interpreter missing from allowlist: %s", interp)
		}
	}
}

func TestNewInterpreterCache(t *testing.T) {
	cache := NewInterpreterCache()
	if cache == nil {
		t.Fatal("NewInterpreterCache returned nil")
	}
	if cache.cache == nil {
		t.Fatal("cache map not initialized")
	}
}
