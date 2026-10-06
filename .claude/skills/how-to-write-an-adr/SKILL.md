---
name: how-to-write-an-adr
description: Write an Architecture Decision Record in this repo's numbering and format, including companion ADRs for cross-repo changes. Use when a design decision should be recorded or a change contradicts an existing ADR.
---

# How to write an ADR

Use when a change is architecturally significant — a new bounded-context
integration, a reversal of a prior decision, a cross-repo contract change,
or anything a future reader would otherwise have to reverse-engineer from
the diff. Not every change needs one: a bug fix or a routine feature
addition inside an already-decided architecture doesn't.

## Numbering and location

`docs/docs/adr/NNNN-kebab-case-title.md`, four-digit zero-padded,
sequential — check the highest existing number
(`git ls-tree --name-only origin/develop -- docs/docs/adr/` and pick the
next integer, never reuse or guess). As of this writing the highest is
`0020-idle-share-staffing-signal.md`, so the next ADR is `0021`.

**Two other files need a matching entry in the SAME PR, or the new ADR
is unreachable from the site's navigation even though the page itself
builds fine:**

1. `docs/docs/adr/index.md` maintains its own "The records" table by
   hand — add a new row, matching the exact `| [NNNN](...) | Title |
   Status |` format the other twenty rows use.
2. `docs/sidebars.ts`'s `Architecture Decision Records` category is an
   EXPLICIT array of doc ids (`'adr/0001-hexagonal-ports-and-adapters'`,
   ... `'adr/0020-idle-share-staffing-signal'`) — it is NOT derived
   automatically from the files on disk or from `id`/frontmatter
   ordering. A new ADR file with correct frontmatter still will not
   appear in the left-nav sidebar at all until its id is appended to
   this array. This is easy to miss because `npm run build` does not
   fail when a doc exists but isn't in any sidebar — Docusaurus just
   renders it as an orphan page reachable only by direct link.

## Frontmatter (Docusaurus needs all fields)

```yaml
---
id: NNNN-kebab-case-title
slug: /adr/NNNN-kebab-case-title
title: "NN. Title (a short noun phrase, matching the heading)"
sidebar_label: "NN. Short label for the nav sidebar"
description: "One or two sentences — this shows up in search and link
  previews, so make it stand alone without the rest of the doc."
---
```

`id`/`slug` are the full kebab-case filename (minus `.md`); `title`/
`sidebar_label` repeat the number as plain text (`"18. ..."`, not `#18`).
Note this repo's real ADRs (e.g. `0018-remove-fleet-rest-identity.md`,
`0016-transactional-outbox.md`) do NOT set `sidebar_position` in
frontmatter — order within the category comes entirely from this
file's position in `docs/sidebars.ts`'s explicit array (see below), not
from a frontmatter field. Don't add `sidebar_position` speculatively if
this repo's existing ADRs don't use it; it has no effect here.

## Format: Michael Nygard's template

```markdown
# NNNN. Title (a short noun phrase)

## Status
Accepted | Proposed | Deprecated | Superseded by [ADR-XXXX](./xxxx-slug.md)

## Context
The forces at play — technical, business, constraints — that make this
decision necessary. Write in the past tense, as if explaining to someone
who wasn't there. State the alternatives seriously considered, not just
the one chosen; a reader six months from now needs to know a simpler
option was weighed and rejected, not assume nobody thought of it.

## Decision
What was actually decided, stated as an active, present-tense
declaration ("we will..."). Be specific about the mechanism, not just
the intent.

## Consequences
What becomes easier, what becomes harder. Split into `### Easier` /
`### Harder` subsections — every real ADR in this repo from 0012 onward
uses this split rather than one flat list; keep it for consistency.
```

`docs/docs/adr/0016-transactional-outbox.md` is this repo's most
thorough model: it adds an `## Alternatives considered` section (four
rejected designs, each with a one-line reason) and a `## Verification`
section listing the actual unit/integration test names and `make`
targets that prove the decision was implemented correctly — copy that
structure for anything nontrivial rather than stopping at Context/
Decision/Consequences.

## Superseding an earlier ADR

Don't edit the old ADR's Decision section. Add a `Supersedes` reference
in the new ADR's Status line and leave the old one otherwise untouched —
see the real pair in this repo:

- `0017-adopt-fleet-rest-identity.md` — the original decision, left in
  place as the historical record, never edited.
- `0018-remove-fleet-rest-identity.md` — `## Status` reads `Accepted —
  implemented in the same change that introduced this record. Supersedes
  [ADR-0017](./0017-adopt-fleet-rest-identity.md).`, and its `index.md`
  row shows ADR-0017 itself as `Superseded by [0018](./0018-remove-fleet-rest-identity.md)`.

Both rows in `docs/docs/adr/index.md`'s table need updating in the same
PR: the new ADR's row, AND the old ADR's `Status` column changed from
`Accepted` to `Superseded by [NNNN](...)`.

## Cross-repo decisions: use a companion ADR, not one repo's private opinion

When a decision genuinely spans two bounded-context repos, write ONE ADR
per repo, each referencing the other explicitly. This repo's own
`0012-measured-rate-feed-for-propose-path-plan.md` is the workforce side
of exactly this pattern: it documents `workforce-management` as
`labor-performance`'s **Customer** in a Customer/Supplier relationship
(explicitly naming the DDD relationship type), references
`labor-performance`'s own ADR-0006 for the supplying side of the
contract, and calls out the vocabulary-mismatch translation
(`PathId` lowercase open string vs. `TaskType` uppercase closed enum)
that only this side of the integration needs to resolve. Don't write the
decision once in one repo and expect the other repo's readers to find
it; each bounded context's docs site is read independently.

## After writing: regenerate and verify the docs build

```bash
cd docs
npm ci
npm run build   # onBrokenLinks / onBrokenAnchors are both 'throw' — this
                 # WILL fail if the frontmatter/slug is wrong or a
                 # cross-reference link is broken
```

A broken ADR link or malformed frontmatter fails the build with a clear
Docusaurus error, not a silent 404 — always run this locally before
opening the PR. This repo's CI does NOT gate the docs build itself on
every PR (only `docs-api-drift`, which checks the REST-reference tree
generated from `apis/openapi.yaml`, and the separate `Docs` GitHub Pages
deploy workflow which runs on push to `main` only) — a broken ADR
cross-reference can merge to `develop` without CI catching it, so this
local `npm run build` step is the only real gate until release. If the
change also touched an existing ADR's status line (a supersede), run
`npm run build` from a clean `docs/` state — see the fleet skill's
`docusaurus-plugin-openapi-docs` note if the build fails with a sidebar
error unrelated to your actual edit; that is a different, known
Docusaurus limitation, not evidence the ADR itself is wrong.
