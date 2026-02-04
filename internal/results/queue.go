// Package results provides a persistent queue for schedule execution results.
// Results are queued locally and uploaded when the server is reachable.

package results

import (
	"encoding/binary"
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

const resultsBucket = "pending_results"

// PendingResult represents a schedule execution result awaiting upload
type PendingResult struct {
	ID         uint64    `json:"id"`
	ScheduleID string    `json:"schedule_id"`
	ExecutedAt time.Time `json:"executed_at"`
	ExitCode   int       `json:"exit_code"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	TimedOut   bool      `json:"timed_out"`
}

// ResultQueue provides persistent storage for pending results
type ResultQueue struct {
	db *bolt.DB
}

// NewResultQueue opens or creates the result queue database
func NewResultQueue(dbPath string) (*ResultQueue, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(resultsBucket))
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &ResultQueue{db: db}, nil
}

// Enqueue adds a result to the queue
func (q *ResultQueue) Enqueue(r *PendingResult) error {
	return q.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resultsBucket))

		// Auto-increment ID
		id, _ := b.NextSequence()
		r.ID = id

		data, err := json.Marshal(r)
		if err != nil {
			return err
		}

		return b.Put(itob(id), data)
	})
}

// Dequeue retrieves up to limit results from the queue (oldest first)
func (q *ResultQueue) Dequeue(limit int) ([]*PendingResult, error) {
	var results []*PendingResult

	err := q.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resultsBucket))
		c := b.Cursor()

		for k, v := c.First(); k != nil && len(results) < limit; k, v = c.Next() {
			var r PendingResult
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			results = append(results, &r)
		}
		return nil
	})

	return results, err
}

// Remove deletes results by ID after successful upload
func (q *ResultQueue) Remove(ids []uint64) error {
	return q.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resultsBucket))
		for _, id := range ids {
			if err := b.Delete(itob(id)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Count returns the number of pending results
func (q *ResultQueue) Count() (int, error) {
	var count int
	err := q.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resultsBucket))
		count = b.Stats().KeyN
		return nil
	})
	return count, err
}

// Close closes the database
func (q *ResultQueue) Close() error {
	return q.db.Close()
}

// itob converts uint64 to big-endian bytes for ordered keys
func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
