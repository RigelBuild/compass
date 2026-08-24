# Design: reorganize `docs/designs/`

Status: Draft
Tracking: RIG-2577

Matt's trigger, verbatim: "why do we have a platform dir in compass? i think we
need to reorganize the design docs."

## Problem / Intent

`docs/designs/` has three buckets plus the policy file: `product/` (65 records:
58 `<name>/design.md` directories + 7 flat `<name>.md`, plus `DECISIONS.md`),
`platform/` (17 records: 12 directories + 5 flat), and `repo/` (2, including
this record). Two problems:

1. **`platform/` is a catch-all in a product repo.** It lumps four distinct
   concerns — agent/runner-execution *runtime* (`compass-elastic-session-runtime/`,
   `compass-runner-gateway-error-sentinels/`, `compass-runner-concurrent-dispatch/`,
   `compass-runner-arbitrary-uid/`), *CI/testing infra* (`compass-dogfood-e2e/`,
   `compass-dogfood-loop/`, `compass-dogfood-e2e-steer-deliver-seam/`,
   `compass-pr-validation/`, `compass-agent-image-publish.md`,
   `compass-local-dev/`), *dependency/library tooling*
   (`compass-renovate-migration.md`, `compass-effect-adoption-decision.md`,
   `compass-agent-effect-adoption/`, `compass-agent-effect-otel/`,
   `compass-drop-proto.md`), and *product behavior*
   (`compass-initial-prompt-removal.md`, `compass-forks-reversal/`). The name
   also collides with the fleet-wide sense of "platform" (the shared
   infrastructure layer), making it doubly ambiguous in a product repo. The
   bucket is an accident of migration: the eng-docs migration record itself asserted
   "`platform/` — a sealed-monorepo domain — does not exist in compass"
   (`docs/designs/repo/compass-eng-docs/design.md:386-387`), yet 17 records now
   live there.

2. **`product/` is a 65-record flat pile.** It outgrew scanning, and it also
   holds non-UI records that are arguably infra (`compass-agent-runner-transport/`,
   `compass-agent-transport-consolidation/`, `compass-server-ownership-layer/`,
   `compass-forge-integration-testing/`, `compass-forge-poll-driver/`,
   `compass-forge-write-path/`, `compass-agent-primary-lifecycle/`,
   `compass-spawn-control/`, `compass-agent-spawn-despawn/`).

This record designs the reorg: the new taxonomy (frozen by Matt's 2026-08-23
ruling, §Resolved decisions), the required design-ledger-gate expansion that the
dissolution of `product/` forces, and the migration strategy that keeps the
design-ledger-gate and the eng-docs build green throughout, plus the doc updates
that make the taxonomy a stated contract instead of an accident.

## Recon findings (the coupling surface)

Every claim below was verified against merged main tip `55a72453` and
re-verified this session against the current tip `1e8c0ab9`. Drift found and
folded in: the ledger now carries 214 `DL-` rows (the duplicated `DL-239` row
was renumbered `DL-240`), one new flat product record landed
(`compass-mention-offline-redelivery-pre-settle-closure.md`, carrying the
`DL-240` Record link — so 65 product records: 58 directories + 7 flat), and the
`docs/designs/product` code/config citation surface is now 40 files (67 hits).

### 1. design-ledger-gate governs `product/` only, by path constant

`tools/design-ledger-gate/index.ts:44-47`:

```ts
/** The product design-corpus directory the gate governs. */
export const PRODUCT_DIR = "docs/designs/product";
/** The canonical ledger, parsed as the decision table (never as a record). */
export const DECISIONS_PATH = `${PRODUCT_DIR}/DECISIONS.md`;
```

Membership is `touchesRecord` (`index.ts:208-215`):

```ts
export function touchesRecord(file: string): boolean {
    if (!file.startsWith(`${PRODUCT_DIR}/`)) return false;
    if (file === DECISIONS_PATH) return false;
    const rest = file.slice(PRODUCT_DIR.length + 1);
    if (rest.endsWith("/design.md")) return true; // <name>/design.md layout
    if (rest.endsWith(".md") && !rest.includes("/")) return true; // <name>.md layout
    return false;
}
```

Two load-bearing consequences:

- **A nested `product/<group>/<name>/design.md` stays governed** — line 212
  matches any depth (`rest.endsWith("/design.md")`), and the record-file scan is
  a recursive glob (`index.ts:684`, `new Bun.Glob(`${PRODUCT_DIR}/**/*.md`)`
  filtered through `touchesRecord`). So sub-grouping *under a governed root*
  needs no membership-logic change for directory-layout records — only the root
  set itself is hardcoded.
- **A flat `<root>/<group>/<name>.md` falls OUT of governance** — line 213
  rejects any flat `.md` with a `/` in `rest`. The 7 flat product records
  (`compass-tauri-shell.md`, `compass-agent-container-runtime.md`,
  `compass-mention-offline-redelivery.md`,
  `compass-mention-offline-redelivery-pre-settle-closure.md`,
  `compass-slack-thread-rendering.md`, `compass-session-trace-diff.md`,
  `compass-ask-typed-derivation.md`) must either sit at a governed root or
  convert to `<name>/design.md` layout when sub-grouped.

Ledger `Record` cells resolve **product-relative**: `readRecord` joins
`` `${r}/${PRODUCT_DIR}/${productRelPath}` `` (`index.ts:692-693`). So any move
of a ledger-linked record shifts its `Record`-cell link path, and the cell must
be re-pointed **in the same PR** or the SNAPSHOT leg goes red on an
unresolvable Record link.

