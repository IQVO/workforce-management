//go:build integration

package laborperformancecache

import (
	"context"
	"fmt"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// TestNewConsumerForTopic_ReplaysTaskPerformanceRecordedAndComputesMean
// verifies the real replay/readiness path against an isolated Kafka
// broker: publish fake TaskPerformanceRecorded CloudEvents onto a
// throwaway topic, the Consumer replays it, MeanActualSeconds returns
// the right value.
func TestNewConsumerForTopic_ReplaysTaskPerformanceRecordedAndComputesMean(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1", tckafka.WithClusterID("workforce-laborperformancecache-itest"))
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
	topic := fmt.Sprintf("workforce-laborperformancecache-itest-%d", time.Now().UnixNano())
	createTopicAndWaitForLeader(t, ctx, brokers, topic)

	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic}
	defer func() { _ = writer.Close() }()

	// Two TaskPerformanceRecorded events for PICK (mean of 40 and 60 is
	// 50), plus one with a null efficiency_pct that must still count
	// toward the mean, exactly per the wire contract documented in
	// ADR 0013. The first carries a real idle_seconds_before (20); the
	// second omits idle_seconds_before entirely (absent key decodes to
	// nil, same as an explicit JSON null) -- it must contribute to the
	// idle-share denominator (actual_seconds) only, never the numerator.
	messages := []kafkago.Message{
		{Value: []byte(`{"specversion":"1.0","id":"evt-1","source":"/warehouse/labor-performance","type":"com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded","subject":"task-1","datacontenttype":"application/json","dataschema":"urn:warehouse:labor-performance:events:TaskPerformanceRecorded:v1","time":"2026-09-05T09:30:00Z","data":{"task_id":"task-1","associate_id":"assoc-1","task_type":"PICK","efficiency_pct":91.2,"actual_seconds":40,"idle_seconds_before":20,"completed_at":"2026-09-05T09:30:00Z"}}`)},
		{Value: []byte(`{"specversion":"1.0","id":"evt-2","source":"/warehouse/labor-performance","type":"com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded","subject":"task-2","datacontenttype":"application/json","dataschema":"urn:warehouse:labor-performance:events:TaskPerformanceRecorded:v1","time":"2026-09-05T10:00:00Z","data":{"task_id":"task-2","associate_id":"","task_type":"PICK","efficiency_pct":null,"actual_seconds":60,"completed_at":"2026-09-05T10:00:00Z"}}`)},
		// An at-least-once redelivery of evt-2 (same CloudEvents id) must
		// not be folded into the mean twice.
		{Value: []byte(`{"specversion":"1.0","id":"evt-2","source":"/warehouse/labor-performance","type":"com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded","subject":"task-2","datacontenttype":"application/json","dataschema":"urn:warehouse:labor-performance:events:TaskPerformanceRecorded:v1","time":"2026-09-05T10:00:00Z","data":{"task_id":"task-2","associate_id":"","task_type":"PICK","efficiency_pct":null,"actual_seconds":60,"completed_at":"2026-09-05T10:00:00Z"}}`)},
		// A retired flat-envelope message must be skipped, never parsed.
		{Value: []byte(`{"event_id":"legacy","event_type":"TaskPerformanceRecorded","occurred_at":"2026-09-05T10:00:00Z","source":"labor-performance","data":{"task_id":"legacy","task_type":"PICK","actual_seconds":9000}}`)},
	}
	if err := writer.WriteMessages(ctx, messages...); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	consumer, err := NewConsumerForTopic(ctx, brokers, topic, nil)
	if err != nil {
		t.Fatalf("construct consumer: %v", err)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	if err := consumer.WaitReady(ctx); err != nil {
		runCancel()
		_ = consumer.Close()
		t.Fatalf("consumer did not become ready: %v", err)
	}

	got, err := consumer.MeanActualSeconds(ctx, shared.PathId("pick"))
	if err != nil {
		runCancel()
		_ = consumer.Close()
		t.Fatalf("MeanActualSeconds: %v", err)
	}
	if got != 50 {
		runCancel()
		_ = consumer.Close()
		t.Fatalf("mean = %v, want 50 (null efficiency_pct must still count toward the mean)", got)
	}

	// idle=20 (only evt-1 observed), actual=40+60=100 -> share = 20/120.
	idleShare, err := consumer.IdleSharePct(ctx, shared.PathId("pick"))
	if err != nil {
		runCancel()
		_ = consumer.Close()
		t.Fatalf("IdleSharePct: %v", err)
	}
	wantIdleShare := 20.0 / 120.0
	if idleShare != wantIdleShare {
		runCancel()
		_ = consumer.Close()
		t.Fatalf("idle share = %v, want %v (evt-2's absent idle_seconds_before must contribute actual_seconds only)", idleShare, wantIdleShare)
	}

	runCancel()
	if err := consumer.Close(); err != nil {
		t.Fatalf("close consumer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run consumer: %v", err)
	}
}

// TestNewConsumerForTopic_TwoInstancesInARow_BothReplayFully is the
// regression test for the shared-consumer-group data-loss bug: a second
// consumer instance against the same topic must independently replay the
// full history under its own consumer group, not resume from the first
// instance's committed offset with an empty cache.
func TestNewConsumerForTopic_TwoInstancesInARow_BothReplayFully(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1", tckafka.WithClusterID("workforce-laborperformancecache-itest2"))
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
	topic := fmt.Sprintf("workforce-laborperformancecache-itest2-%d", time.Now().UnixNano())
	createTopicAndWaitForLeader(t, ctx, brokers, topic)

	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic}
	defer func() { _ = writer.Close() }()
	if err := writer.WriteMessages(ctx, kafkago.Message{
		Value: []byte(`{"specversion":"1.0","id":"evt-1","source":"/warehouse/labor-performance","type":"com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded","subject":"task-1","datacontenttype":"application/json","dataschema":"urn:warehouse:labor-performance:events:TaskPerformanceRecorded:v1","time":"2026-09-05T09:30:00Z","data":{"task_id":"task-1","associate_id":"assoc-1","task_type":"PACK","efficiency_pct":80.0,"actual_seconds":25,"completed_at":"2026-09-05T09:30:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	for instance := 1; instance <= 2; instance++ {
		consumer, err := NewConsumerForTopic(ctx, brokers, topic, nil)
		if err != nil {
			t.Fatalf("construct consumer %d: %v", instance, err)
		}
		runCtx, runCancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- consumer.Run(runCtx) }()

		if err := consumer.WaitReady(ctx); err != nil {
			runCancel()
			_ = consumer.Close()
			t.Fatalf("consumer %d did not become ready: %v", instance, err)
		}
		got, err := consumer.MeanActualSeconds(ctx, shared.PathId("pack"))
		if err != nil {
			runCancel()
			_ = consumer.Close()
			t.Fatalf("consumer %d did not replay the event: %v", instance, err)
		}
		if got != 25 {
			runCancel()
			_ = consumer.Close()
			t.Fatalf("consumer %d mean = %v, want 25", instance, got)
		}

		runCancel()
		if err := consumer.Close(); err != nil {
			t.Fatalf("close consumer %d: %v", instance, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("run consumer %d: %v", instance, err)
		}
	}
}

func createTopicAndWaitForLeader(t *testing.T, ctx context.Context, brokers []string, topic string) {
	t.Helper()
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial Kafka controller: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("create topic %q: %v", topic, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		partitions, err := conn.ReadPartitions(topic)
		if err == nil && len(partitions) == 1 && partitions[0].Leader.ID >= 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("topic %q did not obtain a partition leader", topic)
}
