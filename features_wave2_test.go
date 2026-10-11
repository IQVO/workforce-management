package bdd

// Wave-2 step definitions: generic REST steps (any method, raw bodies, JSON
// path assertions, response headers), the events the service publishes, an
// unreachable fulfillment-execution, and the readiness gate. They sit on the
// shared world, so every assertion still goes through the REST boundary or
// the recorded domain events -- never into repositories or aggregates.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/cucumber/godog"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// harness exposes the collaborators behind the server that scenarios need to
// observe: the events published and the readiness gate.
type harness struct {
	published *recordingPublisher
	readiness *inboundhttp.Readiness
}

// recordingPublisher forwards to the production-shaped log publisher and
// remembers every event it was asked to publish.
type recordingPublisher struct {
	inner  ports.EventPublisher
	mu     sync.Mutex
	events []shared.DomainEvent
}

func (p *recordingPublisher) Publish(ctx context.Context, evs ...shared.DomainEvent) error {
	p.mu.Lock()
	p.events = append(p.events, evs...)
	p.mu.Unlock()
	return p.inner.Publish(ctx, evs...)
}

func (p *recordingPublisher) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e.EventName() == name {
			n++
		}
	}
	return n
}

// unreachableCapacity is fulfillment-execution when it cannot be reached.
type unreachableCapacity struct{}

func (unreachableCapacity) InstalledCapacity(context.Context, shared.Capability) (int, error) {
	return 0, fmt.Errorf("%w: connection refused", ports.ErrInstalledCapacityUnavailable)
}

// --- requests -----------------------------------------------------------------

func (w *world) sendRequest(ctx context.Context, method, path string) error {
	return w.do(ctx, method, path, nil)
}

func (w *world) sendRequestWithBody(ctx context.Context, method, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, method, w.server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	w.lastStatus = resp.StatusCode
	w.lastContentType = resp.Header.Get("Content-Type")
	w.lastHeader = resp.Header
	w.lastBody = raw
	return nil
}

func (w *world) fulfillmentExecutionIsUnreachable() error {
	w.capacityDown = true
	w.rebuild()
	return nil
}

func (w *world) gracefulShutdownBegins() error {
	w.h.readiness.SetNotReady()
	return nil
}

// --- assertions ---------------------------------------------------------------

// lookup walks a dotted path ("lines.0.pathId", "0.understaffed") through the
// last JSON response, which may be an object or an array at the root.
func (w *world) lookup(path string) (any, bool, error) {
	var root any
	if err := json.Unmarshal(w.lastBody, &root); err != nil {
		return nil, false, fmt.Errorf("decode response %q: %w", string(w.lastBody), err)
	}
	cur := root
	for _, part := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false, nil
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false, nil
			}
			cur = node[i]
		default:
			return nil, false, nil
		}
	}
	return cur, true, nil
}

func (w *world) fieldIsString(path, want string) error {
	got, ok, err := w.lookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("response has no field %q: %s", path, string(w.lastBody))
	}
	if s, isString := got.(string); !isString || s != want {
		return fmt.Errorf("field %q is %v, want %q", path, got, want)
	}
	return nil
}

func (w *world) fieldIsBool(path, want string) error {
	got, ok, err := w.lookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("response has no field %q: %s", path, string(w.lastBody))
	}
	if b, isBool := got.(bool); !isBool || strconv.FormatBool(b) != want {
		return fmt.Errorf("field %q is %v, want %s", path, got, want)
	}
	return nil
}

func (w *world) fieldIsNumber(path, want string) error {
	got, ok, err := w.lookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("response has no field %q: %s", path, string(w.lastBody))
	}
	wantN, err := strconv.ParseFloat(want, 64)
	if err != nil {
		return fmt.Errorf("number %q: %w", want, err)
	}
	if n, isNumber := got.(float64); !isNumber || n != wantN {
		return fmt.Errorf("field %q is %v, want %v", path, got, wantN)
	}
	return nil
}

func (w *world) fieldIsAbsent(path string) error {
	_, ok, err := w.lookup(path)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("response must omit %q: %s", path, string(w.lastBody))
	}
	return nil
}

func (w *world) responseIsListOf(want int) error {
	var list []any
	if err := json.Unmarshal(w.lastBody, &list); err != nil {
		return fmt.Errorf("response is not a JSON array: %q", string(w.lastBody))
	}
	if len(list) != want {
		return fmt.Errorf("response lists %d entries, want %d: %s", len(list), want, string(w.lastBody))
	}
	return nil
}

func (w *world) headerIs(name, want string) error {
	if got := w.lastHeader.Get(name); got != want {
		return fmt.Errorf("header %s is %q, want %q", name, got, want)
	}
	return nil
}

func (w *world) headerIsAbsent(name string) error {
	if got := w.lastHeader.Get(name); got != "" {
		return fmt.Errorf("header %s is %q, want it absent", name, got)
	}
	return nil
}

func (w *world) eventsPublished(count int, name string) error {
	if got := w.h.published.count(name); got != count {
		return fmt.Errorf("%d %s events published, want %d", got, name, count)
	}
	return nil
}

func registerWave2(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a (GET|POST) request is sent to "([^"]*)"$`, w.sendRequest)
	sc.Step(`^a (POST|PUT) request is sent to "([^"]*)" with the body (.+)$`, w.sendRequestWithBody)
	sc.Step(`^the request is rejected with status (\d+)$`, w.expectStatus)
	sc.Step(`^the response status is (\d+)$`, w.expectStatus)
	sc.Step(`^fulfillment-execution cannot be reached$`, w.fulfillmentExecutionIsUnreachable)
	sc.Step(`^the service begins graceful shutdown$`, w.gracefulShutdownBegins)

	sc.Step(`^the response field "([^"]*)" is "([^"]*)"$`, w.fieldIsString)
	sc.Step(`^the response field "([^"]*)" is (true|false)$`, w.fieldIsBool)
	sc.Step(`^the response field "([^"]*)" is the number (-?[0-9.]+)$`, w.fieldIsNumber)
	sc.Step(`^the response omits the field "([^"]*)"$`, w.fieldIsAbsent)
	sc.Step(`^the response is a list of (\d+) entries$`, w.responseIsListOf)
	sc.Step(`^the response header "([^"]*)" is "([^"]*)"$`, w.headerIs)
	sc.Step(`^the response has no header "([^"]*)"$`, w.headerIsAbsent)

	sc.Step(`^(\d+) "([^"]*)" events? (?:was|were) published$`, w.eventsPublished)
}
