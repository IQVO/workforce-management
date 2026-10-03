---
paths:
  - "web/**"
---

# Frontend micro-frontend remote (`web/`)

This repo also owns `web/`: `workforce-mfe`, a Vite + React **Module
Federation remote** consumed by the separate `warehouse-console` shell repo
(ADR-0011, fleet-wide MFE console architecture). It is a plain browser
client of this service's own REST API (staffing-gap-by-path dashboard) —
nothing in `web/` talks to any other bounded context, and nothing in
`internal/` knows `web/` exists.

- Has its own `package.json`, build, and dev server (`:5185`).
- Does **not** participate in this repo's Go quality gate and is **not**
  part of the Go module — `make check`/`check-all` never touch it.
- Requires `CORS_ALLOWED_ORIGINS` to include its origin (default already
  does, see rules/integrations.md's CORS section).
