package shared

import (
	"errors"
	"testing"
)

func TestNewAssociateId(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    AssociateId
		wantErr error
	}{
		{"valid", "assoc-1", AssociateId("assoc-1"), nil},
		{"empty", "", AssociateId(""), ErrEmptyAssociateId},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewAssociateId(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestNewPathId(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    PathId
		wantErr error
	}{
		{"valid", "pack", PathId("pack"), nil},
		{"empty", "", PathId(""), ErrEmptyPathId},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewPathId(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestNewCertification(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Certification
		wantErr error
	}{
		{"valid", "hazmat", Certification("hazmat"), nil},
		{"empty", "", Certification(""), ErrEmptyCertification},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewCertification(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
		})
	}
}
