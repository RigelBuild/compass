# Compass Linear webhook mount — amendment: provider-suffixed path, mount owned by the forge notification lane

Status: Active
Tracker: RIG-2717

> **Extends #625 (frozen).** This record is a sibling amendment to
> `docs/designs/product/compass-linear-agent-responder/design.md` (merged in
> #625). The merged record is frozen; per house convention a later change ADDS
> a record. This amendment changes two things about the frozen record's Part 1 /
> T7 webhook receiver: (1) the mount **path** moves from bare `/webhooks` to
> `/webhooks/linear`, and (2) the **ownership** of the mount itself moves from
> RIG-2717 T7 to the forge notification lane (RIG-2732), which lands a single
> shared Linear webhook handler both lanes ride. Every other DL-254 invariant —
> the network-TLS-door placement, the fail-closed raw-body HMAC check, the
> ack-200-before-work discipline, the per-deployment public base URL — survives
> unchanged.

## Problem / Intent

The frozen record mounts the Linear responder at a **bare** path and assigns
the mount to RIG-2717 T7:

> "The receiver is a **plain `netMux.Handle("/webhooks", …)` alongside the
> Connect paths, NOT a Connect service**" — #625 design.md:125-126
>
> "### T7 — Server: mount `/webhooks` on the network door + assembly … In
> `buildNetworkServer` … `netMux.Handle("POST /webhooks", h)` beside the Connect
> mounts" — #625 design.md:775-785

Two things have changed since that freeze:

1. **A second webhook provider landed.** RIG-2883 mounted the GitHub App
   ingress at `POST /webhooks/github`
   (`go/server/github_webhook.go:24`, `go/server/network_door.go:315-325`) — a
   provider-suffixed path. That leaves the Linear side asymmetric: bare
   `/webhooks` for one provider, `/webhooks/github` for the other. The bare
   path was only ever a historical artifact — RIG-2717 was designed before any
   GitHub webhook existed, so it claimed the unqualified path.

2. **The forge notification lane (RIG-2732) needs the same Linear webhook.**
   That record already routes Linear **data-change** events (`Issue`/`Comment`
   create/update/remove) through the **same** Linear webhook and handler, with
   an inner `type`-switch handing `AgentSessionEvent` to the RIG-2717
   dispatcher and data-change types to its own arms
   (`compass-forge-agent-notification/design.md:214-234`, §Approach). Session
   and data-change events arrive on one Linear webhook — same path, same
   secret, same signature scheme — so there is exactly **one** signature-verify
   and one mount, not two.

RIG-2732's own Global Constraints originally sequenced its mount work *after*
RIG-2717's mount landed
(`compass-forge-agent-notification/design.md:507-511`). RIG-2717 shipped the
responder **library** (verify/parse/client #638, routing/dispatcher #646,
public-URL #639, the `linear_agent_sessions` table + `@linear` seed) but never
landed T7 (the mount + dispatcher assembly + goroutine start) or T8 (the e2e).
So the mount is still unbuilt, and the sequencing question is live rather than
settled.

Matt ruled both points (RIG-2717, 2026-08-30):

- **The mount path is `/webhooks/linear`**, not bare `/webhooks` — symmetric
  with `/webhooks/github`.
- **The forge notification lane (RIG-2732) lands the shared mount.** One Linear
  webhook handler, owned by the lane that already needs the inner type-switch,
  with the RIG-2717 dispatcher riding it through a seam.

## Approach

Two mounts on the network TLS door, one per provider, each with its own
fail-closed verify — the shape RIG-2883 already established for GitHub:

- `POST /webhooks/github` — the GitHub App ingress (RIG-2883, landed).
- `POST /webhooks/linear` — the Linear ingress (this amendment). A single
  `http.Handler` doing one raw-body HMAC-SHA256 `Linear-Signature` verify, then
  an inner `type`-switch: `AgentSessionEvent` → the RIG-2717 dispatcher;
  `Issue`/`Comment` data-change → the RIG-2732 notify/board arms.

### Ownership split at the seam

- **RIG-2732 (compass-forge) owns:** `netMux.Handle("/webhooks/linear", …)` in
  `buildNetworkServer` (`go/server/network_door.go:315-325`, beside the GitHub
  mount), the single Linear HMAC verify (reusing
  `linearagent.VerifySignature(secret, rawBody, headerHex)`, landed
  `go/internal/linearagent/webhook.go:65`), the stale-timestamp guard, and the
  inner `type`-switch. The data-change arms and their normalize/route path are
  RIG-2732's.
- **RIG-2717 (compass-server) owns:** the dispatcher **assembly** — wiring
  `linearagent.NewDispatcher` with its concrete seams
  (`ResolveResponder`, the `*comms.Comms` poster, the `linear_agent_sessions`
  store, the Linear API client), starting `Dispatcher.Run` on the serve
  errgroup beside the other bus consumers, and exposing the session seam the
  handler calls:

  ```go
  // linearagent.Dispatcher (landed, go/internal/linearagent/dispatcher.go:145)
  func (d *Dispatcher) Enqueue(ev *SessionEvent) error // full queue -> ErrQueueFull
  ```

  The handler maps `ErrQueueFull` to HTTP 500 so Linear retries the delivery
  (the frozen §T6 contract, #625 design.md:742-745). `ParseSessionEvent`
  (`webhook.go:55`) turns the verified raw body into the `*SessionEvent` the
  seam takes.

This keeps one verifier and one mount while preserving the frozen
responder-library boundary: RIG-2717 still owns everything from `Enqueue`
inward (route → topic → association → the two `created` emits), and RIG-2732
owns the front door.

### What RIG-2717's residual T7/T8 become

- **T7 (RIG-2717)** shrinks to **assembly-only**: construct + wire the
  dispatcher, start its goroutine, expose the `Enqueue` seam. It no longer
  mounts anything.
- **T8 (RIG-2717)** — the full-loop e2e against a fake Linear — stays with
  RIG-2717, driven through the shared `/webhooks/linear` mount once RIG-2732
  lands it. The rollout gate (a real delegation round-trip, #625
  design.md:795-804) is unchanged in substance; only the URL path it exercises
  moves to `/webhooks/linear`.

### Runbook / ops fallout

The per-deployment Linear App webhook URL registered in the Linear console
moves from `<public-base>/webhooks` to `<public-base>/webhooks/linear` (the T0
ops step, #625 design.md:594-603). This is free: registering that URL is
already a manual console step in the runbook — the "Agent session events"
category auto-disabled on the original 404s and requires a manual re-enable
regardless (#625 design.md:605-609) — so the path simply lands correct the
first time it is re-registered. The server-resolved indirection link
(`<public-base>/l/session/<id>`, DL-268) is unaffected: it is a separate
read-only route, not under `/webhooks`.

## Rejected alternatives

- **Keep the mount in RIG-2717, forge extends it later.** Faithful to the
  original sequencing, but serializes RIG-2732's Linear data-change arm behind
  a RIG-2717 mount that had not landed, for no benefit: the handler's body is
  the type-switch RIG-2732 needs anyway, so the lane that owns the switch
  should own the mount. Overruled by Matt.
- **Two separate Linear handlers (one per event class) on two paths.**
  Rejected: session and data-change events share one Linear webhook, one
  secret, and one signature scheme, so two handlers would run two identical
  fail-closed verifiers over the same delivery — the exact interleaving RIG-2883
  avoided by keeping *providers* (not event classes) on separate paths
  (`compass-forge-agent-notification/design.md:210-213`).
- **Keep bare `/webhooks` for Linear.** Rejected: asymmetric with
  `/webhooks/github` and a historical artifact. Overruled by Matt.

## Ledger-impact

Ledger-impact: adds DL-302 to `docs/designs/DECISIONS.md` under the "Linear
agent responder" section. DL-254 does NOT flip: its status cell stays `Active`
and its frozen Decision-cell prose is untouched (rows are append-only,
`tools/design-ledger-gate/index.ts:25-27`) — this amendment is its amending
sibling, and the new row carries the delta, mirroring the return-path amendment
(DL-268 amending DL-256 without flipping it,
`compass-linear-return-path-indirection-amendment/design.md` §Ledger-impact).
Record links are relative to `docs/designs/DECISIONS.md`, so they carry the
`product/` prefix (`tools/design-ledger-gate/index.ts:52-60` includes `product`
in `GOVERNED_ROOTS`).

Spec-impact: none. This is a docs-only design record; it changes no
`compass.v1` contract and adds no requirement to `docs/specs/`.
