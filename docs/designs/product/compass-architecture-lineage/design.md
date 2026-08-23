# Compass architecture lineage

Status: Active

The early Compass milestone records (v0.3 through v0.8) are retired. Every
load-bearing decision they made is now a row in
[`DECISIONS.md`](../DECISIONS.md) — the canonical index of current truth — so the
milestone narratives themselves carried nothing live except a thin layer of
still-true *rationale*: the "why not the alternative" context a one-line ledger
row deliberately drops. This record preserves that rationale in one place and
points each note at the ledger row that holds the decision's authority. It is
not a new decision surface; it is the surviving reasoning behind decisions ruled
elsewhere.

Read the ledger first. Come here for *why* a ledgered decision went the way it
did, when that reasoning is not obvious from the one-line statement.

## The current shape, in one paragraph

Compass is a three-tier system — Client → Server → Runner, with the
communication layer as the spine ([DL-007](../DECISIONS.md)). Postgres is the
store of record and is not swappable; an in-memory event bus is a cache/fan-out
ring over it, not a second store ([DL-020](../DECISIONS.md),
[DL-021](../DECISIONS.md)). Transport is gRPC everywhere, authenticated by
per-Runner provisioned tokens ([DL-013](../DECISIONS.md)). The in-container
agent is a first-party program on the Oh My Pi SDK, emitting `compass.v1`
natively ([DL-023](../DECISIONS.md)), one per per-agent container on the Runner
for blast-radius isolation ([DL-024](../DECISIONS.md)). The UI shell is
board-primary, with channel chat folded into the workspace
([DL-031](../DECISIONS.md)), rendering a first-party typed session trace over a
typed gRPC stream ([DL-039](../DECISIONS.md)). The agent tree is the organizing
primitive ([DL-095](../DECISIONS.md)).

## Carried-over rationale

Each note is context the ledger row cites but does not restate. Grouped by area;
the linked row is the authority.

### Topology & seams

- **Commodity layers behind thin Compass-owned seams.** The posture is to lean
  on proven infrastructure (Postgres, OCI, gRPC) and own only the thin seam that
  adapts it, rather than build substrate from scratch — so a substrate choice
  stays swappable behind the seam without rewriting the product above it. The
  seam is the Compass-owned surface; the layer behind it is replaceable.
- **The comms layer is the *structure* of the product, not a pipeline.** Audit
  and search are substrate properties — every message is a durable, queryable
  row — not a feature bolted onto a message bus. This is why comms is built
  first-party ([DL-021](../DECISIONS.md)) rather than adopted as a dependency:
  the primacy of the comms substrate is the reason to own it, and the one-line
  row names the mechanism (Postgres write-through fan-out), not the primacy.
- **Postgres store + in-memory bus is a deliberate duality.** The store of
  record and the fan-out ring are two things on purpose
  ([DL-020](../DECISIONS.md), [DL-021](../DECISIONS.md)): the store is the
  durable truth, the ring is an ephemeral cache/fan-out for live delivery. The
  ring is never a second store; losing it loses no committed state.

### Runner, containers, config

- **A per-agent container is a structural sandbox, not credential avoidance.**
  Isolation, clone-per-workstream, scoped credentials, and default-deny egress
  are the reason for the container boundary ([DL-024](../DECISIONS.md)) — the
  blast radius is structural, so a compromised agent is contained by the
  boundary itself rather than by trusting it to hold a narrow credential.
- **Containers are throwaway; durable state lives in the Server.** An agent
  container can be torn down and relocated without context loss because nothing
  durable lives in it — the session log and all state are the Server's
  ([DL-020](../DECISIONS.md), and the session-persistence chain
  DL-088/DL-089). This is what makes restart-and-resume a first-class operation
  rather than a recovery hack.
- **Config distribution is a Runner-mediated pull to a local read-only mount to
  avoid a cross-host network filesystem** ([DL-022](../DECISIONS.md)). The
  rejected alternative was mounting config over the network into every
  container; materializing it locally per Runner keeps the container's config
  read path host-local and offline-survivable. The row names the pull; the
  rationale is the network-FS it refuses.
