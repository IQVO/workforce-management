//go:build integration

package kafka

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// TestPublish_RealKafka_SameShiftPlanLandsOnSamePartition is the real-broker
// regression test for the partition-scaleup ordering gap (warehouse-infra PR
// #42, 1->8 partitions on warehouse.workforce.events). It boots an isolated
// Kafka broker (testcontainers, mirroring internal/adapters/outbound/
// kafkacatalog's consumer_integration_test.go pattern), creates the topic
// with 8 partitions (matching the fleet's post-scaleup partition count), and
// asserts every message produced for the SAME ShiftPlan aggregate — across
// two separate commits for that same building/shift — is delivered to the
// SAME partition, proving the key-based partitioner (not LeastBytes'
// round robin) now governs routing for this publisher.
func TestPublish_RealKafka_SameShiftPlanLandsOnSamePartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1", tckafka.WithClusterID("workforce-publisher-itest"))
	if err != nil {
		t.Fatalf("start Kafka container: %v", err)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate Kafka container: %v", err)
		}
	}()

	brokers, err := container.Brokers(ctx)
	if err != nil {
		t.Fatalf("Kafka brokers: %v", err)
	}
	topic := fmt.Sprintf("workforce-publisher-itest-%d", time.Now().UnixNano())
	createTopicWithPartitions(t, ctx, brokers, topic, 8)

	// A real segmentio.Writer with the production Hash balancer, pointed at
	// the 8-partition topic, so the partitioner (not just Encode's Key
	// field) is exercised end to end.
	writer := &kafkago.Writer{
		Addr:     kafkago.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafkago.Hash{},
	}
	defer func() { _ = writer.Close() }()

	repo := memory.NewShiftPlanRepo()
	pub := NewPublisherWithWriter(writer, repo)

	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 40, PlannedHours: 16},
		{PathId: "stow", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8},
	}
	installed := map[shared.PathId]int{"pack": 5, "pick": 5, "stow": 5}
	sp, err := shiftplan.CommitShiftPlan("BLD1", "SHIFT1", lines, installed, installed, 8.0, time.Now().UTC())
	if err != nil {
		t.Fatalf("commit shift plan: %v", err)
	}
	if err := repo.Save(ctx, sp); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := pub.Publish(ctx, sp.PullEvents()...); err != nil {
		t.Fatalf("publish first commit: %v", err)
	}

	// A second, later commit for the SAME building/shift.
	recommitLines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 4, PlannedRate: 50, PlannedHours: 32}}
	recommitInstalled := map[shared.PathId]int{"pack": 10}
	sp2, err := shiftplan.CommitShiftPlan("BLD1", "SHIFT1", recommitLines, recommitInstalled, recommitInstalled, 8.0, time.Now().UTC())
	if err != nil {
		t.Fatalf("recommit shift plan: %v", err)
	}
	if err := repo.Save(ctx, sp2); err != nil {
		t.Fatalf("save recommitted plan: %v", err)
	}
	if err := pub.Publish(ctx, sp2.PullEvents()...); err != nil {
		t.Fatalf("publish recommit: %v", err)
	}

	wantTotal := len(lines) + len(recommitLines)
	partitions := readAllPartitions(t, ctx, brokers, topic, wantTotal)
	if len(partitions) != wantTotal {
		t.Fatalf("expected %d messages read back, got %d", wantTotal, len(partitions))
	}
	first := partitions[0]
	for i, p := range partitions {
		if p != first {
			t.Fatalf("message %d landed on partition %d, want every message for BLD1/SHIFT1 on partition %d (got %v)", i, p, first, partitions)
		}
	}
}

// createTopicWithPartitions creates topic with numPartitions and waits for
// every partition to have an elected leader before returning.
func createTopicWithPartitions(t *testing.T, ctx context.Context, brokers []string, topic string, numPartitions int) {
	t.Helper()
	conn := dialAndCreateTopic(t, ctx, brokers[0], kafkago.TopicConfig{Topic: topic, NumPartitions: numPartitions, ReplicationFactor: 1})
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		partitions, err := conn.ReadPartitions(topic)
		if err == nil && len(partitions) == numPartitions {
			allLeaders := true
			for _, p := range partitions {
				if p.Leader.ID < 0 {
					allLeaders = false
					break
				}
			}
			if allLeaders {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("topic %q did not obtain leaders for all %d partitions", topic, numPartitions)
}

// readAllPartitions reads want messages from topic (from the earliest
// offset, across all partitions) and returns the partition each landed on,
// in the order read from the log (which — for a single-producer sequential
// Publish — is publish order per partition, and the two calls here each
// write within one partition since they share a key).
func readAllPartitions(t *testing.T, ctx context.Context, brokers []string, topic string, want int) []int {
	t.Helper()
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     "workforce-publisher-itest-reader",
		StartOffset: kafkago.FirstOffset,
		MinBytes:    1,
		MaxBytes:    10e6,
	})
	defer func() { _ = reader.Close() }()

	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var got []int
	for len(got) < want {
		m, err := reader.ReadMessage(readCtx)
		if err != nil {
			t.Fatalf("read message %d/%d: %v", len(got)+1, want, err)
		}
		// Each message on the real wire is a CloudEvents 1.0 structured-mode
		// event carrying the content-type header (ADR-0026).
		e, err := cloudevents.Decode(m.Value)
		if err != nil {
			t.Fatalf("message %d is not a CloudEvent: %v", len(got)+1, err)
		}
		if e.Type() != cloudevents.TypeShiftPlanCommitted || e.Subject() != string(m.Key) {
			t.Fatalf("message %d: type=%q subject=%q key=%q", len(got)+1, e.Type(), e.Subject(), m.Key)
		}
		var ct string
		for _, h := range m.Headers {
			if h.Key == "content-type" {
				ct = string(h.Value)
			}
		}
		if ct != cloudevents.MediaType {
			t.Fatalf("message %d: content-type header = %q, want %q", len(got)+1, ct, cloudevents.MediaType)
		}
		got = append(got, m.Partition)
	}
	return got
}

// dialAndCreateTopic dials the Kafka controller and creates the topic, retrying transient broker errors.
// Right after the testcontainers Kafka reports ready, the first connection can be reset ("connection reset
// by peer"); a create that succeeded just before such a reset reports TopicAlreadyExists on the retry, which
// is success. Fails the test only when the broker is still unusable after the deadline.
func dialAndCreateTopic(t *testing.T, ctx context.Context, broker string, cfg kafkago.TopicConfig) *kafkago.Conn {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for attempt := 0; ; attempt++ {
		conn, err := kafkago.DialContext(ctx, "tcp", broker)
		if err == nil {
			err = conn.CreateTopics(cfg)
			if err == nil || errors.Is(err, kafkago.TopicAlreadyExists) {
				return conn
			}
			_ = conn.Close()
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("dial/create topic %q on %s after %d attempt(s): %v", cfg.Topic, broker, attempt+1, lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
