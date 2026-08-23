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

- inline: `[SEA-1234](https://linear.app/…)` — write `SEA-1234`
- reference-definition: a trailing `[SEA-1234]: https://linear.app/…` line —
  drop the definition; keep the bare `SEA-1234` in the prose

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

The freeze convention (`AGENTS.md`: a later change adds a new record, never
rewrites one) protects a record's **decision content**. It does **not** freeze a
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
