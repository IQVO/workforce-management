package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/riandyrn/otelchi"
	otelchimetric "github.com/riandyrn/otelchi/metric"

	"github.com/claudioed/workforce-management/internal/analytics/report"
)

// ReportsHandlers is the inbound HTTP adapter for the workforce Labor
// Utilization & Staffing data product's READER. It depends only on the
// read-model port (report.ReportStore); it never touches the OLTP use cases or
// the writer.
type ReportsHandlers struct {
	Store report.ReportStore
}

// laborRowDTO is the wire shape of one report row. It is a dedicated DTO so the
// read-model struct (report.Row) never leaks onto the API. An empty pathId is
// the building-wide, associate-scoped bucket (shifts, breaks, certifications).
type laborRowDTO struct {
	PathId              string  `json:"pathId"`
	HourBucket          string  `json:"hourBucket"`
	ShiftsStarted       int     `json:"shiftsStarted"`
	ShiftsEnded         int     `json:"shiftsEnded"`
	Breaks              int     `json:"breaks"`
	AvgBreakSeconds     float64 `json:"avgBreakSeconds"`
	Certifications      int     `json:"certifications"`
	LaborAssigned       int     `json:"laborAssigned"`
	LaborReassigned     int     `json:"laborReassigned"`
	UnderstaffingEvents int     `json:"understaffingEvents"`
}

// laborReportDTO is the wire shape of a labor report response.
type laborReportDTO struct {
	Rows []laborRowDTO `json:"rows"`
}

// freshnessDTO is the wire shape of the freshness-lag response.
type freshnessDTO struct {
	LagSeconds float64 `json:"lagSeconds"`
}

// GetLabor serves GET /reports/labor. from and to (RFC3339) are required;
// pathId and granularity are optional (granularity defaults to hour).
func (h *ReportsHandlers) GetLabor(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	from, ok := parseRequiredTime(w, r, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := parseRequiredTime(w, r, q.Get("to"), "to")
	if !ok {
		return
	}

	granularity := report.GranularityHour
	if g := q.Get("granularity"); g != "" {
		if g != string(report.GranularityHour) {
			writeReportBadRequest(w, r, "granularity must be 'hour'")
			return
		}
		granularity = report.Granularity(g)
	}

	rep, err := h.Store.Query(r.Context(), report.ReportQuery{
		From:        from,
		To:          to,
		PathId:      q.Get("pathId"),
		Granularity: granularity,
	})
	if err != nil {
		writeReportInternal(w, r, err)
		return
	}

	dto := laborReportDTO{Rows: make([]laborRowDTO, 0, len(rep.Rows))}
	for _, row := range rep.Rows {
		dto.Rows = append(dto.Rows, laborRowDTO{
			PathId:              row.Key.PathId,
			HourBucket:          row.Key.HourBucket.UTC().Format(time.RFC3339),
			ShiftsStarted:       row.ShiftsStarted,
			ShiftsEnded:         row.ShiftsEnded,
			Breaks:              row.Breaks,
			AvgBreakSeconds:     row.AvgBreakSeconds,
			Certifications:      row.Certifications,
			LaborAssigned:       row.LaborAssigned,
			LaborReassigned:     row.LaborReassigned,
			UnderstaffingEvents: row.UnderstaffingEvents,
		})
	}
	writeJSON(w, http.StatusOK, dto)
}

// GetFreshness serves GET /reports/labor/freshness.
func (h *ReportsHandlers) GetFreshness(w http.ResponseWriter, r *http.Request) {
	lag, err := h.Store.FreshnessLag(r.Context())
	if err != nil {
		writeReportInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, freshnessDTO{LagSeconds: lag.Seconds()})
}

// GetReportsHealthz serves GET /healthz for the reports service.
func (h *ReportsHandlers) GetReportsHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{statusKey: "ok"})
}

// parseRequiredTime parses an RFC3339 timestamp, writing an RFC 7807 400 and
// returning ok=false when it is missing or malformed.
func parseRequiredTime(w http.ResponseWriter, r *http.Request, raw, name string) (time.Time, bool) {
	if raw == "" {
		writeReportBadRequest(w, r, "query parameter '"+name+"' is required (RFC3339)")
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeReportBadRequest(w, r, "query parameter '"+name+"' must be an RFC3339 timestamp")
		return time.Time{}, false
	}
	return t, true
}

// writeReportProblem writes an RFC 7807 problem+json response for the reports
// service, reusing the OLTP service's problemDetails shape and type-URI base.
func writeReportProblem(w http.ResponseWriter, r *http.Request, status int, slug, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDetails{
		Type:     problemErrorsURIBase + "/" + slug,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	})
}

// writeReportBadRequest writes the reports service's RFC 7807 400.
func writeReportBadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	writeReportProblem(w, r, http.StatusBadRequest, "invalid-report-query",
		"The report query is malformed or missing a required parameter", detail)
}

// writeReportInternal writes the reports service's RFC 7807 500.
func writeReportInternal(w http.ResponseWriter, r *http.Request, err error) {
	writeReportProblem(w, r, http.StatusInternalServerError, "report-store-error",
		"The report could not be served", err.Error())
}

// NewReportsRouter builds the chi router for the workforce-reports reader
// service. A nil logger falls back to slog.Default(); an empty serviceName
// falls back to DefaultReportsServiceName.
func NewReportsRouter(h *ReportsHandlers, logger *slog.Logger, serviceName string, opts ...RouterOption) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if serviceName == "" {
		serviceName = DefaultReportsServiceName
	}
	var o routerOptions
	for _, opt := range opts {
		opt(&o)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(otelchi.Middleware(serviceName, otelchi.WithChiRoutes(r)))
	r.Use(otelchimetric.NewServerRequestDuration(
		otelchimetric.NewBaseConfig(serviceName),
	))
	r.Use(RequestLogger(logger))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", h.GetReportsHealthz)
	r.Get("/reports/labor", h.GetLabor)
	r.Get("/reports/labor/freshness", h.GetFreshness)

	return r
}

// DefaultReportsServiceName is the OTel service name the reports reader reports
// when the caller does not override it (via OTEL_SERVICE_NAME).
const DefaultReportsServiceName = "workforce-reports"
