package labourplan

import (
	"errors"
	"testing"
)

func validRequirement() Requirement {
	return Requirement{
		Key:              Key{SiteCode: "WH1", ShiftId: "2026-11-18-day", PathId: "PICK"},
		PlannedHeads:     4,
		RateUnitsPerHour: 120,
		Hours:            8,
		HeadsSource:      HeadsSourceForecast,
		Version:          1,
	}
}

func TestValidateAcceptsAContractRow(t *testing.T) {
	if err := validRequirement().Validate(); err != nil {
		t.Fatalf("valid row refused: %v", err)
	}
	zero := validRequirement()
	zero.PlannedHeads = 0
	if err := zero.Validate(); err != nil {
		t.Fatalf("zero heads is a legitimate requirement: %v", err)
	}
}

func TestValidateRefusesEachBrokenRule(t *testing.T) {
	cases := map[string]func(*Requirement){
		"missing site":   func(r *Requirement) { r.Key.SiteCode = "" },
		"blank site":     func(r *Requirement) { r.Key.SiteCode = "  " },
		"missing shift":  func(r *Requirement) { r.Key.ShiftId = "" },
		"missing path":   func(r *Requirement) { r.Key.PathId = "" },
		"negative heads": func(r *Requirement) { r.PlannedHeads = -1 },
		"version zero":   func(r *Requirement) { r.Version = 0 },
		"version below":  func(r *Requirement) { r.Version = -3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequirement()
			mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrInvalidRequirement) {
				t.Fatalf("Validate = %v, want ErrInvalidRequirement", err)
			}
		})
	}
}

func TestSupersedesAppliesOnlyAGreaterVersion(t *testing.T) {
	stored := validRequirement()
	stored.Version = 3
	cases := []struct {
		name    string
		version int
		found   bool
		want    bool
	}{
		{"no stored row", 1, false, true},
		{"greater version", 4, true, true},
		{"equal version is a no-op", 3, true, false},
		{"lower version is a no-op", 2, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			incoming := validRequirement()
			incoming.Version = tc.version
			if got := incoming.Supersedes(stored, tc.found); got != tc.want {
				t.Fatalf("Supersedes(v%d over v%d, found=%v) = %v, want %v", tc.version, stored.Version, tc.found, got, tc.want)
			}
		})
	}
}
