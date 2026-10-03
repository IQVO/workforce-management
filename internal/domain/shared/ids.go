// Package shared holds value objects and domain events used across the
// Workforce Management aggregates: AssociateId, PathId, Certification, and
// the domain event envelope.
package shared

import "errors"

// AssociateId identifies a single associate.
type AssociateId string

// PathId identifies a process path (e.g. "pack", "pick", "stow").
type PathId string

// Certification is a named qualification an associate can hold (e.g. "pack",
// "hazmat", "pick"). An associate untrained on a path cannot be assigned to
// it.
type Certification string

// ErrEmptyAssociateId is returned when an AssociateId is constructed empty.
var ErrEmptyAssociateId = errors.New("associate id must not be empty")

// ErrEmptyPathId is returned when a PathId is constructed empty.
var ErrEmptyPathId = errors.New("path id must not be empty")

// ErrEmptyCertification is returned when a Certification is constructed empty.
var ErrEmptyCertification = errors.New("certification must not be empty")

// NewAssociateId validates and constructs an AssociateId.
func NewAssociateId(v string) (AssociateId, error) {
	if v == "" {
		return "", ErrEmptyAssociateId
	}
	return AssociateId(v), nil
}

// NewPathId validates and constructs a PathId.
func NewPathId(v string) (PathId, error) {
	if v == "" {
		return "", ErrEmptyPathId
	}
	return PathId(v), nil
}

// NewCertification validates and constructs a Certification.
func NewCertification(v string) (Certification, error) {
	if v == "" {
		return "", ErrEmptyCertification
	}
	return Certification(v), nil
}

// Capability is a physical station capability as fulfillment-execution's
// Station registry records it (e.g. "pick", "pack", "rebin", "slam"). It is
// deliberately a DIFFERENT type from PathId: a process path (e.g. the
// canonical "PICK", or a real "pick-zone-a") declares the capabilities a
// station must hold to serve it via the process-path catalogue's
// requiredCapabilities, and the two vocabularies do not share a case or
// shape convention. Never convert a PathId into a Capability by casting —
// resolve it through the catalogue (see ADR-0014's addendum).
type Capability string
