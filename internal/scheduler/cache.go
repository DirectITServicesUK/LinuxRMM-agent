// Package scheduler provides local schedule execution for offline operation.
// This file implements persistent schedule cache using bbolt for offline execution.

package scheduler

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

const scheduleBucket = "schedules"

// CachedSchedule represents a schedule stored locally for offline execution
type CachedSchedule struct {
	ID              string         `json:"id"`
	ScriptContent   string         `json:"script_content"`
	Interpreter     string         `json:"interpreter"`
	CronExpression  string         `json:"cron_expression,omitempty"`
	IntervalMinutes int            `json:"interval_minutes,omitempty"`
	Variables       map[string]any `json:"variables"`
	OutputRetention string         `json:"output_retention"`
	NextRunAt       time.Time      `json:"next_run_at"`
	LastSyncAt      time.Time      `json:"last_sync_at"`
}

// ScheduleCache provides persistent storage for schedules
type ScheduleCache struct {
	db     *bolt.DB
	parser *CronParser
}

// NewScheduleCache opens or creates the schedule cache database
func NewScheduleCache(dbPath string) (*ScheduleCache, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	// Ensure bucket exists
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(scheduleBucket))
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &ScheduleCache{
		db:     db,
		parser: NewCronParser(),
	}, nil
}

// SaveSchedule stores or updates a schedule
func (c *ScheduleCache) SaveSchedule(s *CachedSchedule) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(scheduleBucket))
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return b.Put([]byte(s.ID), data)
	})
}

// DeleteSchedule removes a schedule by ID
func (c *ScheduleCache) DeleteSchedule(id string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(scheduleBucket))
		return b.Delete([]byte(id))
	})
}

// GetSchedule retrieves a schedule by ID
func (c *ScheduleCache) GetSchedule(id string) (*CachedSchedule, error) {
	var schedule CachedSchedule
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(scheduleBucket))
		data := b.Get([]byte(id))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &schedule)
	})
	if err != nil {
		return nil, err
	}
	if schedule.ID == "" {
		return nil, nil
	}
	return &schedule, nil
}

// GetDueSchedules returns schedules that are due for execution
func (c *ScheduleCache) GetDueSchedules() ([]*CachedSchedule, error) {
	var due []*CachedSchedule
	now := time.Now()

	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(scheduleBucket))
		return b.ForEach(func(k, v []byte) error {
			var s CachedSchedule
			if err := json.Unmarshal(v, &s); err != nil {
				return nil // Skip invalid entries
			}
			if s.NextRunAt.Before(now) || s.NextRunAt.Equal(now) {
				due = append(due, &s)
			}
			return nil
		})
	})

	return due, err
}

// GetAllSchedules returns all cached schedules
func (c *ScheduleCache) GetAllSchedules() ([]*CachedSchedule, error) {
	var schedules []*CachedSchedule

	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(scheduleBucket))
		return b.ForEach(func(k, v []byte) error {
			var s CachedSchedule
			if err := json.Unmarshal(v, &s); err != nil {
				return nil
			}
			schedules = append(schedules, &s)
			return nil
		})
	})

	return schedules, err
}

// Close closes the database
func (c *ScheduleCache) Close() error {
	return c.db.Close()
}

// CalculateNextRun determines the next execution time for a schedule
func (c *ScheduleCache) CalculateNextRun(s *CachedSchedule) time.Time {
	if s.CronExpression != "" {
		next, err := c.parser.NextRun(s.CronExpression, time.Now())
		if err != nil {
			return time.Now().Add(time.Hour) // Fallback
		}
		return next
	}
	// Interval-based
	return time.Now().Add(time.Duration(s.IntervalMinutes) * time.Minute)
}
