# Compass design decisions

Current Compass design truth: every ratified, citable design decision is one
file, `<area>/DL-NNN.md`, holding a stable id, a one-line statement, a
live/superseded status with provenance, and a link to the frozen record holding
the rationale. `<area>` is the top-level directory of that record under
`docs/designs/` (`agent`, `server`, `ui`, ...).

**Absence of a decision is not evidence of no ruling.** The decisions cover
named, citable rulings (a record's numbered Decision, a Global Constraint, or a
§-level ruling another record cites) plus every known overturn, not inline
micro-choices. For anything not here, the linked frozen records remain
authoritative.

The read-first index is generated from these files at docs build time and is
never committed: [Compass decisions](https://compass-eng-docs.pages.dev/designs/decisions/).
The rules that keep the decisions honest live in the
[design-ledger record](../meta/compass-design-ledger/design.md) and
[CONTRIBUTING](../CONTRIBUTING.md); the `design-ledger-gate` CI check enforces
the mechanical half.

## Conventions

- **ID**: `DL-<zero-padded int>`, unique within compass, append-only, never
  reused. The file name is the id. Claim new ids with `bun tools/dl-claim`.
- **Status**: exactly `Active (<who>, YYYY-MM-DD)`, `Retired (<who>,
  YYYY-MM-DD)`, or `Superseded by DL-<n> (<who>, YYYY-MM-DD)`. Use `Superseded
  by DL-<n>` when a later decision replaces this one; use `Retired` when the
  decision is abandoned with no replacement (the thing it decided was scrapped,
  not re-decided).
- **Decision**: an immutable one-line paraphrase of the ruling. A new ruling is
  a new file plus a `Superseded`/`Retired` flip on the old one, never an
  in-place reword.
- **Record**: a link into the frozen record, relative to the decision file; an
  `#anchor` is required for links into large records.

A decision file is front matter only, with exactly these keys in this order:

```markdown
---
id: DL-NNN
decision: "One-line ruling."
status: "Active (<who>, YYYY-MM-DD)"
record: ../../<area>/<record>/design.md#anchor
---
```
