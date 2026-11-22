// Package events provides EventPublisher implementations. LogPublisher is
// the default; its Publisher interface is kafka-ready — a Kafka-backed
// implementation can satisfy the same ports.EventPublisher contract without
// touching application or domain code.
package events

import (
	"context"
	"log/slog"
	"sync"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// LogPublisher publishes events by logging them and buffering them in
// memory, so tests can assert on what was published.
type LogPublisher struct {
	Logger *slog.Logger

	mu     sync.Mutex
	events []shared.DomainEvent
}

// NewLogPublisher constructs a LogPublisher writing to logger. A nil logger
// disables logging while still buffering events.
func NewLogPublisher(logger *slog.Logger) *LogPublisher {
	return &LogPublisher{Logger: logger}
}

// Publish logs and buffers events.
func (p *LogPublisher) Publish(ctx context.Context, evts ...shared.DomainEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range evts {
		if p.Logger != nil {
			p.Logger.Info("domain event published", "event_name", sanitizeLog(e.EventName()), "occurred_at", e.OccurredAt())
		}
		p.events = append(p.events, e)
	}
	return nil
}

// Events returns all events published so far.
func (p *LogPublisher) Events() []shared.DomainEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]shared.DomainEvent(nil), p.events...)
}
