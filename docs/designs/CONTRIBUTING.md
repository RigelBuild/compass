# Authoring design records in Compass

Compass is a **public** repository. Design records under `docs/designs/` are
published verbatim to the engineering-docs site, so a record must carry no dead
private-repo artifacts and must never expose a colleague's security analysis in
a form they did not intend to publish.

These five rules are the standing policy for every design record authored here
going forward. Rules 1-4 match the sanitization the one-shot migration applied
to the records imported from the private `sealed` repo (new records should be
written this way from the start so they need no migration); rule 5 governs what
happens to a record's inbound links when another record is deleted.

## 1. Tracker IDs stay as plain-text provenance

Keep `SEA-####` issue references as **bare plain text** — they are load-bearing
provenance (records cite each other through them). Do **not** wrap them in a
`linear.app` link, in either form:

- inline: `[RIG-1234](https://linear.app/…)` — write `RIG-1234`
- reference-definition: a trailing `[RIG-1234]: https://linear.app/…` line —
  drop the definition; keep the bare `RIG-1234` in the prose

A public reader sees an opaque internal ticket ID, which is honest and
harmless. A dead `linear.app` URL is worse than no URL.

## 2. No `oss/compass/` path prefixes

The private repo vendored Compass under `oss/compass/`. This repo **is** that
tree, without the prefix. Cite paths relative to the repo root:

- `oss/compass/go/internal/runtime/image.go` → `go/internal/runtime/image.go`
- `oss/compass/apps/ui/src/stub-data.ts` → `apps/ui/src/stub-data.ts`

## 3. De-link private records to prose

Records whose subject is seal-the-product (`seal-*.md`) are **not** published
here. When a Compass record references one, keep the reference as prose and drop
the link wrapper:

- `[the seal restructure record](seal-restructure.md)` →
  `the seal restructure record`

Cross-product references that name a Compass component (e.g. Warden) or public
OSS (e.g. Cotal, Apache-2.0) are kept as written.

## 4. Never edit another author's security sections

Threat-model, security-boundary, and egress sections are kept **verbatim**. Do
not restructure, summarize, or "sanitize" a section under a heading matching
threat-model / security / egress (including security-boundary) that you did not
author. If a section needs a change, raise it with its author rather than
editing it in-place.

## 5. Freeze protects decision content, not links — fix inbound links on deletion

The freeze convention (a later change adds a new record, never rewrites one)
protects a record's **decision content**. It does **not** freeze a
record's links to *other* records: a link whose target no longer exists is rot,
not content. So when a record is deleted or superseded, re-point or de-link its
inbound references **from surviving records in the same PR** — even from a frozen
`Active`/`Historical` record — pointing them at the successor record, the new
home for the carried-over rationale, or the decision ledger, or dropping the link
wrapper to prose (rule 3) when nothing replaces the target. A dead `](path)`
link degrades on the docsite to a bare GitHub blob URL into a deleted path, which
is exactly the "published record cites something that no longer exists" artifact
rule 1 forbids for tracker IDs. This is a link-integrity edit, not a
decision-content rewrite, so it is not a freeze violation; leave the record's
decisions, prose, and security sections (rule 4) untouched.

The same link-integrity requirement covers the ledger's own `Record` cell: the
gate resolves every row's `Record` link regardless of the row's status, so a
`Retired` or `Superseded by` row whose record is deleted still gets its `Record`
cell re-pointed (to the successor, the new home for the rationale, or the
ledger's own record) in the same PR — a retracted decision's link is held to the
same standard as a live one's.

## 6. The bucket taxonomy

Design records live under one of six top-level buckets in `docs/designs/`. The
bucket names the record's concern; pick the one that fits and place the record
there.

- `ui/` — the product's visible surfaces: shell, board, sidebar, keyboard,
  rendering, the design system, and the native/desktop shell family.
- `agent/` — agent behavior and lifecycle: config, comms, session, spawn,
  transport, prompts, the ask contract, the agent container.
- `server/` — server-side domain model and write paths: the ownership layer,
  forge, threading/issue model, notification and mention delivery.
- `meta/` — process and method records governing the corpus and the product's
  engineering posture: architecture lineage, the design ledger itself, test
  strategy, scope gates.
- `infra/` — runtime and CI/testing infrastructure, sub-grouped as
  `infra/runtime/` and `infra/ci/`.
- `repo/` — repository tooling and the dependency/library decisions that govern
  the build (Effect adoption, Renovate, proto drop, the eng-docs site).

### Layout

The layout rule is `<bucket>/[<subgroup>/]<name>/design.md`: a record is a
`<name>/design.md` directory (which may own supporting `.md` files beside its
`design.md`), optionally nested one subgroup deep under a bucket (as `infra/`
is). A flat `<name>.md` is allowed **only at a bucket root** — a flat `.md`
nested inside a subgroup falls out of gate governance, so a sub-grouped record
must use the `<name>/design.md` directory layout. Add a subgroup when a bucket
outgrows flat scanning; until then records sit directly under their bucket.

### The design-ledger-gate governs every bucket

`tools/design-ledger-gate` scans every governed bucket (the six taxonomy buckets
above): every record's `Status:`
header is checked for presence and grammar, and a PR that touches a governed
record must either touch the ledger (`docs/designs/DECISIONS.md`) or declare a
`Ledger-impact:` line in its description. The `DECISIONS.md` ledger rows stay
scoped to **product decisions** — the gate governing a record is independent of
whether that record carries a ledger row.

### Moving a record is not a freeze violation

A move changes a record's path and its link graph, not one word of its
decisions — so it is a link-integrity edit under rule 5, not a decision-content
rewrite, and the freeze does not forbid it. When a record moves, re-point every
inbound reference to it (other records' links, the ledger's `Record` cell, and
code/config/doc citations) **in the same PR**, exactly as rule 5 requires on
deletion — a move leaves the same dangling-link rot a deletion would. Two narrow
metadata edits ride the same standard and are likewise not freeze violations:
normalizing a newly-governed record's `Status:` header to the gate grammar, and
a one-line correction of a record's stale self-described location.
