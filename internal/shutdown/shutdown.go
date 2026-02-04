// Package shutdown provides coordinated shutdown for multiple components.
// It ensures components are shut down in reverse order of registration,
// allowing dependent components to stop gracefully before their dependencies.
//
// Usage:
//
//	coord := shutdown.NewCoordinator(logger)
//	coord.Register("poller", poller)
//	coord.Register("database", database)
//	// On shutdown:
//	coord.Shutdown(ctx) // Shuts down database first, then poller
package shutdown

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Shutdowner is the interface that components must implement to participate
// in coordinated shutdown.
type Shutdowner interface {
	// Shutdown gracefully stops the component. It should respect the context's
	// deadline and return ctx.Err() if it cannot complete in time.
	Shutdown(ctx context.Context) error
}

// component tracks a registered component for shutdown.
type component struct {
	name       string
	shutdowner Shutdowner
}

// Coordinator manages ordered shutdown of multiple components.
// Components are shut down in reverse order of registration.
type Coordinator struct {
	components []component
	logger     *slog.Logger
}

// NewCoordinator creates a new shutdown coordinator.
func NewCoordinator(logger *slog.Logger) *Coordinator {
	return &Coordinator{
		components: make([]component, 0),
		logger:     logger.With(slog.String("component", "shutdown")),
	}
}

// Register adds a component to be shut down. Components are shut down
// in reverse order of registration (LIFO - last in, first out).
//
// This ordering allows components registered later (which may depend on
// earlier components) to shut down first.
func (c *Coordinator) Register(name string, s Shutdowner) {
	c.components = append(c.components, component{
		name:       name,
		shutdowner: s,
	})
	c.logger.Debug("registered shutdown handler",
		slog.String("handler", name),
	)
}

// Shutdown stops all registered components in reverse order.
// It logs timing for each component and collects any errors.
//
// The shutdown context's deadline applies to the entire shutdown process.
// Individual component shutdowns may complete faster.
//
// Returns the first error encountered, or nil if all components
// shut down successfully.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.logger.Info("starting coordinated shutdown",
		slog.Int("components", len(c.components)),
	)

	var firstErr error

	// Shut down in reverse order (LIFO)
	for i := len(c.components) - 1; i >= 0; i-- {
		comp := c.components[i]

		// Check if we've already exceeded the shutdown deadline
		select {
		case <-ctx.Done():
			c.logger.Error("shutdown deadline exceeded",
				slog.String("remaining_component", comp.name),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("shutdown deadline exceeded at component %s: %w", comp.name, ctx.Err())
			}
			return firstErr
		default:
		}

		c.logger.Info("shutting down component",
			slog.String("handler", comp.name),
		)

		start := time.Now()
		err := comp.shutdowner.Shutdown(ctx)
		duration := time.Since(start)

		if err != nil {
			c.logger.Error("component shutdown failed",
				slog.String("handler", comp.name),
				slog.Duration("duration", duration),
				slog.String("error", err.Error()),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to shutdown %s: %w", comp.name, err)
			}
			// Continue shutting down other components even if one fails
		} else {
			c.logger.Info("component shutdown complete",
				slog.String("handler", comp.name),
				slog.Duration("duration", duration),
			)
		}
	}

	if firstErr != nil {
		c.logger.Warn("coordinated shutdown completed with errors")
	} else {
		c.logger.Info("coordinated shutdown complete")
	}

	return firstErr
}

// ComponentCount returns the number of registered components.
func (c *Coordinator) ComponentCount() int {
	return len(c.components)
}
