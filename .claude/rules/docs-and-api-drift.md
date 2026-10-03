---
paths:
  - "docs/**"
  - "apis/**"
  - "README.md"
---

# Docs site and API drift

- Docs site (Docusaurus) generates the REST reference from
  `apis/openapi.yaml`:
  `cd docs && npm ci && npm run gen-api-docs && npm run build`.
- If `apis/openapi.yaml` changed, regenerate the reference pages:
  `cd docs && npm run gen-api-docs` and commit the result (CI's
  `docs-api-drift` job fails on any diff) — see `docs/package.json`'s
  `gen-api-docs` script. Generated output lives in `docs/docs/api-reference/rest`.
- `apis/asyncapi.yaml` has no generated pages today; its narrative
  counterpart is `docs/docs/ecosystem/integration.md` — update both together.
- README.md must stay current: run steps, endpoints w/ curl examples, layering
  note, and the "stops at the path boundary" rationale if touched.
