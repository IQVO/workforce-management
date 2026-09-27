package memory

import (
	"context"
	"sync"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// AssignmentRepo is a thread-safe in-memory ports.AssignmentRepo.
type AssignmentRepo struct {
	mu   sync.RWMutex
	byID map[shared.AssociateId]*assignment.LaborAssignment
}

// NewAssignmentRepo constructs an empty AssignmentRepo.
func NewAssignmentRepo() *AssignmentRepo {
	return &AssignmentRepo{byID: make(map[shared.AssociateId]*assignment.LaborAssignment)}
}

// Save stores a snapshot of la.
func (r *AssignmentRepo) Save(ctx context.Context, la *assignment.LaborAssignment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[la.AssociateId()] = snapshot(la)
	return nil
}

// FindByAssociateID loads the LaborAssignment for id, or ports.ErrNotFound.
func (r *AssignmentRepo) FindByAssociateID(ctx context.Context, id shared.AssociateId) (*assignment.LaborAssignment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	la, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return snapshot(la), nil
}

// CountActiveByPath counts how many associates currently have an active
// assignment to pathId.
func (r *AssignmentRepo) CountActiveByPath(ctx context.Context, pathId shared.PathId) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, la := range r.byID {
		if active, ok := la.ActivePathId(); ok && active == pathId {
			count++
		}
	}
	return count, nil
}

func snapshot(la *assignment.LaborAssignment) *assignment.LaborAssignment {
	var active *assignment.Interval
	if iv, ok := la.ActiveInterval(); ok {
		active = &iv
	}
	return assignment.Rehydrate(la.AssociateId(), active, la.History(), la.Version())
}
