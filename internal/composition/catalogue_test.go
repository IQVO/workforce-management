package composition_test

import (
	"context"
	"strings"
	"testing"

	"github.com/claudioed/workforce-management/internal/composition"
)

// The catalogue always comes from Kafka (ADR-0038): no brokers is a boot
// error naming KAFKA_BROKERS, never a fall back to another source.
func TestBuildCatalogue_RequiresBrokers(t *testing.T) {
	for _, brokers := range [][]string{nil, {}, {""}} {
		cat, consumer, err := composition.BuildCatalogue(context.Background(), context.Background(),
			composition.CatalogueConfig{Brokers: brokers}, quietLogger())
		if err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS is required") {
			t.Fatalf("brokers %v: err = %v, want a boot error naming KAFKA_BROKERS", brokers, err)
		}
		if cat != nil || consumer != nil {
			t.Fatalf("brokers %v: catalogue=%v consumer=%v, want nil on error", brokers, cat, consumer)
		}
	}
}

// Fail closed (ADR-0013): a broker nobody listens on stops the boot after the
// bounded retries; it never degrades to an empty catalogue.
func TestBuildCatalogue_UnreachableBrokerFailsBoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled context makes bootretry give up at once
	cat, consumer, err := composition.BuildCatalogue(ctx, context.Background(),
		composition.CatalogueConfig{Brokers: []string{"127.0.0.1:1"}}, quietLogger())
	if err == nil {
		t.Fatal("an unreachable broker must fail boot")
	}
	if cat != nil || consumer != nil {
		t.Fatalf("catalogue=%v consumer=%v, want nil on error", cat, consumer)
	}
}
