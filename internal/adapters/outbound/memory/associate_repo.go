// Package memory provides thread-safe in-memory implementations of every
// application port, for tests and local development.
package memory

import (
	"context"
	"sync"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// AssociateRepo is a thread-safe in-memory ports.AssociateRepo.
type AssociateRepo struct {
	mu   sync.RWMutex
	byID map[shared.AssociateId]*associate.AssociateShift
}

// NewAssociateRepo constructs an empty AssociateRepo.
func NewAssociateRepo() *AssociateRepo {
	return &AssociateRepo{byID: make(map[shared.AssociateId]*associate.AssociateShift)}
}

// Save stores a snapshot of a.
func (r *AssociateRepo) Save(ctx context.Context, a *associate.AssociateShift) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[a.AssociateId()] = associate.Rehydrate(a.AssociateId(), a.Certifications(), a.IsOnBreak(), a.HoursLogged(), a.Ended(), a.Version())
	return nil
}

// FindByID loads the AssociateShift for id, or ports.ErrNotFound.
func (r *AssociateRepo) FindByID(ctx context.Context, id shared.AssociateId) (*associate.AssociateShift, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return associate.Rehydrate(a.AssociateId(), a.Certifications(), a.IsOnBreak(), a.HoursLogged(), a.Ended(), a.Version()), nil
}
