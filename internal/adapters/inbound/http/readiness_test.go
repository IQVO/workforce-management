package http_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
)

func TestReadyz_ZeroValueIsReady(t *testing.T) {
	h := &inbound.Handler{}
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	inbound.NewRouter(h, nil, "").ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a zero-value Readiness", w.Code)
	}
}

func TestReadyz_ReflectsSetNotReady(t *testing.T) {
	readiness := &inbound.Readiness{}
	h := &inbound.Handler{Readiness: readiness}
	router := inbound.NewRouter(h, nil, "")

	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status before SetNotReady = %d, want 200", w.Code)
	}

	readiness.SetNotReady()

	r2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, r2)
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("status after SetNotReady = %d, want 503", w2.Code)
	}
}

func TestReadiness_NilReceiverIsReady(t *testing.T) {
	var readiness *inbound.Readiness
	if !readiness.Ready() {
		t.Fatal("a nil *Readiness must report ready (Handler zero value semantics)")
	}
	// Must not panic.
	readiness.SetNotReady()
	if !readiness.Ready() {
		t.Fatal("a nil *Readiness must stay ready even after SetNotReady (documented no-op)")
	}
}
