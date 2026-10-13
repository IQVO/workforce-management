package kafka

import (
	"testing"

	segmentio "github.com/segmentio/kafka-go"
)

// TestNewAnalyticsDLQWriter_WaitsForAllInSyncReplicas pins acks=all on the
// analytics DLQ writer (ADR-0031): kafka-go defaults RequiredAcks to
// RequireNone (acks=0), so a broker blip would silently drop the poison
// message while the consumer commits its offset as if it had been
// dead-lettered.
func TestNewAnalyticsDLQWriter_WaitsForAllInSyncReplicas(t *testing.T) {
	w := newAnalyticsDLQWriter([]string{"localhost:9092"}, "warehouse.workforce.analytics"+analyticsDLQTopicSuffix)
	t.Cleanup(func() { _ = w.Close() })
	if w.RequiredAcks != segmentio.RequireAll {
		t.Fatalf("DLQ RequiredAcks = %v, want RequireAll (kafka-go's default is RequireNone, acks=0)", w.RequiredAcks)
	}
	if w.Async {
		t.Fatal("DLQ writer must be synchronous so a failed dead-letter write reaches the consumer")
	}
	if !w.AllowAutoTopicCreation {
		t.Fatal("DLQ writer must auto-create the .dlq topic (it usually does not exist before the first poison message)")
	}
}