- **Devenv-per-project container composition** lets one agent host span multiple
  repositories with their toolchains composed into the container image, rather
  than one container per repo — the multi-repo unification an agent workstream
  needs.

### The authenticated door

- **One ALPN/h2 port serves both gRPC-Web and native gRPC** — the authenticated
  listener multiplexes on the protocol, so operators expose a single port rather
  than a Web gateway beside a native one ([DL-012](../DECISIONS.md)).
- **Operator-brings-PEM certs; ACME is deferred.** The cert model is
  operator-provisioned PEM material ([DL-012](../DECISIONS.md)); automated
  issuance is a later additive, not a v1 dependency, so the door has no
  build-time coupling to an ACME provider.
- **The provisioned token is written to a `0600` file, never stdout or a log.**
  A minted Runner token is a bearer credential; it lands in a mode-`0600` file
  and is never emitted to a stream a log aggregator could capture.
- **The token is per-Runner, not per-job.** A Runner enrolls once and holds one
  token for its lifetime ([DL-013](../DECISIONS.md)); a per-job token was
  rejected as churn with no isolation gain, since the Runner is already the
  trust boundary.
- **Async token-resolve cannot run in a sync `tonic` interceptor** — resolving a
  bearer token against Postgres is an async call, and the interceptor seam is
  synchronous, so token authz sits at an async layer above the interceptor. A
  live future constraint on where auth can execute, not a free swap.
- **The event bus attaches-first, then snapshots across two locks, to drop
  nothing.** A subscriber attaches to the live fan-out before reading the
  backlog snapshot, taking the snapshot under a lock ordering that guarantees no
  message falls between the snapshot and the live stream — the no-drop mechanism
  behind the cache/fan-out ring ([DL-021](../DECISIONS.md)).

### Storage & ownership

- **Transcript bodies live in object storage behind a blob seam**, not in
  Postgres rows ([DL-019](../DECISIONS.md)) — the store of record indexes them;
  the bodies ride an S3-compatible seam so large transcripts never bloat the
  relational store.
- **Owner-membership is transitive.** An agent inherits its owner's channel
  visibility, so ownership is the single axis that resolves what an agent can
  see rather than a separate per-agent ACL.
- **Secrets cross a boundary contract**: encryption-at-rest, per-principal
  authorization, and audit redaction are the three properties any secret-bearing
  path must hold — the contract later realized by the Server ownership layer
  ([DL-052](../DECISIONS.md)).

### UI shell & session rendering

- **The board-primary reshape folds comms *into* the workspace.** The shell
  moved back from a channel-first prototype to a board-primary workspace with
  channel chat as a surface within it ([DL-031](../DECISIONS.md)); the rationale
  is that supervision is board-first — an operator watches the fleet board and
  drops into a channel from it, not the reverse.
- **Agent identity composes at read time.** An `Account` and its
  `AgentLifecycle` are co-addressed on the account id and composed when read,
  rather than denormalized into one row — so lifecycle state and account
  identity evolve independently.
- **The log/trace panel is a fixed, minimizable topology** — an observation pane
  with a fixed position that minimizes rather than a free-floating or
  closable one, so the operator's spatial model of the workspace is stable.
- **The session trace is a typed contract, not opaque bytes and not ACP.**
  Session events cross a typed gRPC stream and Compass renders them first-party
  ([DL-039](../DECISIONS.md)); the block-level CSS taxonomy
  (`.block-thinking` / `.block-tool` / `.block-plan` / `.block-diff`) is the
  live UI contract that typing buys — a renderer that understands block kinds
  rather than replaying an opaque byte stream.

## Why one lineage record, not seven preserved milestones

The milestone records were a version narrative — each superseded the last, so
six of the seven were already history and the seventh (v0.8) held only decisions
now ledgered. Keeping them invited the confusion of reading a superseded
narrative as current truth. Consolidating the still-true rationale here, with
every decision's authority in the ledger, keeps one canonical index and one
rationale companion instead of a chain of overturned drafts.
