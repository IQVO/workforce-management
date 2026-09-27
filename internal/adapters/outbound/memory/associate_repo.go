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

// Save stores a snapshot of a. Mirrors the Postgres adapter's version
// bookkeeping (ADR 0021) so use-case tests against the in-memory fake
// see the same version-advances-by-one-on-update behavior as production:
// a fresh associate (no existing row) is stored at a.Version() as-is; an
// existing row's version is incremented by one on every Save,
// regardless of what a.Version() carried in (this in-memory fake has no
// concurrent writers to guard against, so it never rejects a Save --
// unlike the Postgres adapter's version-guarded upsert).
func (r *AssociateRepo) Save(ctx context.Context, a *associate.AssociateShift) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	version := a.Version()
	if existing, ok := r.byID[a.AssociateId()]; ok {
		version = existing.Version() + 1
	}
	r.byID[a.AssociateId()] = associate.Rehydrate(a.AssociateId(), a.Certifications(), a.IsOnBreak(), a.HoursLogged(), a.Ended(), version)
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
