// Package scheduler provides local schedule execution for offline operation.
// This file wraps robfig/cron for cron expression parsing without running a scheduler.

package scheduler

import (
	"time"

	"github.com/robfig/cron/v3"
)

// CronParser wraps robfig/cron for schedule-only usage
type CronParser struct {
	parser cron.Parser
}

// NewCronParser creates a parser supporting standard 5-field cron with descriptors
func NewCronParser() *CronParser {
	return &CronParser{
		parser: cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		),
	}
}

// NextRun calculates the next execution time for a cron expression
func (p *CronParser) NextRun(expression string, after time.Time) (time.Time, error) {
	schedule, err := p.parser.Parse(expression)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(after), nil
}

// Validate checks if a cron expression is valid
func (p *CronParser) Validate(expression string) error {
	_, err := p.parser.Parse(expression)
	return err
}
