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
	// associates, when linked via WithAssociates, lets the site-scoped
	// count resolve each associate's site and shift state (the Postgres
	// adapter does the same with a JOIN on associate_shift). Unlinked, no
	// associate has a known site, so the scoped count is always 0.
	associates *AssociateRepo
}

// NewAssignmentRepo constructs an empty AssignmentRepo.
func NewAssignmentRepo() *AssignmentRepo {
	return &AssignmentRepo{byID: make(map[shared.AssociateId]*assignment.LaborAssignment)}
}

// WithAssociates links the associate roster used to answer
// CountActiveByPathAtSite and returns r for chaining.
func (r *AssignmentRepo) WithAssociates(a *AssociateRepo) *AssignmentRepo {
	r.associates = a
	return r
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

// CountActiveByPathAtSite counts associates with an active assignment to
// pathId whose not-ended shift is recorded at siteCode. Associates with no
// site never match (ADR 0034); an empty siteCode matches nothing.
func (r *AssignmentRepo) CountActiveByPathAtSite(ctx context.Context, pathId shared.PathId, siteCode shared.SiteCode) (int, error) {
	if r.associates == nil || siteCode.IsUnscoped() {
		return 0, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for id, la := range r.byID {
		if active, ok := la.ActivePathId(); !ok || active != pathId {
			continue
		}
		a, err := r.associates.FindByID(ctx, id)
		if err != nil {
			continue // no roster entry: no site, never counted at a site
		}
		if !a.Ended() && a.SiteCode() == siteCode {
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
