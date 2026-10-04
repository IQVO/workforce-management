package composition_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/claudioed/workforce-management/internal/composition"
)

func writeCatalogue(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "process-paths.yaml")
	const content = `
building: test-fc
paths:
  - id: PICK
    matchPrefix: pick
    direct: true
    requiredCapabilities: [pick]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write catalogue: %v", err)
	}
	return path
}

// The default source is the boot-time file: valid -> catalogue, no consumer.
func TestBuildCatalogue_FileSource(t *testing.T) {
	cat, consumer, err := composition.BuildCatalogue(context.Background(), context.Background(),
		composition.CatalogueConfig{Source: composition.CatalogueFromFile, File: writeCatalogue(t)}, quietLogger())
	if err != nil {
		t.Fatalf("BuildCatalogue: %v", err)
	}
	if consumer != nil {
		t.Error("the file source must not start a Kafka consumer")
	}
	if _, err := cat.Lookup("pick-zone-a"); err != nil {
		t.Errorf("Lookup(pick-zone-a): %v", err)
	}
	if _, err := cat.Lookup("hazmat"); err == nil {
		t.Error("Lookup(hazmat) must fail: not a declared path")
	}
}

// Fail closed (ADR-0013): a missing catalogue file stops boot; it never
// degrades to an empty catalogue.
func TestBuildCatalogue_MissingFileFailsBoot(t *testing.T) {
	cat, _, err := composition.BuildCatalogue(context.Background(), context.Background(),
		composition.CatalogueConfig{Source: "", File: filepath.Join(t.TempDir(), "absent.yaml")}, quietLogger())
	if err == nil {
		t.Fatal("a missing PATH_CATALOGUE_FILE must fail boot")
	}
	if cat != nil {
		t.Errorf("catalogue = %v, want nil on error", cat)
	}
}

func TestBuildCatalogue_KafkaSourceRequiresBrokers(t *testing.T) {
	for _, brokers := range [][]string{nil, {""}} {
		_, _, err := composition.BuildCatalogue(context.Background(), context.Background(),
			composition.CatalogueConfig{Source: composition.CatalogueFromKafka, Brokers: brokers}, quietLogger())
		if err == nil {
			t.Fatalf("PATH_CATALOGUE_SOURCE=kafka with brokers %v must fail boot", brokers)
		}
	}
}
