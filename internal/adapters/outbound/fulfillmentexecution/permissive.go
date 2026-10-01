package fulfillmentexecution

import (
	"context"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// PermissiveClient is the default InstalledCapacityClient: it never
// contacts fulfillment-execution and never fabricates a capacity number.
// Every call returns ports.ErrInstalledCapacityUnavailable, which
// CommitShiftPlan surfaces as a FAILED commit -- unlike
// labor-performance's PermissiveClient (which fails open because a
// measured rate is a soft, optional enrichment to a proposal), this
// fails loudly because an installed-capacity ceiling gates a commit that
// mutates real state, and this fleet's own rule is to fail loud for
// anything that mutates real state. A caller must explicitly opt into
// INSTALLED_CAPACITY_MODE=http to ever commit a shift plan in this
// service's default configuration.
type PermissiveClient struct{}

// NewPermissiveClient constructs a PermissiveClient.
func NewPermissiveClient() *PermissiveClient { return &PermissiveClient{} }

func (PermissiveClient) InstalledCapacity(_ context.Context, _ shared.Capability) (int, error) {
	return 0, ports.ErrInstalledCapacityUnavailable
}
