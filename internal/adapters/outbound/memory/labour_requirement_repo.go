package memory

import (
	"context"
	"sync"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// LabourRequirementRepo is a thread-safe in-memory ports.LabourRequirements
// (the local copy of warehouse-planning's LabourPlanCommitted, ADR 0043). Save
// applies the same version guard as the Postgres store.
type LabourRequirementRepo struct {
	mu   sync.RWMutex
	rows map[labourplan.Key]labourplan.Requirement
}

// NewLabourRequirementRepo constructs an empty LabourRequirementRepo.
func NewLabourRequirementRepo() *LabourRequirementRepo {
	return &LabourRequirementRepo{rows: make(map[labourplan.Key]labourplan.Requirement)}
}

// Find returns the stored requirement for key, or found=false.
func (r *LabourRequirementRepo) Find(_ context.Context, key labourplan.Key) (labourplan.Requirement, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	req, ok := r.rows[key]
	return req, ok, nil
}

// Save stores req unless a row with an equal or greater version is stored.
func (r *LabourRequirementRepo) Save(_ context.Context, req labourplan.Requirement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.rows[req.Key]
	if req.Supersedes(stored, ok) {
		r.rows[req.Key] = req
	}
	return nil
}

// ProcessedEvents is a thread-safe in-memory ports.ProcessedEvents.
type ProcessedEvents struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// NewProcessedEvents constructs an empty ProcessedEvents.
func NewProcessedEvents() *ProcessedEvents {
	return &ProcessedEvents{seen: make(map[string]struct{})}
}

// MarkProcessed records eventId if absent and reports whether it was new.
func (p *ProcessedEvents) MarkProcessed(_ context.Context, eventId string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.seen[eventId]; dup {
		return false, nil
	}
	p.seen[eventId] = struct{}{}
	return true, nil
}

// Compile-time assertions that both satisfy their ports.
var (
	_ ports.LabourRequirements = (*LabourRequirementRepo)(nil)
	_ ports.ProcessedEvents    = (*ProcessedEvents)(nil)
)
