package http

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIdempotencyKeyWire_TrailingOWSIsStrippedBeforeTheMiddleware pins the
// HTTP/1.1 fact ADR 0042 rests on: RFC 9112 frames a field line as
// `field-name ":" OWS field-value OWS` and RFC 9110 section 5.5 makes the
// recipient drop that OWS before it evaluates the value, so a key sent as
// "8G'S<TAB>" reaches RequireIdempotencyKey as "8G'S". The spec's wire pattern
// therefore allows trailing SP/HTAB after a valid key. If Go ever stops
// stripping, this test fails and the pattern in apis/openapi.yaml must be
// revisited together with validIdempotencyKey.
func TestIdempotencyKeyWire_TrailingOWSIsStrippedBeforeTheMiddleware(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(IdempotencyKeyHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	for wire, want := range map[string]string{
		"8G'S\t":  "8G'S",
		"abc ":    "abc",
		"a b \t ": "a b",
	} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		raw := "POST /x HTTP/1.1\r\nHost: test\r\nContent-Length: 0\r\nConnection: close\r\n" +
			IdempotencyKeyHeader + ": " + wire + "\r\n\r\n"
		if _, err := conn.Write([]byte(raw)); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
		if got := <-seen; got != want {
			t.Fatalf("wire value %q reached the handler as %q, want %q", wire, got, want)
		}
		if !validIdempotencyKey(want) {
			t.Fatalf("the stripped value %q must be a valid key", want)
		}
	}
}
