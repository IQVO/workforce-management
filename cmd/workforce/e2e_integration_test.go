//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ce "github.com/cloudevents/sdk-go/v2/event"
	"github.com/jackc/pgx/v5/pgxpool"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
)

// The end-to-end proof that the shipped binary is really wired: the test boots
// the REAL entry point (run, in-process) against testcontainers Postgres and
// Kafka with nothing but the environment a deployment sets, and drives it only
// through REST and Kafka. It feeds the two consumers (the process-path
// catalogue and the labor-performance measured-rate cache) by producing their
// CloudEvents, reads the effects from Postgres and from the two output topics,
// restarts the service in-process, and finally takes the dependencies away to
// watch /readyz follow them.

const (
	topicEvents    = "warehouse.workforce.events"
	topicAnalytics = "warehouse.workforce.analytics"
	topicCatalogue = "warehouse.process-path-management.events"
	topicLabor     = "warehouse.labor-performance.events"

	e2eSource  = "/warehouse/workforce-management"
	typePrefix = "com.warehouse.wes.workforce-management."
)

// containerStartTimeout bounds a testcontainers start.
const containerStartTimeout = 120 * time.Second

func startPostgres(t *testing.T) (*tcpostgres.PostgresContainer, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce_e2e"), tcpostgres.WithUsername("e2e"), tcpostgres.WithPassword("e2e"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	return c, url
}

