package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// Gap fix D13: a projector whose consumer loop has exited must not keep
// serving a healthy admin endpoint. superviseConsumer returns an error the
// moment the loop exits on its own, so run() returns it and main exits
// non-zero (Kubernetes restarts the pod instead of leaving a dead projector
// that every probe still reports as fine).

func TestSuperviseConsumer_ConsumerErrorStopsTheProjector(t *testing.T) {
	cause := errors.New("commit offsets: broker gone")
	stop := make(chan os.Signal, 1)

	err := superviseConsumer(func(context.Context) error { return cause }, stop)

	if !errors.Is(err, errConsumerStopped) {
		t.Fatalf("err = %v, want errConsumerStopped", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the consumer's own error", err)
	}
}

func TestSuperviseConsumer_ConsumerReturningNilWithoutSignalStillStops(t *testing.T) {
	stop := make(chan os.Signal, 1)

	err := superviseConsumer(func(context.Context) error { return nil }, stop)

	if !errors.Is(err, errConsumerStopped) {
		t.Fatalf("err = %v, want errConsumerStopped: a loop that ends without a shutdown signal is a dead projector", err)
	}
}

func TestSuperviseConsumer_SignalCancelsTheConsumerAndReturnsNil(t *testing.T) {
	stop := make(chan os.Signal, 1)
	cancelled := make(chan struct{})
	consume := func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return nil
	}
	stop <- syscall.SIGTERM

	if err := superviseConsumer(consume, stop); err != nil {
		t.Fatalf("err = %v, want nil on a requested shutdown", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the consumer's context was not cancelled on the shutdown signal")
	}
}
