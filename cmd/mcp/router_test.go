package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inboundmcp "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
)

// newTestRouter builds the router exactly as run() does: the real MCP server
// (in-memory deps are fine — no tool is invoked here).
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	server := inboundmcp.NewServer(inboundmcp.Deps{})
	return newRouter(inboundmcp.Handler(server), "workforce-management-mcp")
}

func TestRouter_HealthzServesOK(t *testing.T) {
	h := newTestRouter(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Fatalf("GET /healthz body = %q, want {\"status\":\"ok\"}", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET /healthz Content-Type = %q, want application/json", ct)
	}
}

func TestRouter_MCPPathsReachStreamableHandler(t *testing.T) {
	h := newTestRouter(t)
	for _, path := range []string{"/", "/mcp"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// initialize is the first Streamable HTTP request a client sends;
			// a JSON-RPC reply proves the request reached the SDK handler at
			// this mount point.
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("POST %s: got %d, want the MCP handler to answer", path, rec.Code)
			}
			raw, _ := io.ReadAll(rec.Body)
			if !strings.Contains(string(raw), `"jsonrpc"`) {
				t.Fatalf("POST %s: body %q does not look like a JSON-RPC reply", path, string(raw))
			}
		})
	}
}
