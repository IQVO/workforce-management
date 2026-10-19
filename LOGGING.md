# Logging

The Workforce Management service uses Go's standard library `log/slog`
package for structured logging.

## Why `log/slog`

- **Zero new dependency.** `log/slog` shipped in Go 1.21 and this module
  targets a much newer Go toolchain, so structured logging is available for
  free — no `zerolog`/`zap` import to vet, pin, or patch.
- **Go ecosystem standard.** `slog` is the community-converged structured
  logging API; libraries and tooling increasingly accept or emit
  `*slog.Logger` / `slog.Handler` directly.
- **Composable handlers.** A `slog.Handler` is an interface, so trace
  correlation is a wrapper (`telemetry.TraceHandler`) around the JSON
  handler rather than a change to any call site — see
  [Trace correlation](#trace-correlation) below.
- `zerolog` and `zap` were considered and rejected: this is a
  moderate-throughput CRUD/event-driven service, not a hot-path log
  producer, so their extra allocation-avoidance machinery is unneeded
  dependency weight here.

## Configuration

| Env var     | Default | Values                          |
|-------------|---------|----------------------------------|
| `LOG_LEVEL` | `info`  | `debug`, `info`, `warn`, `error` (case-insensitive) |

Set `LOG_LEVEL=debug` for verbose local development; leave unset (or `info`)
in production.

## Output format

Logs are emitted as single-line JSON to stdout via
`slog.NewJSONHandler(os.Stdout, ...)`, e.g.:

```json
{"time":"2026-08-23T12:00:00Z","level":"INFO","msg":"http server listening","addr":":8080"}
{"time":"2026-08-23T12:00:01Z","level":"INFO","msg":"http request","method":"GET","path":"/healthz","route":"/healthz","status":200,"duration_ms":0,"bytes":16,"request_id":"abc123","trace_id":"60b0a70a6ebad03fb2e4b4d05246c4ba","span_id":"3c745e4adb43be47"}
{"time":"2026-08-23T12:00:02Z","level":"INFO","msg":"domain event published","event_name":"AssociateShiftStarted","occurred_at":"2026-08-23T12:00:02Z"}
```

JSON-to-stdout is the standard shape for container/Kubernetes log
collection (Fluent Bit, Vector, CloudWatch, etc.) without any extra
formatting step.

## What gets logged

- **HTTP requests** — a chi middleware (`internal/adapters/inbound/http/logging.go`)
  logs method, raw `path`, the matched `route` pattern, status,
  `duration_ms`, response `bytes`, and the chi `request_id` for every
  request. Responses with a `5xx` status are logged at `Error`; everything
  else at `Info`. Logging both `path` and `route` gives log aggregation the
  same low-cardinality grouping key the trace spans use, without losing the
  concrete URL.
- **Domain events** — `events.LogPublisher` is a test double only (ADR-0038:
  no `cmd/**` binary selects it); production events go through the outbox to
  Kafka. A `nil` `*slog.Logger` disables event
  logging while still buffering events in memory (used by tests).
- **Startup/shutdown and config errors** — missing required env vars,
  invalid env var values, and the Kafka publisher's configuration/close
  errors are logged via `slog` before the process exits.

## Trace correlation

`cmd/workforce` wraps the JSON handler in
`telemetry.TraceHandler` (`internal/adapters/outbound/telemetry/slogotel.go`)
before calling `slog.SetDefault`. On every record logged with a context that
carries a valid span context, the wrapper appends `trace_id` and `span_id`;
records logged without an active span are passed through unchanged, so
nothing gains empty correlation fields.

This only fires for the `*Context` logger methods (`InfoContext`,
`ErrorContext`, …) — a plain `logger.Info(...)` has no context to read a span
from. The HTTP request-logging middleware uses the `*Context` variants, and
the otelchi middleware runs ahead of it so a span already exists by the time
the line is written.

See the README's Observability section for what the resulting `trace_id`
links to.
