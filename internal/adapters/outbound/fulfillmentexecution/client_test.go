package fulfillmentexecution_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

type captured struct {
	method string
	path   string
}

func newServer(t *testing.T, status int, responseBody string, got *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if responseBody != "" {
			_, _ = w.Write([]byte(responseBody))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstalledCapacityCallsPublishedContract(t *testing.T) {
	var got captured
	srv := newServer(t, http.StatusOK, `{"capability":"pack","installed":4}`, &got)

	client := fulfillmentexecution.NewClient(srv.URL+"/", nil)
	installed, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if err != nil {
		t.Fatalf("InstalledCapacity: %v", err)
	}
	if installed != 4 {
		t.Fatalf("installed = %d, want 4", installed)
	}
	if got.method != http.MethodGet || got.path != "/capacity/pack" {
		t.Fatalf("called %s %s, want GET /capacity/pack", got.method, got.path)
	}
}

// TestInstalledCapacitySendsCapabilityVerbatim proves the client puts
// the capability it is given on the wire byte-for-byte -- no case
// folding, no path-id translation. Resolving a process path to its
// capability is CommitShiftPlan's job (through the process-path
// catalogue), not this adapter's: fulfillment-execution matches the
// registered capability string exactly, so live "GET /capacity/PICK"
// returns 0 while "GET /capacity/pick" returns the real count.
func TestInstalledCapacitySendsCapabilityVerbatim(t *testing.T) {
	tests := []struct {
		capability shared.Capability
		wantPath   string
	}{
		{capability: "pick", wantPath: "/capacity/pick"},
		{capability: "rebin", wantPath: "/capacity/rebin"},
		{capability: "PICK", wantPath: "/capacity/PICK"},
	}
	for _, tt := range tests {
		t.Run(string(tt.capability), func(t *testing.T) {
			var got captured
			srv := newServer(t, http.StatusOK, `{"installed":0}`, &got)

			client := fulfillmentexecution.NewClient(srv.URL, nil)
			if _, err := client.InstalledCapacity(context.Background(), tt.capability); err != nil {
				t.Fatalf("InstalledCapacity: %v", err)
			}
			if got.path != tt.wantPath {
				t.Fatalf("path = %q, want %q", got.path, tt.wantPath)
			}
		})
	}
}

func TestInstalledCapacityZeroIsARealAnswerNotAnError(t *testing.T) {
	// Mirrors fulfillment-execution's own contract: an unrecognized
	// capability returns installed: 0 with a 200, not a 404 -- and
	// this client must surface that as a real 0, not
	// ErrInstalledCapacityUnavailable.
	srv := newServer(t, http.StatusOK, `{"capability":"unknown","installed":0}`, &captured{})

	client := fulfillmentexecution.NewClient(srv.URL, nil)
	installed, err := client.InstalledCapacity(context.Background(), shared.Capability("unknown"))
	if err != nil {
		t.Fatalf("InstalledCapacity: %v", err)
	}
	if installed != 0 {
		t.Fatalf("installed = %d, want 0", installed)
	}
}

func TestInstalledCapacityMapsEveryFailureModeToErrInstalledCapacityUnavailable(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		responseBody string
	}{
		{name: "500 internal error", status: http.StatusInternalServerError, responseBody: ""},
		{name: "malformed JSON body", status: http.StatusOK, responseBody: "{not json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, tt.status, tt.responseBody, &captured{})
			client := fulfillmentexecution.NewClient(srv.URL, nil)
			_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
			if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
				t.Fatalf("want ErrInstalledCapacityUnavailable, got %v", err)
			}
		})
	}
}

func TestInstalledCapacityUnreachableServer_ReturnsErrInstalledCapacityUnavailable(t *testing.T) {
	client := fulfillmentexecution.NewClient("http://127.0.0.1:1", nil)
	_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("want ErrInstalledCapacityUnavailable, got %v", err)
	}
}
