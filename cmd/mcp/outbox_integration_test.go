//go:build integration

package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	inboundmcp "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
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

// TestMCPAssignLabor_WritesOutboxRows is the end-to-end regression for the
// audit finding that MCP-initiated LaborAssigned / LaborReassigned never
// reached Kafka (LogPublisher + no UnitOfWork). It boots a real Postgres
// (testcontainers), wires cmd/mcp's production composition with
// EVENT_PUBLISHER=kafka, calls the assign_labor tool over MCP Streamable
// HTTP, and asserts that the transactional outbox holds an UNPUBLISHED row
// for the analytics topic per event (LaborAssigned, then LaborReassigned).
// The relay stays in cmd/workforce, so rows (not Kafka messages) are the
// contract of this binary (ADR-0016). The integration topic only carries
// ShiftPlanCommitted, which no MCP tool raises, so it correctly gets no row.
// A catalogue-invalid pathId is rejected (ADR-0013) before anything is
// stored.
func TestMCPAssignLabor_WritesOutboxRows(t *testing.T) {
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
		Kind: composition.KafkaPublisher, Brokers: []string{"127.0.0.1:1"}, RunRelay: false,
	}, r.pool, r.shiftPlans, quietLogger())
	if err != nil {
		t.Fatalf("BuildEventPublisher: %v", err)
	}
	t.Cleanup(built.Close)
	if built.Relay != nil {
		t.Fatal("cmd/mcp must not run the outbox relay")
	}

	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "STOW", MatchPrefix: "stow", RequiredCapabilities: []string{"stow"}},
	})

	// Seed one associate certified for both paths. Persisted straight
	// through the repo, so the outbox starts empty.
	shift := associate.NewAssociateShift("assoc-mcp-1", []shared.Certification{"pack", "stow"}, time.Now())
	shift.PullEvents()
	if err := r.associates.Save(ctx, shift); err != nil {
		t.Fatalf("seed associate: %v", err)
	}

	srv := httptest.NewServer(newRouter(inboundmcp.Handler(inboundmcp.NewServer(buildDeps(r, built.Publisher, catalogue, 8, quietLogger()))), "workforce-management-mcp-itest"))
	t.Cleanup(srv.Close)

	client := sdk.NewClient(&sdk.Implementation{Name: "itest-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	assign := func(pathId string) *sdk.CallToolResult {
		t.Helper()
		out, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name:      "assign_labor",
			Arguments: map[string]any{"associateId": "assoc-mcp-1", "pathId": pathId},
		})
		if err != nil {
			t.Fatalf("call assign_labor(%s): %v", pathId, err)
		}
		return out
	}

	// ADR-0013: a path id outside the catalogue is a tool error and stores nothing.
	if out := assign("hazmat"); !out.IsError {
		t.Fatalf("assign_labor(hazmat) must be rejected by the path catalogue, got %+v", out.Content)
	}
	if rows := outboxRows(t, r.pool); len(rows) != 0 {
		t.Fatalf("rejected assign_labor stored %d outbox rows, want 0", len(rows))
	}

	if out := assign("pack"); out.IsError {
		t.Fatalf("assign_labor(pack) returned a tool error: %+v", out.Content)
	}
	if out := assign("stow"); out.IsError {
		t.Fatalf("assign_labor(stow) returned a tool error: %+v", out.Content)
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
