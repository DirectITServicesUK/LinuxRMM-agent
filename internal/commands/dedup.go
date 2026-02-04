// dedup.go provides command deduplication for the dual delivery mechanism.
//
// When commands can arrive via both WebSocket push and HTTP polling, we need
// to ensure each command executes exactly once. This package provides a
// thread-safe set of seen command IDs with automatic size limiting.
//
// Usage:
//   - Before executing a command, call MarkSeen(id)
//   - If it returns true, the command is new and should be executed
//   - If it returns false, the command was already processed (skip it)
//
// The deduplication set uses sync.Map for thread safety and automatically
// evicts old entries when the configured limit is reached.
package commands

import (
	"log/slog"
	"sync"
	"time"
)

const (
	// defaultMaxSeen is the maximum number of command IDs to track.
	// When exceeded, oldest entries are evicted.
	defaultMaxSeen = 1000
)

// Deduplicator tracks seen command IDs to prevent duplicate execution.
// Thread-safe for concurrent use from WebSocket and poller goroutines.
type Deduplicator struct {
	seen    sync.Map // map[string]time.Time - command ID -> first seen time
	count   int
	maxSeen int
	mu      sync.Mutex // protects count and eviction
	logger  *slog.Logger
}

// NewDeduplicator creates a new command deduplicator.
func NewDeduplicator(logger *slog.Logger) *Deduplicator {
	return &Deduplicator{
		maxSeen: defaultMaxSeen,
		logger:  logger,
	}
}

// MarkSeen attempts to mark a command ID as seen.
// Returns true if the command is new (should be executed).
// Returns false if the command was already seen (should be skipped).
//
// Thread-safe: can be called concurrently from multiple goroutines.
func (d *Deduplicator) MarkSeen(commandID string) bool {
	// Try to load first (fast path - most commands are new)
	if _, loaded := d.seen.Load(commandID); loaded {
		d.logger.Debug("skipping duplicate command",
			slog.String("command_id", commandID),
		)
		return false
	}

	// Command not seen - try to store it
	d.mu.Lock()
	defer d.mu.Unlock()

	// Double-check after acquiring lock (another goroutine may have added it)
	if _, loaded := d.seen.LoadOrStore(commandID, time.Now()); loaded {
		d.logger.Debug("skipping duplicate command",
			slog.String("command_id", commandID),
		)
		return false
	}

	// Successfully stored - increment count
	d.count++

	// Evict old entries if we've exceeded the limit
	if d.count > d.maxSeen {
		d.evictOldest()
	}

	return true
}

// evictOldest removes the oldest entries to make room for new ones.
// Caller must hold d.mu lock.
func (d *Deduplicator) evictOldest() {
	// Find and remove oldest entries until we're under limit
	// Simple approach: remove 10% of entries when limit exceeded
	toRemove := d.maxSeen / 10
	if toRemove < 1 {
		toRemove = 1
	}

	type entry struct {
		id   string
		time time.Time
	}

	// Collect all entries to find oldest
	var entries []entry
	d.seen.Range(func(key, value any) bool {
		entries = append(entries, entry{
			id:   key.(string),
			time: value.(time.Time),
		})
		return true
	})

	// Sort by time (oldest first) - simple bubble for small N
	for i := 0; i < len(entries)-1; i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].time.Before(entries[i].time) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	// Remove oldest entries
	removed := 0
	for i := 0; i < len(entries) && removed < toRemove; i++ {
		d.seen.Delete(entries[i].id)
		removed++
		d.count--
	}

	d.logger.Debug("evicted old command IDs",
		slog.Int("removed", removed),
		slog.Int("remaining", d.count),
	)
}

// IsSeen checks if a command ID has been seen without marking it.
// Useful for debugging/testing.
func (d *Deduplicator) IsSeen(commandID string) bool {
	_, exists := d.seen.Load(commandID)
	return exists
}

// Count returns the current number of tracked command IDs.
func (d *Deduplicator) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}