func startKafka(t *testing.T) (*tckafka.KafkaContainer, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()
	c, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1", tckafka.WithClusterID("workforce-e2e"))
	if err != nil {
		t.Fatalf("start kafka: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	brokers, err := c.Brokers(ctx)
	if err != nil {
		t.Fatalf("kafka brokers: %v", err)
	}
	return c, brokers
}

// topicExists asks the broker for the topic through a metadata request that
// cannot create it (kafka-go's Client.Metadata never auto-creates).
func topicExists(t *testing.T, brokers []string, topic string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := (&kafkago.Client{Addr: kafkago.TCP(brokers...)}).Metadata(ctx, &kafkago.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	for _, tp := range resp.Topics {
		if tp.Name == topic && tp.Error == nil {
			return true
		}
	}
	return false
}

// createTopic creates a one-partition topic and waits for its leader.
func createTopic(t *testing.T, brokers []string, topic string) {
	t.Helper()
	conn, err := kafkago.Dial("tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()
	if err := conn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("create topic %s: %v", topic, err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if parts, err := conn.ReadPartitions(topic); err == nil && len(parts) == 1 && parts[0].Leader.ID != 0 {
			return
		}
	}
	t.Fatalf("topic %s never got a leader", topic)
}

// consume reads exactly n messages of the one-partition topic from the start,
// bounded by a deadline.
func consume(t *testing.T, brokers []string, topic string, n int) []kafkago.Message {
	t.Helper()
	r := kafkago.NewReader(kafkago.ReaderConfig{Brokers: brokers, Topic: topic, Partition: 0, MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 200 * time.Millisecond})
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.SetOffset(kafkago.FirstOffset); err != nil {
		t.Fatal(err)
	}
	var out []kafkago.Message
	for len(out) < n {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("read %s: got %d of %d messages: %v", topic, len(out), n, err)
		}
		out = append(out, m)
	}
	return out
}

// noMoreMessages fails if the topic holds more than want messages after a
// settling period (nothing was duplicated).
func noMoreMessages(t *testing.T, brokers []string, topic string, want int) {
	t.Helper()
	r := kafkago.NewReader(kafkago.ReaderConfig{Brokers: brokers, Topic: topic, Partition: 0, MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 200 * time.Millisecond})
	defer r.Close()
	if err := r.SetOffset(kafkago.FirstOffset); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := 0
	for {
		if _, err := r.ReadMessage(ctx); err != nil {
			break
		}
		got++
	}
	if got != want {
		t.Fatalf("%s holds %d messages, want exactly %d", topic, got, want)
	}
}

// produceForeign writes one CloudEvents 1.0 structured-mode message, as a
// sibling context would, on a topic this service only consumes.
func produceForeign(t *testing.T, brokers []string, topic, source, evType, subject string, data any) {
	t.Helper()
	e := ce.New(ce.CloudEventsVersionV1)
	e.SetID(fmt.Sprintf("%s-%d", subject, time.Now().UnixNano()))
	e.SetSource(source)
	e.SetType(evType)
	e.SetSubject(subject)
	e.SetTime(time.Now().UTC())
	if err := e.SetData(ce.ApplicationJSON, data); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	w := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic, RequiredAcks: kafkago.RequireAll, BatchTimeout: 10 * time.Millisecond}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.WriteMessages(ctx, kafkago.Message{Key: []byte(subject), Value: raw, Headers: []kafkago.Header{cloudevents.ContentTypeHeader()}}); err != nil {
		t.Fatalf("produce %s to %s: %v", evType, topic, err)
	}
}

// service is one running instance of the real entry point.
type service struct {
	t      *testing.T
	base   string
	cancel context.CancelFunc
	done   chan error
}

// env is the environment a deployment sets.
type env struct {
	dbURL    string
	brokers  []string
	addr     string
	capacity string // fulfillment-execution stand-in (a REST sync edge, out of scope)
}

func newEnv(t *testing.T, dbURL string, brokers []string, capacityURL string) env {
	t.Helper()
	return env{dbURL: dbURL, brokers: brokers, addr: freeAddr(t), capacity: capacityURL}
}

func (e env) apply(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", e.dbURL)
	t.Setenv("MIGRATIONS_DATABASE_URL", "")
	t.Setenv("MIGRATIONS_PATH", "../../migrations")
	t.Setenv("KAFKA_BROKERS", strings.Join(e.brokers, ","))
	t.Setenv("EVENT_PUBLISHER", "kafka")
	t.Setenv("PATH_CATALOGUE_SOURCE", "kafka")
	t.Setenv("LABOR_PERFORMANCE_MODE", "kafka-cache")
	t.Setenv("INSTALLED_CAPACITY_MODE", "http")
	t.Setenv("FULFILLMENT_EXECUTION_BASE_URL", e.capacity)
	t.Setenv("HTTP_ADDR", e.addr)
	t.Setenv("SHUTDOWN_DRAIN_DELAY", "0")
	t.Setenv("OUTBOX_RELAY_INTERVAL", "1s")
	t.Setenv("HOUSEKEEPING_INTERVAL", "0")
	t.Setenv("LOG_LEVEL", "warn")
}

// start boots run in-process and waits until the listener answers /healthz.
func (e env) start(t *testing.T) *service {
	t.Helper()
	e.apply(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := &service{t: t, base: "http://" + e.addr, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- run(ctx) }()
	t.Cleanup(s.stopQuietly)
	waitFor(t, 120*time.Second, "the service to listen on "+e.addr, func() bool {
		select {
		case err := <-s.done:
			t.Fatalf("run returned while booting: %v", err)
		default:
		}
		status, _ := s.try(http.MethodGet, "/healthz", "", nil)
		return status == http.StatusOK
	})
	return s
}

// stop cancels run and requires a clean shutdown within a bound.
func (s *service) stop() {
	s.t.Helper()
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(60 * time.Second):
		s.t.Fatal("run did not return after the shutdown")
	}
	s.done <- nil // let the cleanup find it stopped
}

func (s *service) stopQuietly() {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(60 * time.Second):
	}
}

// try performs a request and returns -1 when the connection fails.
func (s *service) try(method, path, body string, headers map[string]string) (int, string) {
	req, err := http.NewRequestWithContext(context.Background(), method, s.base+path, strings.NewReader(body))
	if err != nil {
		return -1, err.Error()
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (s *service) call(method, path, body string, want int) string {
	s.t.Helper()
	return s.callWith(method, path, body, nil, want)
}

func (s *service) callWith(method, path, body string, headers map[string]string, want int) string {
	s.t.Helper()
	status, raw := s.try(method, path, body, headers)
	if status != want {
		s.t.Fatalf("%s %s = %d %s, want %d", method, path, status, raw, want)
	}
	return raw
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// pgView reads the service's own database.
type pgView struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func newPGView(t *testing.T, url string) pgView {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pgView{t: t, pool: pool}
}

func (v pgView) count(query string, args ...any) int {
	v.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := v.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		v.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// outboxSummary lists "id:event_type" of every outbox row, for failure messages.
func (v pgView) outboxSummary() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := v.pool.Query(ctx, `SELECT id::text || ':' || event_type FROM outbox_events ORDER BY id`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err.Error()
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// The CloudEvents context attributes of a published message, plus its data.
type envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	DataSchema      string          `json:"dataschema"`
	Data            json.RawMessage `json:"data"`
}

func decodeEnvelope(t *testing.T, m kafkago.Message) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(m.Value, &e); err != nil {
		t.Fatalf("not a CloudEvent: %v: %s", err, m.Value)
	}
	return e
}

// assertEnvelope checks every context attribute of a message of this service.
func assertEnvelope(t *testing.T, m kafkago.Message, entity, eventName, stream, subject, key string) envelope {
	t.Helper()
	e := decodeEnvelope(t, m)
	schema := fmt.Sprintf("urn:warehouse:workforce-management:%s:%s:v1", stream, eventName)
	wantType := typePrefix + entity + "." + eventName
	if e.SpecVersion != "1.0" || e.Type != wantType || e.Source != e2eSource ||
		e.DataSchema != schema || e.DataContentType != "application/json" || e.Subject != subject || e.ID == "" {
		t.Fatalf("envelope = %+v, want 1.0 %s from %s (%s) about %s", e, wantType, e2eSource, schema, subject)
	}
	if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
		t.Fatalf("time %q is not RFC 3339: %v", e.Time, err)
	}
	if string(m.Key) != key {
		t.Fatalf("kafka key = %q, want %q", m.Key, key)
	}
	var ct string
	for _, h := range m.Headers {
		if h.Key == "content-type" {
			ct = string(h.Value)
		}
	}
	if ct != "application/cloudevents+json; charset=UTF-8" {
		t.Fatalf("content-type header = %q", ct)
	}
	return e
}

// eventNames lists the event names (last type segment) of the messages in order.
func eventNames(t *testing.T, msgs []kafkago.Message) []string {
	t.Helper()
	names := make([]string, len(msgs))
	for i, m := range msgs {
		typ := decodeEnvelope(t, m).Type
		names[i] = typ[strings.LastIndex(typ, ".")+1:]
	}
	return names
}

func readyz(s *service) (int, string) { return s.try(http.MethodGet, "/readyz", "", nil) }

func waitReadyzStatus(t *testing.T, s *service, want int, what string) string {
	t.Helper()
	var body string
	waitFor(t, 60*time.Second, what, func() bool {
		var status int
		status, body = readyz(s)
		return status == want
	})
	return body
}

// capacityStandIn answers GET /capacity/{capability} like fulfillment-execution
// does: five installed stations. It is the REST sync edge ADR-0038 leaves alone.
func capacityStandIn(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capability := strings.TrimPrefix(r.URL.Path, "/capacity/")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"capability":%q,"installed":5}`, capability)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Phase 1: the output topics do not exist yet (a fresh cluster before
// anything published). Readiness needs Postgres plus a broker answering
// metadata, NOT topics, so the service is ready at once, and it creates no
// output topic itself.
func phaseTopicsMissing(t *testing.T, s *service, brokers []string) {
	t.Helper()
	body := waitReadyzStatus(t, s, http.StatusOK, "/readyz to be 200 although no output topic exists")
	if !strings.Contains(body, "ready") {
		t.Fatalf("readyz = %s", body)
	}
	for _, topic := range []string{topicEvents, topicAnalytics} {
		if topicExists(t, brokers, topic) {
			t.Fatalf("topic %s exists: the service must never create topics itself", topic)
		}
	}
}

// Phase 2: the two consumers. Sibling contexts publish the process-path
// catalogue and a labor-performance measurement; the service picks both up
// live. Until the path is in the catalogue the REST API rejects it. Returns
// the Idempotency-Key of the assignment that finally succeeded.
func phaseConsumers(t *testing.T, s *service, brokers []string) string {
	t.Helper()
	s.call(http.MethodPost, "/associates/assoc-1/start-shift", `{"certifications":["pack"],"siteCode":"WH1"}`, http.StatusCreated)
	if status, _ := s.try(http.MethodPost, "/associates/assoc-1/assignments", `{"pathId":"PACK"}`, map[string]string{"Idempotency-Key": "assign-before-catalogue"}); status != http.StatusBadRequest {
		t.Fatalf("assigning an unknown path = %d, want 400 unknown-path-id before the catalogue is replayed", status)
	}

	produceForeign(t, brokers, topicCatalogue, "/warehouse/process-path-management", cloudevents.TypeProcessPathCreated, "PACK",
		map[string]any{"path_id": "PACK", "match_prefix": "pack", "direct": true, "required_capabilities": []string{"pack"}})
	assignKey := ""
	waitFor(t, 60*time.Second, "the catalogue consumer to learn PACK", func() bool {
		key := fmt.Sprintf("assign-%d", time.Now().UnixNano())
		status, _ := s.try(http.MethodPost, "/associates/assoc-1/assignments", `{"pathId":"PACK"}`, map[string]string{"Idempotency-Key": key})
		if status == http.StatusCreated {
			assignKey = key
		}
		return assignKey != ""
	})

	produceForeign(t, brokers, topicLabor, "/warehouse/labor-performance", cloudevents.TypeTaskPerformanceRecorded, "task-1",
		map[string]any{"task_id": "task-1", "associate_id": "assoc-1", "task_type": "PACK", "efficiency_pct": 100.0, "actual_seconds": 120,
			"completed_at": time.Now().UTC().Format(time.RFC3339)})
	waitFor(t, 60*time.Second, "the measured-rate cache to learn PACK", func() bool {
		_, body := s.try(http.MethodPost, "/paths/pack/plan/propose", `{"siteCode":"WH1","charge":300}`, nil)
		return strings.Contains(body, `"rateSource":"measured"`)
	})
	var got struct {
		ProposedHeads int     `json:"proposedHeads"`
		ResolvedRate  float64 `json:"resolvedRate"`
		RateSource    string  `json:"rateSource"`
	}
	if err := json.Unmarshal([]byte(s.call(http.MethodPost, "/paths/pack/plan/propose", `{"siteCode":"WH1","charge":300}`, http.StatusOK)), &got); err != nil {
		t.Fatal(err)
	}
	if got.RateSource != "measured" || got.ResolvedRate != 30 || got.ProposedHeads != 10 {
		t.Fatalf("propose from the measured rate (120 s per task = 30 per head-hour) = %+v", got)
	}
	return assignKey
}

const commitBody = `{"siteCode":"WH1","shiftId":"shift-1","lines":[{"pathId":"PACK","plannedHeads":2,"plannedRate":30,"plannedHours":16,"installedStations":5}]}`

// Phase 3: the write API, driven only through REST.
func phaseWrites(t *testing.T, s *service) {
	t.Helper()
	s.call(http.MethodPost, "/associates/assoc-1/certifications", `{"certification":"stow"}`, http.StatusNoContent)
	s.callWith(http.MethodPost, "/shift-plans", commitBody, map[string]string{"Idempotency-Key": "commit-1"}, http.StatusCreated)
	body := s.call(http.MethodGet, "/sites/WH1/shifts/shift-1/staffing-gap", "", http.StatusOK)
	if !strings.Contains(body, `"plannedHeads":2`) || !strings.Contains(body, `"activeHeads":1`) {
		t.Fatalf("staffing gap = %s, want 2 planned and 1 active head", body)
	}
}

// Phase 4: the rows are in Postgres, and the relay drained every outbox row.
func phaseRows(t *testing.T, db pgView) {
	t.Helper()
	if n := db.count(`SELECT count(*) FROM associate_shift WHERE associate_id = 'assoc-1' AND 'stow' = ANY(certifications)`); n != 1 {
		t.Fatalf("associate_shift rows for assoc-1 = %d, want 1 certified for stow", n)
	}
	if n := db.count(`SELECT count(*) FROM labor_assignment WHERE associate_id = 'assoc-1' AND upper(active_path_id) = 'PACK'`); n != 1 {
		t.Fatalf("labor_assignment rows = %d, want assoc-1 active on PACK", n)
	}
	if n := db.count(`SELECT count(*) FROM path_plan WHERE building_id = 'WH1' AND shift_id = 'shift-1' AND path_id = 'PACK' AND planned_heads = 2`); n != 1 {
		t.Fatalf("path_plan rows = %d, want the committed PACK line", n)
	}
	waitFor(t, 60*time.Second, "the relay to publish every outbox row", func() bool {
		return db.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})
	if n := db.count(`SELECT count(*) FROM outbox_events WHERE topic = $1`, topicEvents); n != 1 {
		t.Fatalf("outbox rows for %s = %d, want 1 (one ShiftPlanCommitted line)", topicEvents, n)
	}
}

// Phase 5: the CloudEvents on both output topics, byte-exact envelope.
func phaseTopics(t *testing.T, db pgView, brokers []string) {
	t.Helper()
	ev := consume(t, brokers, topicEvents, 1)
	e := assertEnvelope(t, ev[0], "shiftplan", "ShiftPlanCommitted", "events", "WH1/shift-1", "WH1/shift-1")
	var line struct {
		SiteCode     string  `json:"site_code"`
		BuildingID   string  `json:"building_id"`
		ShiftID      string  `json:"shift_id"`
		PathID       string  `json:"path_id"`
		PlannedHeads int     `json:"planned_heads"`
		PlannedRate  float64 `json:"planned_rate"`
		PlannedHours float64 `json:"planned_hours"`
	}
	if err := json.Unmarshal(e.Data, &line); err != nil || line.SiteCode != "WH1" || line.BuildingID != "WH1" || line.ShiftID != "shift-1" ||
		line.PathID != "PACK" || line.PlannedHeads != 2 || line.PlannedRate != 30 || line.PlannedHours != 16 {
		t.Fatalf("ShiftPlanCommitted data = %+v (%v)", line, err)
	}

	total := db.count(`SELECT count(*) FROM outbox_events`)
	assertAnalyticsTopic(t, consume(t, brokers, topicAnalytics, total-1))
}

// assertAnalyticsTopic checks the analytics stream: the first write ever is
// the shift start; the rest are counted by name (the propose polls publish a
// variable number of ShiftPlanProposed).
func assertAnalyticsTopic(t *testing.T, an []kafkago.Message) {
	t.Helper()
	names := eventNames(t, an)
	counts := map[string]int{}
	for _, n := range names {
		counts[n]++
	}
	if names[0] != "AssociateShiftStarted" || counts["AssociateShiftStarted"] != 1 || counts["AssociateCertified"] != 1 ||
		counts["LaborAssigned"] != 1 || counts["ShiftPlanProposed"] < 1 {
		t.Fatalf("analytics topic carries %v, want AssociateShiftStarted first, one AssociateCertified, one LaborAssigned and >=1 ShiftPlanProposed", names)
	}
	assertEnvelope(t, an[0], "associate", "AssociateShiftStarted", "analytics", "assoc-1", "assoc-1")
	for i, n := range names {
		if n == "LaborAssigned" {
			assertLaborAssigned(t, an[i])
		}
	}
}

func assertLaborAssigned(t *testing.T, m kafkago.Message) {
	t.Helper()
	la := assertEnvelope(t, m, "assignment", "LaborAssigned", "analytics", "assoc-1", "assoc-1")
	var assigned struct {
		AssociateID string `json:"associate_id"`
		PathID      string `json:"path_id"`
	}
	if err := json.Unmarshal(la.Data, &assigned); err != nil || assigned.AssociateID != "assoc-1" || !strings.EqualFold(assigned.PathID, "PACK") {
		t.Fatalf("LaborAssigned data = %+v (%v)", assigned, err)
	}
}

// Phase 6: repeating the same keyed request adds no row and no event.
func phaseReplay(t *testing.T, s *service, db pgView, brokers []string, assignKey string) {
	t.Helper()
	before := db.count(`SELECT count(*) FROM outbox_events`)
	s.callWith(http.MethodPost, "/associates/assoc-1/assignments", `{"pathId":"PACK"}`, map[string]string{"Idempotency-Key": assignKey}, http.StatusCreated)
	s.callWith(http.MethodPost, "/shift-plans", commitBody, map[string]string{"Idempotency-Key": "commit-1"}, http.StatusCreated)
	if n := db.count(`SELECT count(*) FROM outbox_events`); n != before {
		t.Fatalf("outbox rows after the replay = %d, want still %d", n, before)
	}
	noMoreMessages(t, brokers, topicEvents, 1)
}

// Phase 7: restart the real entry point in-process on the same database and
// broker: nothing is lost, nothing is published twice.
func phaseRestart(t *testing.T, e env, s *service, db pgView, brokers []string) *service {
	t.Helper()
	before := db.count(`SELECT count(*) FROM outbox_events`)
	analyticsBefore := db.count(`SELECT count(*) FROM outbox_events WHERE topic = $1`, topicAnalytics)
	s.stop()
	s2 := e.start(t)
	body := s2.call(http.MethodGet, "/sites/WH1/shifts/shift-1/staffing-gap", "", http.StatusOK)
	if !strings.Contains(body, `"plannedHeads":2`) || !strings.Contains(body, `"activeHeads":1`) {
		t.Fatalf("after the restart the staffing gap = %s, want 2 planned and 1 active head", body)
	}
	s2.call(http.MethodPost, "/associates/assoc-2/start-shift", `{"certifications":["pack"],"siteCode":"WH1"}`, http.StatusCreated)
	waitFor(t, 60*time.Second, "the relay to publish assoc-2's event after the restart", func() bool {
		return db.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})
	// The restart itself writes nothing: only the staffing-gap read (it
	// publishes PathUnderstaffed, as before the restart) and assoc-2's shift
	// start add rows.
	if n := db.count(`SELECT count(*) FROM outbox_events`); n != before+2 {
		t.Fatalf("outbox rows after the restart = %d, want %d (a PathUnderstaffed and assoc-2's AssociateShiftStarted); rows: %s", n, before+2, db.outboxSummary())
	}
	// Every outbox row reached its topic exactly once: the topic holds as many
	// messages as the outbox has rows for it, all with distinct ids.
	eventsRows := db.count(`SELECT count(*) FROM outbox_events WHERE topic = $1`, topicEvents)
	analyticsRows := db.count(`SELECT count(*) FROM outbox_events WHERE topic = $1`, topicAnalytics)
	if analyticsRows < analyticsBefore+1 {
		t.Fatalf("analytics outbox rows = %d, want at least %d", analyticsRows, analyticsBefore+1)
	}
	ids := map[string]bool{}
	for _, m := range consume(t, brokers, topicAnalytics, analyticsRows) {
		ids[decodeEnvelope(t, m).ID] = true
	}
	for _, m := range consume(t, brokers, topicEvents, eventsRows) {
		ids[decodeEnvelope(t, m).ID] = true
	}
	if len(ids) != analyticsRows+eventsRows {
		t.Fatalf("%d distinct CloudEvents ids over %d messages: an event was published twice", len(ids), analyticsRows+eventsRows)
	}
	noMoreMessages(t, brokers, topicAnalytics, analyticsRows)
	noMoreMessages(t, brokers, topicEvents, eventsRows)
	return s2
}

// Phase 8: /readyz follows the dependencies. Postgres goes first (Kafka still
// up): only postgres is named. Then Kafka goes: kafka is named too. /healthz
// stays liveness and answers 200 throughout.
func phaseDependenciesGoAway(t *testing.T, s *service, pg *tcpostgres.PostgresContainer, kc *tckafka.KafkaContainer) {
	t.Helper()
	stopTimeout := 10 * time.Second
	if err := pg.Stop(context.Background(), &stopTimeout); err != nil {
		t.Fatalf("stop postgres: %v", err)
	}
	body := waitReadyzStatus(t, s, http.StatusServiceUnavailable, "/readyz to turn 503 once Postgres is gone")
	if !strings.Contains(body, "dependency-unavailable") || !strings.Contains(body, "postgres") || strings.Contains(body, "kafka") {
		t.Fatalf("readyz with Postgres down = %s, want dependency-unavailable naming only postgres", body)
	}
	if err := kc.Stop(context.Background(), &stopTimeout); err != nil {
		t.Fatalf("stop kafka: %v", err)
	}
	waitFor(t, 60*time.Second, "/readyz to name kafka as well", func() bool {
		_, body := readyz(s)
		return strings.Contains(body, "postgres") && strings.Contains(body, "kafka")
	})
	if status, body := s.try(http.MethodGet, "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz with every dependency down = %d %s, want 200 (liveness only)", status, body)
	}
}

func TestRunOnRealPostgresAndKafka(t *testing.T) {
	pg, dbURL := startPostgres(t)
	kc, brokers := startKafka(t)
	// The topics this service consumes are owned by sibling contexts and exist
	// on the cluster; the output topics do not exist until the first write.
	createTopic(t, brokers, topicCatalogue)
	createTopic(t, brokers, topicLabor)
	e := newEnv(t, dbURL, brokers, capacityStandIn(t))
	db := newPGView(t, dbURL)

	s := e.start(t)
	phaseTopicsMissing(t, s, brokers)
	assignKey := phaseConsumers(t, s, brokers)
	phaseWrites(t, s)
	phaseRows(t, db)
	phaseTopics(t, db, brokers)
	phaseReplay(t, s, db, brokers, assignKey)
	s = phaseRestart(t, e, s, db, brokers)
	phaseDependenciesGoAway(t, s, pg, kc)
}