Record-level supersession pointers are **record-relative**
(`index.ts:218-222`: "That header value is written RECORD-relative … unlike a
ledger Record cell which is product-relative") and reject escapes
(`index.ts:235-237`: "A pointer that climbs out of PRODUCT_DIR (leading `..`)
can't be a product record"). Exactly one such pointer exists in the corpus
(`docs/designs/product/compass-tauri-shell.md:3`,
`Status: Superseded by compass-native-app/design.md`) — if either endpoint moves
relative to the other, that pointer needs the same-PR fix, and under the current
single-root resolver neither endpoint may leave `product/` while the pointer
exists.

**What breaks on a move INTO a new subdir inside a governed root:** every
`DECISIONS.md` `Record` cell pointing at the moved record (`grep` this session:
214 `DL-` rows, every one carrying a Record link into `product/`), plus
inter-record relative links, plus the one supersession pointer. All fixable in
the same PR; the gate evaluates a tree snapshot, so an atomic move+re-point PR
stays green.

**What breaks on a move OUT of the governed root set:** the record leaves
governance entirely — `touchesRecord` returns false, `listRecordFiles` never
scans it, its `Status:` header stops being checked, and the touch-coupling leg
(a PR touching a product record must touch `DECISIONS.md` or declare
`Ledger-impact:`, `index.ts:20-23`) stops firing for it. Additionally its
`Record` cells can no longer be expressed as sane root-relative links. Which
records are actually ledger-governed today: **57 of the 64** pre-drift product
records carry at least one `Record`-cell link (verified by per-record grep at
`55a72453`; the 65th record added since carries one, `DL-240`) — the 7 with
zero rows are `compass-session-trace-diff.md`, `compass-slack-thread-rendering.md`,
`compass-dev-boot-gate/`, `compass-multi-window/`, `compass-native-client-mode/`,
`compass-threading-ui/`, `compass-ui-fixture-boot/` — while 4 that look
peripheral DO have rows (`compass-tauri-shell.md` 1, `compass-ask-typed-derivation.md`
1, `compass-mention-offline-redelivery.md` 1, `compass-agent-container-runtime.md` 3).
The "arguably infra" product records the trigger names are heavily
ledger-linked (`compass-spawn-control/` 9 rows, `compass-server-ownership-layer/`
8, `compass-forge-write-path/` 7, `compass-agent-spawn-despawn/` 4,
`compass-forge-poll-driver/` 3, `compass-agent-runner-transport/` 2,
`compass-agent-transport-consolidation/` 1, `compass-forge-integration-testing/`
1, `compass-agent-primary-lifecycle/` 1) — under the ruled taxonomy they stay
governed because the gate's root set expands with them (§Approach 3).

Precedent that `platform/` is deliberately ungoverned:
`docs/designs/platform/compass-agent-effect-otel/design.md:552-558` ruled "the
`DECISIONS.md` ledger lives under `docs/designs/product/` and the
design-ledger-gate CI check governs product records ONLY
(`tools/design-ledger-gate/index.ts:45` …); its touch-coupling leg does not fire
for a `platform/` record". Matt's OQ-2 ruling supersedes that boundary
(§Resolved decisions).

### 2. eng-docs: the on-disk layout IS the URL

`apps/eng-docs/scripts/gather.ts:28-31`:

```ts
/**
 * The `docs/` subtrees that map to a nav section by their directory name. Each
 * renders under its own section verbatim (the on-disk layout is the taxonomy).
 */
const DOMAINS = ["designs", "specs", "architecture"] as const;
```

`classify` (`gather.ts:91-98`) keys only on the first segment under `docs/`
(`sourcePath.match(/^docs\/([^/]+)\/(.+)$/)`, then `destRel` is
`` `${domain}/${rest}` ``),
`destRelPath` strips `docs/` (`gather.ts:213-215`), and `routeSlug`
(`gather.ts:322-329`) drops `.md` and github-slugs each segment. Asserted
contract: `gather.test.ts:583-584` — `routeSlug("designs/product/compass-0.6/design.md")`
→ `"/designs/product/compass-06/design"`; `deploy.test.ts:226-232` —
`docs/designs/repo/foo.md` → route `/designs/repo/foo`.

So: **any rename under `docs/designs/` is transparent to the site code** (no
gather/classify change; the sidebar is generated, `gather.ts:13-14`), but **every
moved record's URL renames 1:1 with its path**. Example:
`docs/designs/platform/compass-dogfood-e2e/design.md` routes to
`/designs/platform/compass-dogfood-e2e/design` today and routes to
`/designs/infra/ci/compass-dogfood-e2e/design` after the ruled move.

**The site has no redirects**: `grep redirect apps/eng-docs/**` returned zero
matches this session, and `astro.config.mjs` configures only `sidebar` — an old
URL 404s after a move. No committed artifact hardcodes deployed `/designs/…`
site URLs outside the eng-docs tests themselves (repo-wide grep this session);
the PR preview comment builds its links live from the changed set
(`deploy.ts:146-150`). URL renames are therefore a real but bounded cost:
in-flight bookmarks and any external links rot.

**Dangling in-repo links degrade, not 404**: `rewriteLinks` routes a relative
`.md` link to a site route only when the target is in the gathered set; anything
else becomes a GitHub blob URL (`gather.ts:223-230` — "a link to an ungathered
file degrades to its visible source on GitHub instead of a silent 404"). A blob
URL into a moved-away path is exactly the published-rot artifact
`docs/designs/CONTRIBUTING.md` rule 5 forbids, so every in-repo reference to a
moved path must be updated in the move PR.

### 3. The reference surface outside the corpus

Repo-wide grep this session for `docs/designs/platform` outside `docs/designs/`
found comment/doc citations in: `.github/workflows/publish-agent-image.yml:40`,
`.github/workflows/renovate.yml:7` ("Design:
docs/designs/platform/compass-renovate-migration.md"), `buf.gen.yaml:9`,
`devenv.nix:167,208`, `docs/architecture/build-and-ci.md:128`,
`docs/specs/product/compass.md`, `forks/README.md`, `go/moon.yml`,
`proto/moon.yml`, several `go/internal/runner*` and
`packages/compass-agent/src/transport/*` file comments, and gate/test fixtures
(`tools/design-ledger-gate/index.test.ts`, `tools/orion-ref-gate/index.test.ts`
— fixture strings, not live paths). Inside the corpus, 41 more `docs/designs/platform`
prose citations. Two cited paths are **already dead** (sealed-repo records that
never migrated, pre-existing rot, out of scope but noted): `buf.gen.yaml:9`
(`docs/designs/platform/go-toolchain-default.md`) and `devenv.nix:208`
(`docs/designs/platform/ci-toolchain-shared-defs.md`).

`docs/designs/product` is referenced from 40 code/config files (67 hits,
re-verified at `1e8c0ab9` via `git grep`) including `.moon/workspace.yml:57`,
`apps/ui/.stylelintrc.cjs:20`, and UI test suites
(`apps/ui/src/design-citations.test.ts:4,76` hardcodes
`docs/designs/product/compass-architecture-lineage/design.md` in an assertion
message; `identity.test.ts:17`; `BadgeGlyph.test.tsx:14`). These are comments
and message strings — nothing *resolves* the paths at runtime except the gate —
but they are the grep-surface an executor must sweep per moved record.

### 4. The taxonomy is documented in two places, neither CONTRIBUTING

`docs/designs/CONTRIBUTING.md` states five authoring rules (rules 1-5,
`CONTRIBUTING.md:8-12`: "These five rules are the standing policy…") and
**never documents the bucket set** — grep for a domain list returns nothing. The
domain set IS documented in the fleet design skill
(`config/skills/design/SKILL.md:68-69`: "under `docs/designs/<domain>/`
(`<domain>` = `platform` / `tools` / `agents` / `product`)") — a four-bucket set
that matches neither this repo's actual tree (`tools/` and `agents/` don't
exist here; `repo/` does) nor the ruled set, so it needs an update. The
eng-docs record documents only that repo-tooling records live under
`docs/designs/repo/` (`compass-eng-docs/design.md:387-388`).

## Approach

Matt ruled the taxonomy fork on 2026-08-23 (§Resolved decisions). The end state,
the gate expansion it forces, and the migration strategy:

1. **The frozen end-state taxonomy.** Six top-level buckets; `product/` and
   `platform/` both dissolve as names:

   - `ui/` — the product's visible surfaces: shell, board, sidebar, keyboard,
     rendering, design system, and the native/desktop shell family.
   - `agent/` — agent behavior and lifecycle: config, comms, session, spawn,
     transport, prompts, the ask contract, the agent container.
   - `server/` — server-side domain model and write paths: ownership layer,
     forge, threading/issue model, notification and mention delivery.
   - `meta/` — process/method records governing the corpus and the product's
     engineering posture: architecture lineage, the design ledger itself, test
     strategy, scope gates.
   - `infra/` — runtime and CI/testing infrastructure, sub-grouped immediately
     as `infra/runtime/` and `infra/ci/`.
   - `repo/` — the existing bucket, absorbing the dependency/library tooling
     records (Matt's OQ-3: no `tooling/` bucket).

   The four product-derived buckets sit at the TOP level (not under a surviving
   `product/`) per Matt's rationale: "ultimately the product design docs are
   the most important ones" — they get the prominence. The layout rule
   generalizes: `<bucket>/[<subgroup>/]<name>/design.md`; a flat `<name>.md` is
   allowed at a bucket root only (it stays governed there, per the
   `touchesRecord` flat-file rule carried into the generalized gate). The
   initial migration lands product-derived records DIRECTLY under their bucket
   (one path segment changes; a 4-31-record bucket scans fine flat); further
   sub-grouping inside a bucket is permitted by the layout rule whenever a
   bucket outgrows scanning — only `infra/` sub-groups at migration time, as
   ruled.

2. **The full bucket→record assignment.** Grounded in the ledger's topic
   headings (`DECISIONS.md:37-321`: Strategy, Topology, Transport, Storage,
   Agent runtime & container, Comms & tools, Agent roles & prompts, UI shell,
   Threading & rendering, Ask contract, Desktop shell, Agent batteries, UX
   foundation, Bridge spawn control, First-turn delivery) and each record's
   own H1/intent (spot-verified this session). The executor treats this roster
   as the contract; per-record judgment calls are noted.

   - `ui/` (31): `compass-ade-shell/`, `compass-shell-routing/`,
     `compass-board-view/`, `compass-sidebar-pins/`,
     `compass-sidebar-pins-unreachable-amendment/`, `compass-dock-in-sidebar/`,
     `compass-keyboard-shortcuts-overlay/`, `compass-command-palette/`,
     `compass-keyboard-spine-app-root/`, `compass-bridge-keyboard-nav/`,
     `compass-bridge-reclothe/`, `compass-threading-ui/`,
     `compass-message-surface-rendering/`, `compass-ui-markdown-react10/`,
     `compass-ui-solid-v2/`, `compass-ui-query-layer/`,
     `compass-ui-fixture-boot/`, `compass-ux-foundation/`,
     `compass-ds-token-cutover/`, `compass-badge-clarity/`,
     `compass-dev-boot-gate/` (a UI smoke gate, per its H1),
     `compass-live-roster/` (the agent-tree UI surface),
     `compass-multi-window/`, `compass-native-app/`,
     `compass-native-client-mode/`, `compass-native-client-only/`,
     `compass-native-packaging/`, `compass-stack-cross-process-teardown/`
     (native-app stack), `compass-tauri-shell.md`,
     `compass-slack-thread-rendering.md`, `compass-session-trace-diff.md`.
   - `agent/` (21): `compass-agent-comms-tools/`,
     `compass-agent-config-delivery/`, `compass-agent-config-passthrough/`,
     `compass-agent-session-persistence/`, `compass-agent-primary-lifecycle/`,
     `compass-agent-runner-transport/`,
     `compass-agent-transport-consolidation/`, `compass-agent-spawn-despawn/`,
     `compass-agent-trees/`, `compass-spawn-control/`,
     `compass-manager-prompt/`, `compass-manager-comms-substrate/`,
     `compass-batteries-included/`, `compass-agent-container-runtime.md`,
     `compass-ask-comms-roundtrip/`, `compass-ask-in-channel/`,
     `compass-ask-typed-derivation.md`, `compass-first-turn-delivery/`
     (the agent-session start contract), `compass-system-sender-first-turn/`,
     plus the two former-platform product-behavior strays:
     `compass-initial-prompt-removal.md` (the agent-session start path — same
     seam as `compass-first-turn-delivery/`) and `compass-forks-reversal/`
     (its dominant product surface is the vendored agent harness `oh-my-pi`,
     `forks/README.md:12,26` — the grounded agent-vs-ui call the ruling
     delegated).
   - `server/` (11): `compass-server-ownership-layer/`,
     `compass-server-ownership-layer-amendment/`, `compass-forge-write-path/`,
     `compass-forge-poll-driver/`, `compass-forge-integration-testing/`,
     `compass-notification-delivery/` ("the server-side push into the agent's
     session", per its H1), `compass-mention-offline-redelivery.md`,
     `compass-mention-offline-redelivery-pre-settle-closure.md`,
     `compass-issue-model/`, `compass-zulip-threading-model/` (the threading
     domain model — server-side semantics, distinct from
     `compass-threading-ui/`), `compass-attribution-simplification/` (amends
     the issue model's DL-068).
   - `meta/` (4): `compass-architecture-lineage/`, `compass-design-ledger/`,
     `compass-test-strategy/`, `compass-tier3-scope-gate/`.
   - `infra/runtime/` (4): `compass-elastic-session-runtime/` (whole directory;
     file set re-enumerated at execution time — live lane, hot-lane check),
     `compass-runner-gateway-error-sentinels/`,
     `compass-runner-concurrent-dispatch/`, `compass-runner-arbitrary-uid/`.
   - `infra/ci/` (6): `compass-dogfood-e2e/`, `compass-dogfood-loop/`,
     `compass-dogfood-e2e-steer-deliver-seam/`, `compass-pr-validation/`,
     `compass-local-dev/`, `compass-agent-image-publish.md` (converts to
     `compass-agent-image-publish/design.md` — a flat `.md` inside a subgroup
     would fall out of governance, per the flat-file rule).
   - `repo/` fold (5 join the existing 2): `compass-renovate-migration.md`,
     `compass-effect-adoption-decision.md`, `compass-agent-effect-adoption/`,
     `compass-agent-effect-otel/`, `compass-drop-proto.md`.

   Total: 31 + 21 + 11 + 4 = 67 product-derived (65 product + 2 strays);
   4 + 6 = 10 infra; 5 into repo/ — all 17 platform records accounted for.

3. **The design-ledger-gate expands to a governed-roots list (Matt's OQ-2 b) —
   a required code change, not an option.** `product/` is the gate's sole root
   (`PRODUCT_DIR`, `index.ts:44-47`) and it ceases to exist, so the gate
   changes regardless. The concrete deltas in
   `tools/design-ledger-gate/index.ts`:

   - `PRODUCT_DIR` → `DESIGNS_ROOT = "docs/designs"` plus
     `GOVERNED_ROOTS: readonly string[]` — bucket names relative to
     `DESIGNS_ROOT` (the governed set below). `DECISIONS_PATH` →
     `` `${DESIGNS_ROOT}/DECISIONS.md` `` (the relocation, next point).
   - `touchesRecord` (`index.ts:208-215`) iterates `GOVERNED_ROOTS`: for the
     first root the file sits under, apply the existing two-layout test to
     `rest` (any-depth `/design.md`; flat `.md` only directly at the root).
     Semantics per root are byte-identical to today's per-`PRODUCT_DIR` logic.
   - The record-file scan (`index.ts:684`) globs `docs/designs/**/*.md`
     filtered through the generalized `touchesRecord` (the existing
     glob-then-filter shape, one level higher).
   - `readRecord` (`index.ts:692-693`) joins
     `` `${r}/${DESIGNS_ROOT}/${relPath}` `` — ledger `Record` cells become
     **designs-root-relative, i.e. bucket-qualified**
     (`agent/compass-spawn-control/design.md#…` instead of
     `compass-spawn-control/design.md#…`).
   - `resolveRecordRelative` (`index.ts:227-239`) resolves a record-relative
     supersession pointer against the record's designs-root-relative path and
     rejects a leading-`..` escape of `DESIGNS_ROOT` (was: of `PRODUCT_DIR`).
     Cross-bucket supersession pointers become expressible — which the
     RIG-2542-deferred native family needs (§Plan T8/T10).
   - `HISTORICAL_CHAIN` (`index.ts:66`, currently empty) keys become
     designs-root-relative — comment-only today.
   - Test fixtures follow (`index.test.ts` pins literal
     `docs/designs/product/...` paths by design, `index.test.ts:9-10` — they
     are re-pinned to the new literals, plus new cases: a second governed root,
     an ungoverned bucket, a cross-bucket supersession pointer, a flat `.md`
     inside a subgroup rejected).

4. **The governed set: every bucket — `GOVERNED_ROOTS = [ui, agent, server,
   meta, infra, repo]` (plus `product` transitionally, §6).** Matt's ruling
   named "a bucket list / all of docs/designs/"; this design implements the
   bucket-list mechanism and populates it with all buckets, so governance
   currently equals all of `docs/designs/` while adding a future bucket stays
   an explicit one-line gate edit. Grounding for all-buckets over
   only-the-four: (a) the governed/ungoverned seam is exactly what made
   `platform/` a policy island (the effect-otel ruling, §Recon 1) — reproducing
   it at `infra/`/`repo/` re-creates the problem this reorg exists to kill;
   (b) the drift is already visible — 6 of the 17 platform records carry
   Status headers outside the gate grammar (5 distinct malformed patterns:
   `Status: **Draft**` on two records, `Status: **decided**`,
   `Status: Ratified (Matt, 2026-08-05) …`,
   `Status: Decided (Matt, 2026-08-20)`, `Status: proposed. Owner: …`;
   verified per-file this session) and `compass-eng-docs/design.md` has no
   Status header at all — none of which any check catches today; (c) the
   marginal cost is small: Status normalization is mechanical and folds into
   each move PR, and the touch-coupling leg costs one `Ledger-impact:` PR-body
   line (`index.ts:74`) for a PR that adds no ledger row. Governance means
   Status-grammar + link-integrity + touch-coupling; it does NOT obligate
   infra/repo records to carry ledger rows — the ledger stays the product
   decision table, and a rowless record satisfies touch-coupling via the
   declaration, exactly as product records with zero rows do today.

5. **`DECISIONS.md` relocates to `docs/designs/DECISIONS.md`** — the tree
   root, the natural single-ledger home once its current directory dissolves.
   All 214 `Record` cells re-base from product-relative to bucket-qualified in
   two mechanical passes: the gate-cutover PR prefixes every cell with
   `product/` (a scriptable rewrite that keeps every link resolving against
   the unmoved records), then each bucket-move PR re-points its own records'
   cells (`product/<name>/…` → `<bucket>/<name>/…`). Two touches of the cell
   set is the price of not landing a 90-directory mega-PR; each touch is
   mechanically verifiable by the gate itself.

6. **Sequencing: a transitional governed root keeps the gate green at every
   commit.** The gate evaluates a tree snapshot, and the riskiest state is the
   gap between "gate expects the new world" and "records still in the old one".
   The bridge: the gate-cutover PR (T2) lands the code change with `product`
   INCLUDED in `GOVERNED_ROOTS` alongside the six ruled buckets, moves
   `DECISIONS.md`, and re-bases all 214 cells to `product/…`-qualified paths —
   every link resolves, every record is still governed, snapshot green, all in
   ONE atomic PR. Each subsequent bucket-move PR drains records out of
   `product/`/`platform/` into their ruled bucket and re-points their cells in
   the same commit — the tree is resolvable before and after every merge. The
   final native sweep (T10, RIG-2542-gated) empties `product/` and deletes the
   transitional root from `GOVERNED_ROOTS`. `platform/` is never in the list:
   it is ungoverned today and drains ungoverned; its records GAIN governance on
   arrival in `infra/`/`repo/`/`agent/`, with Status normalization in the same
   move PR.

7. **Ledger-link integrity is a same-PR invariant.** Every move PR updates, in
   the same commit: the affected `DECISIONS.md` `Record` cells, inter-record
   relative links, the supersession pointer if its endpoints shift
   (`compass-tauri-shell.md:3`), and every in-repo citation found by grep
   (§Recon 3). `moon run design-ledger-gate:gate` (`index.ts:19`) is the local
   pre-submit check.

8. **URL renames are accepted; no redirects are added — and under this ruling
   the churn is near-total.** Every product record (65), every platform record
   (17), and the ledger itself change URL; only the two existing `repo/`
   records keep theirs. The site has no redirect machinery today (§Recon 2)
   and gains none: it is a young engineering surface with no committed inbound
   URL outside its own tests, and `rewriteLinks` guarantees updated in-repo
   links keep resolving. The honest cost: every existing bookmark, chat link,
   and external citation into `/designs/product/…` or `/designs/platform/…`
   rots at cutover, and this is a one-time cost Matt's ruling accepts by
   choosing the full dissolution. If external links later prove weightier,
   Astro's `redirects` config in `astro.config.mjs` remains the additive seam.

9. **Freeze is not violated by a move.** The freeze convention protects
   *decision content* (`CONTRIBUTING.md:57-59`: "protects a record's **decision
   content**. It does **not** freeze a record's links"); a move changes the
   file's path and its link graph, not one word of its decisions. This is the
   rule-5 carve-out generalized: "This is a link-integrity edit, not a
   decision-content rewrite, so it is not a freeze violation"
   (`CONTRIBUTING.md:67-68`). The reorg makes that explicit as a sixth
   CONTRIBUTING section (T7) so no future reviewer relitigates it. Two narrow
   *metadata* edits ride the same standard: Status-header normalization to the
   gate grammar (`index.ts:87`) for newly-governed records, and one-line
   corrections of a record's stale self-described location
   (`compass-agent-image-publish.md:8-11,500-504` says it "lives in the sealed
   design corpus") — location metadata and machine-checked headers, not
   decisions.

## Alternatives considered

All four candidates below were live options batched to Matt in this record's
pre-ruling Open Questions; his 2026-08-23 ruling picked the heavier full
dissolution over every one of them. Before the ruling, this record was
red-teamed by an adversarial `design-critic` pass (per `skill://design`): the
critique confirmed every load-bearing grounding citation against the tree,
surfaced Option D and the `tooling/`-vs-`repo/` fork, and hardened the
sequencing and hot-lane machinery that survives unchanged into the ruled plan.

- **Option A (the pre-ruling recommendation) — keep `product/`, sub-group it
  in place (`product/{ui,agent,server,meta}/`), dissolve `platform/` into
  top-level `runtime/` + `ci/` + `tooling/`.** Its headline advantage was ZERO
  gate-code change: `PRODUCT_DIR` untouched, nested `<group>/<name>/design.md`
  already governed (`index.ts:212`), the 57 ledger-linked records keep
  governance for free. Matt ruled heavier: once OQ-2(b) expands the gate
  anyway, the zero-gate-change advantage evaporates (the coupling this
  record's pre-ruling analysis flagged explicitly), and keeping `product/` as
  a wrapper denies the product buckets the top-level prominence Matt wanted —
  "ultimately the product design docs are the most important ones".
- **Option C — minimal: rename `platform/` → `infra/`, leave `product/`
  flat.** Cheapest (17 URL renames, zero ledger edits), but it fixes only the
  name collision: runtime/CI/tooling stay lumped (problem 1 half-fixed), the
  65-record flat pile stays (problem 2 unfixed), and a second reorg later pays
  the URL-rename cost twice. Rejected by the ruling for doing too little.
- **Option D — `product/` (sub-grouped as in A) + a single `infra/`
  super-bucket `infra/{runtime,ci,tooling}/`; top level `{product, infra,
  repo}`.** The red-team's contribution: A's information architecture with a
  symmetric tree. The ruling took its `infra/` super-bucket idea (`infra/`
  with `runtime/`/`ci/` subgroups survives verbatim) but rejected the
  surviving `product/` wrapper for the same prominence reason as A, and folded
  its `tooling/` subgroup into `repo/` per OQ-3.
- **The flatten-that-kept-a-product-bucket (pre-ruling Option B) — concern
  buckets `{app, runtime, ci, tooling, meta}`.** The only pre-ruling candidate
  that dissolved both legacy names, and the closest ancestor of what Matt
  ruled. Differences that had it scored down then, resolved now: it kept a
  single `app/` mega-bucket (the ruling splits product into four, which is
  what actually fixes problem 2), it left `DECISIONS.md`'s home and the
  gate's governed set under-specified (the ruling + this record now specify
  both concretely), and its churn — then counted as the decisive cost — is a
  cost Matt explicitly accepted.

## Global Constraints

- **The design-ledger-gate stays green on every commit of every PR.** Each
  move PR is atomic (moves + `Record`-cell re-points + supersession-pointer
  fixes + `Status:` grammar normalization in one commit), verified locally
  with `moon run design-ledger-gate:gate` before submit. The NEW risk under
  this ruling is the gate-code cutover itself: the roots-list change, the
  `DECISIONS.md` relocation, and the 214-cell re-base MUST land in one atomic
  PR (T2) with `product` kept as a transitional governed root — landing any
  subset separately leaves the gate reading a ledger path or cell paths that
  don't exist, an unresolvable tree no follow-up can fix without a red window.
- **The eng-docs build stays green with no dangling in-repo links**: every
  in-repo reference to a moved path (corpus prose, workflow/config/code
  comments, `docs/architecture/`) is updated in the same PR; `moon run :ci`
  builds the site.
- **URL churn is near-total and accepted**: all 65 product + 17 platform
  record URLs and the ledger's URL rename 1:1 with their paths; the site has
  no redirects and gains none; external links and bookmarks into the old paths
  rot at each move PR's deploy. This is a ruled cost, not an oversight.
- **The full re-point surface is swept per move PR**: the 214 ledger `Record`
  cells (re-based twice: `product/`-qualified at T2, bucket-qualified at each
  bucket PR), the 40-file / 67-hit `docs/designs/product` code/config citation
  surface plus the `docs/designs/platform` surface (§Recon 3), and the one
  supersession pointer (`compass-tauri-shell.md:3`).
- **Freeze**: moves change paths and links only; decision content, prose, and
  security sections (`CONTRIBUTING.md` rule 4) are untouched, per the rule-5
  link-integrity carve-out (`CONTRIBUTING.md:67-68`) — the sole exceptions are
  Status-header normalization to the gate grammar and one-line corrections of
  stale self-described locations (§Approach 9).
- **CONTRIBUTING rules 1-5 hold** for every touched file: bare `SEA-####`/`RIG-####`
  provenance, no `oss/compass/` prefixes, no dead links.
- **No move races a live writer (the hot-lane check).** Before each move PR,
  enumerate open PRs and active lanes touching the source paths (via `gh pr
  list` and `jj log`/bookmarks over the bucket) and DEFER any record with an
  in-flight writer — moving a directory another lane is actively editing
  forces the exact rebase/conflict collision this check exists to prevent.
  This generalizes the named RIG-2542 carve-out below from one stack to a
  mechanical pre-move step; the runtime cluster is a known live lane
  (`compass-elastic-session-runtime/` took a new file post-draft), so T3
  applies this check first.
- **RIG-2542 coordination** (a named instance of the hot-lane check):
  `compass-native-*` records are NOT moved while the compass-native/
  service-owner RIG-2542 stack is open — T8 executes around them, they remain
  under the transitional `product` root (still governed), and the deferred
  sweep is tracked as T10. Design-time reference to them is fine.
- **CONTRIBUTING §6 documents the new taxonomy** (bucket set, subgroup layout
  rule, flat-at-root governance rule, move-is-not-a-freeze-violation, the
  expanded gate) and `config/skills/design/SKILL.md:68-69`'s domain list is
  updated to the ruled set.
- **This record moves nothing.** Execution is a later PR series; this record
  is the contract it reads.

## Plan

### T1 — Ruling intake: freeze the taxonomy (DONE)

Matt ruled OQ-1/OQ-2/OQ-3 on 2026-08-23 (§Resolved decisions); this re-authored
record is the frozen contract. No file moves.

Interfaces:

- Consumes: Matt's ruling (ask, 2026-08-23).
- Produces: this record, re-authored in place.
- Depends on: nothing. Everything below depends on T1.

### T2 — Gate cutover: governed-roots list + DECISIONS.md relocation (atomic, CRITICAL)

One PR, one commit, all of:

1. `tools/design-ledger-gate/index.ts`: `PRODUCT_DIR`/`DECISIONS_PATH` →
   `DESIGNS_ROOT` + `GOVERNED_ROOTS = ["ui", "agent", "server", "meta",
   "infra", "repo", "product"]` (`product` transitional; `ui/agent/server/meta/
   infra` are empty directories-to-be at this commit — an empty root scans to
   zero records, which is green) + `DECISIONS_PATH =
   "docs/designs/DECISIONS.md"`; generalize `touchesRecord`, the record glob,
   `readRecord`, `resolveRecordRelative` per §Approach 3.
2. `git mv docs/designs/product/DECISIONS.md docs/designs/DECISIONS.md`.
3. Re-base all 214 `Record` cells: prefix every cell target with `product/`
   (mechanical, scriptable — every link resolves against the unmoved records).
4. Re-pin `index.test.ts` literals + add the new cases (§Approach 3); update
   the `.moon/workspace.yml:57` comment and the gate's own header comments
   (`index.ts:3-8`) and `moon.yml` citation.
5. Newly-governed `repo/` records get grammar-conformant headers:
   `compass-eng-docs/design.md` gains `Status: Active` (it has none today —
   verified this session); this record already carries `Status: Draft`.
6. PR body declares `Ledger-impact: mechanical re-base, no decision changed`.

Interfaces:

- Consumes: T1 (this record).
- Produces: the generalized gate; `docs/designs/DECISIONS.md`; 214
  `product/`-qualified Record cells; updated fixtures/comments.
- Test cycle: `bun test tools/design-ledger-gate` + `moon run
  design-ledger-gate:gate` + `moon run :ci` green — the gate proves its own
  cutover.
- Depends on: T1. Everything that moves a governed or to-be-governed record
  depends on T2.

### T3 — infra/runtime/ move (hot-lane check first)

Run the hot-lane pre-move check (Global Constraints); the runtime cluster is a
known live lane. Then move
`docs/designs/platform/{compass-elastic-session-runtime,compass-runner-gateway-error-sentinels,compass-runner-concurrent-dispatch,compass-runner-arbitrary-uid}` →
`docs/designs/infra/runtime/`. Normalize
`compass-runner-gateway-error-sentinels/design.md` `Status: Ratified (Matt,
2026-08-05) …` → `Status: Active` (gate grammar, `index.ts:87` — these records
are governed on arrival). Update citations: `devenv.nix:167`,
`go/internal/runner*` comment citations, intra-corpus links (the cluster
cross-links relatively, e.g. `microvm-runner.md:5` links `./design.md` —
unaffected by a whole-dir move; verify via docsite build). PR body:
`Ledger-impact: records relocated into governance, no decision changed`.

Interfaces:

- Consumes: T2 merged; hot-lane check clean (else defer the hot record).
- Produces: `docs/designs/infra/runtime/**` (4 record dirs, file set
  re-enumerated at move time); Status normalization; citation edits per
  `grep -rn "docs/designs/platform/compass-\(elastic\|runner\)"`.
- Test cycle: `moon run design-ledger-gate:gate` + `moon run :ci` green.
- Depends on: T1, T2 (parallel with T4, T5).

### T4 — infra/ci/ move

Move `docs/designs/platform/{compass-dogfood-e2e,compass-dogfood-loop,compass-dogfood-e2e-steer-deliver-seam,compass-pr-validation,compass-local-dev}` →
`docs/designs/infra/ci/`, and convert
`compass-agent-image-publish.md` → `infra/ci/compass-agent-image-publish/design.md`
(flat `.md` inside a subgroup falls out of governance — §Approach 2). Normalize
Status headers to the grammar: `compass-dogfood-e2e` `**Draft** — …` → `Draft`;
`compass-dogfood-loop` `**decided** — …` → `Active`; `compass-pr-validation`
`**Draft**` → `Draft` (verified per-file this session). Correct the stale
self-location note (`compass-agent-image-publish.md:8-11,500-504`, §Approach 9).
Update citations: `.github/workflows/publish-agent-image.yml:40`,
`docs/architecture/build-and-ci.md:128`, intra-corpus links
(`compass-dogfood-e2e-steer-deliver-seam/design.md:6` links
`../compass-dogfood-e2e/design.md` — both move together, survives; verify).

Interfaces:

- Consumes: T2 merged.
- Produces: `docs/designs/infra/ci/**` (6 records, one converted to dir
  layout); Status normalizations; citation edits per grep sweep.
- Test cycle: `moon run design-ledger-gate:gate` + `moon run :ci` green.
- Depends on: T1, T2 (parallel with T3, T5).

### T5 — repo/ fold (dependency/tooling records)

Move `docs/designs/platform/{compass-agent-effect-adoption,compass-agent-effect-otel}`,
`compass-renovate-migration.md`, `compass-effect-adoption-decision.md`,
`compass-drop-proto.md` → `docs/designs/repo/` (Matt's OQ-3; flat records stay
flat — `repo/` root is governed). Normalize
`compass-effect-adoption-decision.md` `Status: Decided (Matt, 2026-08-20)` →
`Status: Active`. Update citations: `.github/workflows/renovate.yml:7`,
`tools/renovate/refresh-devenv-nixpkgs.ts`, `buf.gen.yaml` context comment,
`packages/compass-agent/src/transport/*` comment citations (grep sweep), the
effect records' cross-links (`compass-agent-effect-adoption/design.md:12`
links `../compass-effect-adoption-decision.md` — both move together, survives;
`compass-agent-effect-otel/design.md:5,339-340,382` cites the adoption record
and its own path by absolute repo path — re-point).

Interfaces:

- Consumes: T2 merged.
- Produces: 5 records under `docs/designs/repo/`; Status normalization;
  citation edits.
- Test cycle: `moon run design-ledger-gate:gate` + `moon run :ci` green.
- Depends on: T1, T2 (parallel with T3, T4).

### T6 — Product-behavior strays → agent/; platform/ deleted

Move `docs/designs/platform/compass-initial-prompt-removal.md` and
`docs/designs/platform/compass-forks-reversal/` → `docs/designs/agent/`
(§Approach 2 grounding). Normalize `compass-forks-reversal/design.md:7`
`Status: proposed. Owner: compass-repo.` → `Status: Draft` (only `design.md`
needs it: the sibling `oq-resolutions.md` is a nested flat `.md`, rejected by
the flat-file rule, so its header is inert). These records gain the
touch-coupling obligation; the PR body declares `Ledger-impact: records
relocated into governance, no decision changed` (`LEDGER_IMPACT_RE`,
`index.ts:74`). After this PR `docs/designs/platform/` is empty and the
directory disappears.

Interfaces:

- Consumes: T2-T5 merged (so `platform/` empties exactly here).
- Produces: 2 records under `docs/designs/agent/`; `Status:` normalization;
  citation edits (`go/cmd/compass-runner/main.go`, `forks/README.md`, grep
  sweep); PR-body `Ledger-impact:` line.
- Test cycle: `moon run design-ledger-gate:gate` + `moon run :ci` green.
- Depends on: T1, T2, T3, T4, T5.

### T7 — Document the taxonomy (CONTRIBUTING §6 + design skill)

Add a sixth section to `docs/designs/CONTRIBUTING.md`: the ruled bucket set
(`ui/ agent/ server/ meta/ infra/ repo/`), what belongs where, the
`<bucket>/[<subgroup>/]<name>/design.md` layout rule (flat `.md` governed at a
bucket root only), the move-is-not-a-freeze-violation statement (§Approach 9),
the same-PR link-integrity obligation extended from rule 5 to moves, the
expanded-gate governance (all buckets; ledger rows remain product-decision
scoped, touch-coupling satisfied by declaration), and a one-line note that
`product/` persists transitionally until the RIG-2542-gated native sweep (T10)
removes it. Update `config/skills/design/SKILL.md:68-69`'s domain list from
`platform / tools / agents / product` to the ruled set.

Interfaces:

- Consumes: T1, T2 (the gate semantics it documents are live).
- Produces: `docs/designs/CONTRIBUTING.md` (new §6),
  `config/skills/design/SKILL.md:68-69` edit.
- Test cycle: `moon run :ci` (markdownlint + docsite) green.
- Depends on: T1, T2; lands before T8.

### T8 — Dissolve product/ into ui/ agent/ server/ meta/ (per-bucket PRs)

Four PRs, one per destination bucket, membership per §Approach 2: move
`product/<name>` → `<bucket>/<name>` (flat records stay flat at the bucket
root — governed there, no layout conversion needed); re-point each moved
record's `DECISIONS.md` `Record` cells (`product/<name>/…` →
`<bucket>/<name>/…`) and inter-record links in the same commit; sweep the
40-file code-citation surface per moved record. Run the hot-lane pre-move
check per PR. EXCLUDE the `compass-native-*` records
(`compass-native-app/`, `compass-native-packaging/`,
`compass-native-client-mode/`, `compass-native-client-only/`) while RIG-2542
is open — they stay under the transitional `product` root, still governed.
ALSO defer `compass-tauri-shell.md` with them: it is `Superseded by
compass-native-app/design.md` (`compass-tauri-shell.md:3`) and functionally
part of that family — deferring it moves the corpus's only supersession
pointer ONCE (in T10) instead of twice, and avoids any intermediate
cross-bucket pointer state. (`compass-multi-window/` and
`compass-stack-cross-process-teardown/` are native-family by content but not
by the `compass-native-*` carve-out name — the hot-lane check decides at move
time whether they ride T8's ui/ PR or T10.) PR bodies carry
`Ledger-impact: link re-point only` for the touch-coupling leg. The four bucket
PRs land **serially (rebase-ordered)**, not concurrently: each re-points cells
in the single `docs/designs/DECISIONS.md`, so parallel opens would contend on
that one file and force mechanical rebases (no red window — each PR stays atomic
and self-green via the transitional `product` root — purely merge friction).

Interfaces:

- Consumes: T2 (gate), T7 (documented contract); hot-lane check per PR.
- Produces: `docs/designs/{ui,agent,server,meta}/**` per §Approach 2's roster;
  `DECISIONS.md` Record-cell edits (the 214-cell set, batched per bucket PR);
  citation edits.
- Test cycle: per PR, `moon run design-ledger-gate:gate` + `moon run :ci`
  green.
- Depends on: T1, T2, T6 (strays already in `agent/`), T7.

### T9 — Post-move verification sweep

Repo-wide grep proves zero live references to `docs/designs/platform/` (the
two pre-existing dead sealed citations `buf.gen.yaml:9` / `devenv.nix:208` are
re-pointed to prose or left with an explicit "sealed-private" annotation —
executor's call, flagged in the PR) and that the only remaining
`docs/designs/product/` paths are the T10-deferred native-family records and
their Record cells. Docsite build renders every record; spot-check routes for
one record per bucket against `routeSlug` expectations.

Interfaces:

- Consumes: T3-T8 merged.
- Produces: verification evidence in the final PR description; any stragglers
  fixed.
- Test cycle: `git grep -n "docs/designs/platform"` → only historical prose
  inside frozen records (each verified non-link); `git grep -n
  "docs/designs/product"` → only the deferred native set; `moon run :ci`
  green.
- Depends on: T3-T8.

### T10 — Deferred native-family sweep + transitional-root removal (gated on RIG-2542)

The one task with an external gate. Once RIG-2542 closes: move the deferred
`compass-native-*` records + `compass-tauri-shell.md` (and, if T8 deferred
them, `compass-multi-window/` / `compass-stack-cross-process-teardown/`) →
`docs/designs/ui/`; re-point their Record cells; rewrite the supersession
pointer to its final same-bucket record-relative form
(`compass-native-app/design.md` from `ui/compass-tauri-shell.md` — verified
against the generalized `resolveRecordRelative`: resolves inside
`DESIGNS_ROOT`, no escape); remove `product` from `GOVERNED_ROOTS` and delete
the empty `docs/designs/product/` directory; drop CONTRIBUTING §6's
transitional note. **Filed as its own tracked follow-up issue at T8 submit
time** (not left as prose) so the debt is visible on the board per
`rule://own-your-issue` — a half-dissolved `product/` with no tracking anchor
is the hole this task closes.

Interfaces:

- Consumes: RIG-2542 closure; T8, T9 merged.
- Produces: the native family under `ui/`; final supersession-pointer form;
  `GOVERNED_ROOTS` without `product`; `docs/designs/product/` gone; §6 note
  removed.
- Test cycle: `moon run design-ledger-gate:gate` + `moon run :ci` green;
  `git grep "docs/designs/product"` → zero live references.
- Depends on: T8, T9 + RIG-2542 closure (external gate).

## Tasks

- [x] T1 — Matt ruled OQ-1/OQ-2/OQ-3 (ask, 2026-08-23); taxonomy frozen into this record.
- [ ] T2 — gate cutover: `GOVERNED_ROOTS` (+ transitional `product`) + `DECISIONS.md` → `docs/designs/DECISIONS.md` + 214-cell `product/` re-base + fixtures, one atomic PR; gate green.
- [ ] T3 — `infra/runtime/` move (4 records) + hot-lane check + Status normalization + citations; CI green.
- [ ] T4 — `infra/ci/` move (6 records, `compass-agent-image-publish` flat→dir) + Status normalizations + citations; CI green.
- [ ] T5 — `repo/` fold (5 dependency/tooling records) + Status normalization + citations; CI green.
- [ ] T6 — strays → `agent/` + `Status:` normalization; `platform/` deleted; gate green.
- [ ] T7 — CONTRIBUTING §6 taxonomy + design-skill domain list updated.
- [ ] T8 — `product/` dissolved into `ui/ agent/ server/ meta/` (per-bucket PRs; native family + tauri-shell deferred to T10); ledger cells re-pointed; gate green.
- [ ] T9 — zero-reference verification sweep; evidence in PR.
- [ ] T10 — native-family sweep + `product` root removal, gated on RIG-2542 closure; filed as its own tracked issue at T8 submit.

## Resolved decisions

- **OQ-1 (bucket set) — RULED (Matt, ask 2026-08-23):** dissolve `product/`
  into four top-level buckets `ui/ agent/ server/ meta/` ("ultimately the
  product design docs are the most important ones" — they get top-level
  prominence), dissolve `platform/` into a top-level `infra/` (sub-groupable:
  `infra/runtime/`, `infra/ci/`), each top-level bucket may sub-group. The
  pre-ruling recommendation (Option A, keep `product/` sub-grouped) and the
  red-team's Option D are rejected — §Alternatives.
- **OQ-2 (governance domain) — RULED (Matt, ask 2026-08-23): (b) expand the
  gate** to a bucket list / all of `docs/designs/`. Implemented as
  `GOVERNED_ROOTS` enumerating every bucket (§Approach 3-4), with the
  `DECISIONS.md` relocation to `docs/designs/DECISIONS.md` and bucket-qualified
  Record cells. The gate-code change is IN SCOPE for this reorg (T2) — forced
  regardless, since the gate's sole root ceases to exist.
- **OQ-3 (`tooling/` vs `repo/`) — RULED (Matt, ask 2026-08-23): fold
  `tooling/` into `repo/`.** The five dependency/library records join the
  existing `repo/` bucket (T5); no `tooling/` bucket exists.
- **Governed-set composition (design call under the OQ-2 ruling):** all six
  buckets are governed, not just the four product-derived ones — Matt's "a
  bucket list / all of docs/designs/" language, plus the grounded tradeoff in
  §Approach 4 (the ungoverned-island precedent, the already-visible Status
  drift, the one-line touch-coupling cost). Ledger rows remain product-scoped;
  governance of `infra/`/`repo/` means Status + link integrity +
  touch-coupling only.
- **Non-load-bearing deferral — stale location claims in frozen records:**
  historical statements like `compass-eng-docs/design.md:386-387` ("platform/
  … does not exist in compass") become false-in-hindsight after the reorg.
  Decision: leave frozen records verbatim (they were true when written; freeze
  protects them) and let CONTRIBUTING §6 carry the current truth. The sole
  exceptions are the two metadata edits in §Approach 9. This deferral is
  cosmetic either way and blocks nothing.
