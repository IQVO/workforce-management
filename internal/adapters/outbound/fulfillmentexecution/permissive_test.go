package fulfillmentexecution_test

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func TestPermissiveClient_AlwaysReturnsErrInstalledCapacityUnavailable(t *testing.T) {
	client := fulfillmentexecution.NewPermissiveClient()
	_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("want ErrInstalledCapacityUnavailable, got %v", err)
	}
}
