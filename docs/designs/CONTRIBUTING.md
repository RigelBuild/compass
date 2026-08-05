# Authoring design records in Compass

Compass is a **public** repository. Design records under `docs/designs/` are
published verbatim to the engineering-docs site, so a record must carry no dead
private-repo artifacts and must never expose a colleague's security analysis in
a form they did not intend to publish.

These four rules are the standing policy for every design record authored here
going forward. They match the sanitization the one-shot migration
(`tools/docs-migrate/`, SEA-1766) applied to the records imported from the
private `sealed` repo; new records should be written this way from the start so
they need no migration.

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
