// Package fulfillmentexecution provides outbound InstalledCapacityClient
// implementations: an HTTP client against fulfillment-execution's
// published GetInstalledCapacity contract, and a fail-LOUD permissive
// no-op selected by INSTALLED_CAPACITY_MODE (default "permissive") so
// unit tests and CI never reach the network.
//
// This follows the env-selected adapter pattern already used elsewhere in
// this fleet (order-management -> inventory-storage, this repo's own ->
// labor-performance), but with the FAIL-LOUD choice order-management's
// own inventory-storage client uses, not labor-performance's fail-open
// one: a measured rate is a soft, optional enrichment to a PROPOSAL, but
// an installed-capacity ceiling gates a COMMIT that mutates real state --
// this fleet's own rule is to fail loud for anything that mutates real
// state. See ADR-0014.
package fulfillmentexecution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// DefaultTimeout bounds a single call to fulfillment-execution, so a slow
// or hanging Supplier does not stall CommitShiftPlan indefinitely.
const DefaultTimeout = 3 * time.Second

// HTTPDoer is the subset of *http.Client this adapter depends on, so unit
// tests can substitute a fake transport without a real server.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a plain net/http implementation of ports.InstalledCapacityClient,
// calling fulfillment-execution's published contract:
//
//	GET /capacity/{capability} -> 200 with InstalledCapacityResponse
//
// It imports nothing from the fulfillment-execution module: the response
// shape below is a local mirror of that service's published wire contract
// (see its apis/openapi.yaml, InstalledCapacityResponse schema, and
// ADR-0018).
type Client struct {
	baseURL string
	doer    HTTPDoer
}

// Option customises a Client built by NewClient.
type Option func(*Client)

// NewClient builds a Client against baseURL (from
// FULFILLMENT_EXECUTION_BASE_URL). A nil doer defaults to an
// *http.Client with DefaultTimeout.
func NewClient(baseURL string, doer HTTPDoer, opts ...Option) *Client {
	if doer == nil {
		doer = &http.Client{Timeout: DefaultTimeout}
	}
	c := &Client{baseURL: strings.TrimRight(baseURL, "/"), doer: doer}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// installedCapacityResponse mirrors fulfillment-execution's
// InstalledCapacityResponse response schema.
type installedCapacityResponse struct {
	Capability string `json:"capability"`
	Installed  int    `json:"installed"`
}

// InstalledCapacity calls GET /capacity/{capability} on
// fulfillment-execution with capability sent verbatim. The caller passes
// a real station capability (e.g. "pick", "pack") that CommitShiftPlan
// resolved from the process-path catalogue's requiredCapabilities -- never
// a process-path id: fulfillment-execution counts stations by the exact
// capability strings they were registered with, so a path id such as the
// canonical "PICK" (or "pick-zone-a") would silently count 0 stations.
// Returns ports.ErrInstalledCapacityUnavailable (never a raw
// transport/decode error) on ANY failure -- unreachable service,
// non-200, or malformed body -- so CommitShiftPlan's single
// error-handling branch never needs to distinguish those cases.
func (c *Client) InstalledCapacity(ctx context.Context, capability shared.Capability) (int, error) {
	endpoint := fmt.Sprintf("%s/capacity/%s", c.baseURL, url.PathEscape(string(capability)))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ports.ErrInstalledCapacityUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ports.ErrInstalledCapacityUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: unexpected status %d", ports.ErrInstalledCapacityUnavailable, resp.StatusCode)
	}

	var decoded installedCapacityResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, fmt.Errorf("%w: %w", ports.ErrInstalledCapacityUnavailable, err)
	}
	return decoded.Installed, nil
}
