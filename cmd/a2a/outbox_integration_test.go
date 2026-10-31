//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	inbounda2a "github.com/claudioed/workforce-management/internal/adapters/inbound/a2a"
	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/composition"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

type outboxRow struct {
	topic, eventType string
	published        bool
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT topic, event_type, published_at IS NOT NULL FROM outbox_events ORDER BY id`)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.topic, &r.eventType, &r.published); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

// problemStatus returns the in-band problem status of an agent reply, or 0
// when the reply carries data instead.
func problemStatus(t *testing.T, resp a2a.SendMessageResult) int {
	t.Helper()
	msg, ok := resp.(*a2a.Message)
	if !ok || len(msg.Parts) == 0 {
		t.Fatalf("reply = %T, want a message", resp)
	}
	data, ok := msg.Parts[0].Content.(a2a.Data)
	if !ok {
		t.Fatalf("first reply part is %T, want data", msg.Parts[0].Content)
	}
	raw, _ := json.Marshal(data.Value)
	var env struct {
		Problem *struct {
			Status int `json:"status"`
		} `json:"problem"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("reply is not the skill envelope: %v", err)
	}
	if env.Problem == nil {
		return 0
	}
	return env.Problem.Status
}

// TestA2AAssignLabor_WritesOutboxRows is the end-to-end proof that a
// DELEGATED assignment is a first-class write: it boots a real Postgres
// (testcontainers), wires cmd/a2a's production composition with
// EVENT_PUBLISHER=kafka, sends the assign_labor skill over A2A JSON-RPC, and
// asserts the transactional outbox holds an UNPUBLISHED row for the analytics
// topic per event (LaborAssigned, then LaborReassigned) — the same rows REST
// and MCP produce (ADR-0026). The relay stays in cmd/workforce, so rows (not
// Kafka messages) are the contract of this binary. A catalogue-invalid pathId
// is answered as a problem (ADR-0013) and stores nothing. A2A never becomes a
// data-sync channel: the facts leave only as outbox rows.
func TestA2AAssignLabor_WritesOutboxRows(t *testing.T) {
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

	// Brokers are never dialled: this process only inserts outbox rows.
	built, err := composition.BuildEventPublisher(composition.PublisherConfig{
		Brokers: []string{"127.0.0.1:1"}, RunRelay: false,
	}, r.pool, r.shiftPlans, quietLogger())
	if err != nil {
		t.Fatalf("BuildEventPublisher: %v", err)
	}
	t.Cleanup(built.Close)
	if built.Relay != nil {
		t.Fatal("cmd/a2a must not run the outbox relay")
	}

	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "STOW", MatchPrefix: "stow", RequiredCapabilities: []string{"stow"}},
	})

	// Seed one associate certified for both paths. Persisted straight
	// through the repo, so the outbox starts empty.
	shift := associate.NewAssociateShift("assoc-a2a-1", []shared.Certification{"pack", "stow"}, time.Now())
	shift.PullEvents()
	if err := r.associates.Save(ctx, shift); err != nil {
		t.Fatalf("seed associate: %v", err)
	}

	executor := inbounda2a.NewExecutor(buildDeps(r, built.Publisher, catalogue, 8, quietLogger()))
	srv := httptest.NewServer(newRouter(inbounda2a.Handler(executor), inbounda2a.Card(inbounda2a.Config{AgentURL: "http://placeholder/", Version: "itest"}), "workforce-management-a2a-itest"))
	t.Cleanup(srv.Close)

	card := inbounda2a.Card(inbounda2a.Config{AgentURL: srv.URL + "/a2a", Version: "itest"})
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatalf("NewFromCard: %v", err)
	}

	assign := func(pathId string) int {
		t.Helper()
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{
			"skill":     "assign_labor",
			"arguments": map[string]any{"associateId": "assoc-a2a-1", "pathId": pathId},
		}))
		resp, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
		if err != nil {
			t.Fatalf("SendMessage assign_labor(%s): %v", pathId, err)
		}
		return problemStatus(t, resp)
	}

	// ADR-0013: a path id outside the catalogue is a 400 problem and stores nothing.
	if st := assign("hazmat"); st != 400 {
		t.Fatalf("assign_labor(hazmat) problem status = %d, want 400 from the path catalogue", st)
	}
	if rows := outboxRows(t, r.pool); len(rows) != 0 {
		t.Fatalf("rejected assign_labor stored %d outbox rows, want 0", len(rows))
	}

	if st := assign("pack"); st != 0 {
		t.Fatalf("assign_labor(pack) answered a %d problem", st)
	}
	if st := assign("stow"); st != 0 {
		t.Fatalf("assign_labor(stow) answered a %d problem", st)
	}

	rows := outboxRows(t, r.pool)
	want := []string{cloudevents.TypeLaborAssigned, cloudevents.TypeLaborReassigned}
	if len(rows) != len(want) {
		t.Fatalf("outbox rows = %+v, want exactly %d (LaborAssigned then LaborReassigned)", rows, len(want))
	}
	for i, row := range rows {
		if row.topic != outboundkafka.AnalyticsTopic {
			t.Errorf("row %d topic = %q, want %q", i, row.topic, outboundkafka.AnalyticsTopic)
		}
		if row.eventType != want[i] {
			t.Errorf("row %d event_type = %q, want %q", i, row.eventType, want[i])
		}
		if row.published {
			t.Errorf("row %d is already published; this binary must not run the relay", i)
		}
	}
}
