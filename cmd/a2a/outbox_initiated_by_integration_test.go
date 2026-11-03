//go:build integration

package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	inbounda2a "github.com/claudioed/workforce-management/internal/adapters/inbound/a2a"
	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/composition"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// outboxEvent is one persisted outbox row's CloudEvent, decoded just enough to
// inspect its attributes and data.
type outboxEvent struct {
	eventType string
	topic     string
	envelope  map[string]json.RawMessage
	data      map[string]json.RawMessage
}

func outboxEvents(t *testing.T, pool *pgxpool.Pool) []outboxEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT topic, event_type, value FROM outbox_events ORDER BY id`)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxEvent
	for rows.Next() {
		var ev outboxEvent
		var value []byte
		if err := rows.Scan(&ev.topic, &ev.eventType, &value); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		if err := json.Unmarshal(value, &ev.envelope); err != nil {
			t.Fatalf("outbox value is not a CloudEvent: %v (%s)", err, value)
		}
		if err := json.Unmarshal(ev.envelope["data"], &ev.data); err != nil {
			t.Fatalf("CloudEvent data is not an object: %v (%s)", err, value)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

// initiatedBy returns data.initiated_by decoded as strings, and whether the
// key is present at all.
func (e outboxEvent) initiatedBy(t *testing.T) (map[string]string, bool) {
	t.Helper()
	raw, ok := e.data["initiated_by"]
	if !ok {
		return nil, false
	}
	var by map[string]string
	if err := json.Unmarshal(raw, &by); err != nil {
		t.Fatalf("initiated_by is not an object of strings: %v (%s)", err, raw)
	}
	return by, true
}

// TestA2AAssignLabor_OutboxCarriesInitiatedBy proves, against a real Postgres
// and the production cmd/a2a composition, that the PERSISTED analytics outbox
// row of an A2A-initiated assignment carries data.initiated_by with via=a2a
// stamped by the adapter (plus the optional attribution the caller gave), that
// a move is attributed too, that no CloudEvents attribute is added, and that
// the REST path (which calls AssignLabor.Execute, never the agent entry point)
// persists a row with NO initiated_by.
func TestA2AAssignLabor_OutboxCarriesInitiatedBy(t *testing.T) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	r, closeRepos, err := newRepos(ctx, quietLogger(), url, url, migrationsDirForTest(t))
	if err != nil {
		t.Fatalf("newRepos: %v", err)
	}
	t.Cleanup(closeRepos)

	built, err := composition.BuildEventPublisher(composition.PublisherConfig{
		Brokers: []string{"127.0.0.1:1"}, RunRelay: false,
	}, r.pool, r.shiftPlans, quietLogger())
	if err != nil {
		t.Fatalf("BuildEventPublisher: %v", err)
	}
	t.Cleanup(built.Close)

	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "STOW", MatchPrefix: "stow", RequiredCapabilities: []string{"stow"}},
	})
	for _, id := range []shared.AssociateId{"assoc-a2a", "assoc-rest"} {
		shift := associate.NewAssociateShift(id, []shared.Certification{"pack", "stow"}, time.Now())
		shift.PullEvents()
		if err := r.associates.Save(ctx, shift); err != nil {
			t.Fatalf("seed associate %s: %v", id, err)
		}
	}

	deps := buildDeps(r, built.Publisher, catalogue, 8, quietLogger())
	executor := inbounda2a.NewExecutor(deps)
	srv := httptest.NewServer(newRouter(inbounda2a.Handler(executor), inbounda2a.Card(inbounda2a.Config{AgentURL: "http://placeholder/", Version: "itest"}), "workforce-management-a2a-itest"))
	t.Cleanup(srv.Close)
	client, err := a2aclient.NewFromCard(ctx, inbounda2a.Card(inbounda2a.Config{AgentURL: srv.URL + "/a2a", Version: "itest"}))
	if err != nil {
		t.Fatalf("NewFromCard: %v", err)
	}

	call := func(associateId, pathId string, extra map[string]any) {
		t.Helper()
		args := map[string]any{"associateId": associateId, "pathId": pathId}
		for k, v := range extra {
			args[k] = v
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"skill": "assign_labor", "arguments": args}))
		resp, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
		if err != nil {
			t.Fatalf("SendMessage assign_labor(%s,%s): %v", associateId, pathId, err)
		}
		if st := problemStatus(t, resp); st != 0 {
			t.Fatalf("assign_labor(%s,%s) answered a %d problem", associateId, pathId, st)
		}
	}

	// 1. First assignment over A2A, fully attributed.
	call("assoc-a2a", "pack", map[string]any{"proposalId": " prop-42 ", "approvedBy": "supervisor-7", "agentId": "warehouse-ops-agent"})
	// 2. A move over A2A with no attribution at all: still marked via=a2a.
	call("assoc-a2a", "stow", nil)
	// 3. The REST path: the HTTP handler calls AssignLabor.Execute.
	if _, err := deps.AssignLabor.Execute(ctx, "assoc-rest", "pack"); err != nil {
		t.Fatalf("REST-path assignment: %v", err)
	}

	evs := outboxEvents(t, r.pool)
	if len(evs) != 3 {
		t.Fatalf("outbox holds %d rows, want 3", len(evs))
	}

	wantTypes := []string{cloudevents.TypeLaborAssigned, cloudevents.TypeLaborReassigned, cloudevents.TypeLaborAssigned}
	for i, ev := range evs {
		if ev.eventType != wantTypes[i] {
			t.Errorf("row %d type = %q, want %q", i, ev.eventType, wantTypes[i])
		}
		for key := range ev.envelope {
			if strings.Contains(strings.ToLower(key), "initiated") || strings.Contains(strings.ToLower(key), "authority") {
				t.Errorf("row %d: CloudEvents attribute %q was added; attribution is event data only", i, key)
			}
		}
	}

	by, ok := evs[0].initiatedBy(t)
	want := map[string]string{"via": "a2a", "proposal_id": "prop-42", "approved_by": "supervisor-7", "agent_id": "warehouse-ops-agent"}
	if !ok || !maps.Equal(by, want) {
		t.Errorf("A2A LaborAssigned initiated_by = %v (present=%v), want %v", by, ok, want)
	}

	by, ok = evs[1].initiatedBy(t)
	if !ok || !maps.Equal(by, map[string]string{"via": "a2a"}) {
		t.Errorf("A2A LaborReassigned initiated_by = %v (present=%v), want only via=a2a", by, ok)
	}

	if by, ok := evs[2].initiatedBy(t); ok {
		t.Errorf("REST-path LaborAssigned carries initiated_by %v; it must be omitted", by)
	}
}
