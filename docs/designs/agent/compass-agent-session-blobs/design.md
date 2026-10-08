# Compass agent session blobs survive resume

> Freezes on merge; later changes supersede by citation, never rewrite.

Issue: RIG-1582. Parent record:
[compass-agent session persistence](../compass-agent-session-persistence/design.md#oq-r3--inline-image-blobs--out-of-mvp-resume-scope-deferred-to-rig-1582)
(OQ-R3 deferred blobs to this issue). Storage ruling: RIG-4757 option B
(Matt, 2026-10-06).

## Problem / Intent

A resumed agent session loses every inline image. The SDK writes image bytes to
a local content-addressed blob directory and keeps only a `blob:sha256:<hex>`
reference in the session JSONL. Compass tees the JSONL to the Server, but the
blob directory dies with the container. On resume the reference does not
resolve, and the model sees the SDK's "undecodable image data" text. RIG-1582
makes the bytes survive a resume when the Server has an object store, and makes
the loss explicit ("image unavailable after resume") when it does not.

## Approach

Matt ruled the substance (RIG-4757 option B):

- Blob bytes go through the existing Server `ObjectStore` seam under the key
  `blobs/<sha256>`, with a Postgres index row.
- Capture is best-effort.
- With no object store there is no blob persistence, and a resumed agent sees
  an explicit marker in place of the SDK warning.

Everything below is mechanism.

### D1 Capture from the tee

The agent captures blobs from the committed JSONL lines the tee already sees.

In SDK 18.0.11, `#lineFor` builds each persisted line through
`prepareEntryForPersistence`. That step externalizes images synchronously
before the line exists (`truncateForPersistence`,
`session/session-persistence.ts`):

```ts
 * Runs in one synchronous tick ... Image externalization happens via the
 * synchronous blob-store path (`fs.writeFileSync`), so blob bytes are in the
 * kernel page cache before the JSONL line referencing them is written.
```

`BlobStore.putSync` (`session/blob-store.ts`) names the file by the hash of the
stored bytes:

```ts
const hash = new Bun.SHA256().update(data).digest("hex");
const blobPath = path.join(this.dir, hash);
```

So each `blob:sha256:<hex>` in a line reaching `TranscriptTeeBackend.append`
or `writeFull` (`packages/compass-agent/src/session-tee.ts`) names an existing
file under the hash the SDK looks up on load. This holds for any position and
for checkpoint rewrites.

Rejected: an `onEntryAppended` hook. It sees entries before externalization, so
the agent would have to re-derive the SDK's position and threshold rules to
reproduce each ref. The SDK's collab host also overwrites that property.

Mechanics:

- The tee gets one option, `onCommittedLine(line)`. It runs synchronously
  after the local write and before the frame is teed. It is wrapped in
  `try/catch`, so a throw is logged and never reaches the SDK.
- `SessionBlobUploader.offer(line)` matches `blob:sha256:([a-f0-9]{64})`. It
  skips hashes that are done or already queued and enqueues the rest. The
  queue is bounded at `SESSION_BLOB_QUEUE_MAX`; it drops the oldest entry and
  counts the drop.
- One worker reads `<blobsDir>/<hash>` and sends it, with one upload in flight.
- A hash enters the done set only on success, `FailedPrecondition`, or
  oversize (over `MAX_SESSION_BLOB_BYTES`). `FailedPrecondition` also disables
  the uploader for the lifetime.
- Two outcomes do not mark a hash done, and each has its own counter: ENOENT
  (`missing`) and any other error (`failed`). There is no retry loop. A later
  line that still references the hash offers it again.
- The done set is not seeded from disk. After a resume, the first checkpoint
  sends every referenced blob once more. The Server's own-row check (D3) turns
  that into a no-op with no PUT.
- `blobsDir` comes from the SDK's `getBlobsDir()` (`@oh-my-pi/pi-utils`, pinned
  to the SDK's exact version), and the resolved path is logged once at boot.
  `isReservedEnvKey` (`packages/compass-agent/src/cli.ts`) also reserves
  `PI_CODING_AGENT_DIR` and `XDG_DATA_HOME`, so the env file cannot move the
  dir off the Runner's fixed `.omp/agent/blobs`.

### D2 A dedicated best-effort unary

Blob bytes use a new AgentGateway unary, `PutSessionBlob{sha256, data}`. The
Runner forwards it as `RunnerService.RelaySessionBlob`. The forwarder copies
the shape of `Gateway.Board` (`go/internal/runner/gateway/board.go`):

- it resolves the bound session, and a miss is `CodePermissionDenied`;
- it sends the session id the Runner owns together with the verbatim request;
- it returns the Server's code unchanged.

Rejected carriers:

- **The durable conversation lane.** It is delivered-or-erred with a fatal
  latch. Its handler, `Hub.commitFrame` (`go/internal/runnerhub/relay_comms.go`),
  also accepts only transcript entries.
- **Inline base64 in the teed lines.** A checkpoint body with several images
  would pass the 16 MiB cap and trip the R4 latch. It would also bloat the PG
  hot tail and go against the ruling.

Size: AgentGateway reads `agentmsg.MaxBytes` (16 MiB), and the RunnerService
door reads `agentmsg.MaxBytes + 1<<20` (`runnerMaxReadBytes`), so
`MaxSessionBlobBytes = MaxBytes - 1<<20` (15 MiB of raw `bytes`) fits both. No
mime type is sent; the block's `mimeType` stays in the JSONL.

### D3 Server: verify, PUT, then index

`Hub.RelaySessionBlob(ctx, runnerID, req)` takes its guard order from
`Hub.CommitConversationFrame` (`go/internal/runnerhub/relay_comms.go`). It adds
`runnerSessionCtx` scoping, used the way `Hub.dropLostSession` uses it.
`CommitConversationFrame` itself does not scope, so do not copy it literally.
Write it as a `//nolint:dupl` mirror, not a shared helper. The order is:

1. No blob store is wired: `CodeUnavailable`.
2. Run `runnerSessionCtx`, then `accountForRunnerSession`. A miss is
   `CodeNotFound`.
3. The data is over `MaxSessionBlobBytes`, or the hash is not
   `^[a-f0-9]{64}$`: `CodeInvalidArgument`.
4. Map store errors:
   - `ErrFailedPrecondition` (no object store) becomes
     `CodeFailedPrecondition`, the "surface absent" code that
     `Handler.FetchAgentConfig` uses.
   - `ErrInvalidArgument` becomes `CodeInvalidArgument`.
   - Anything else becomes `CodeInternal`.

`Store.PutSessionBlob` runs these steps in order:

1. A nil `objectStore` returns `ErrFailedPrecondition`.
2. If `sha256(data)` does not match the claimed hash, return
   `ErrInvalidArgument`.
3. If this session's own index row exists, return nil.
4. `PutSegment("blobs/"+hash, data)`.
5. Insert the index row with `ON CONFLICT DO NOTHING`.

This is the PUT-before-PG order of `flushUpto`
(`go/internal/store/agent_transcripts.go`). A crash between steps 4 and 5 is
repaired by the next attempt's identical PUT.

The index copies `agent_session_archive_segments` (`0001_init.sql`):

```sql
CREATE TABLE agent_session_blobs (
    session_id  TEXT        NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    sha256      TEXT        NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    size_bytes  BIGINT      NOT NULL CHECK (size_bytes > 0),
    tenant_id   TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, sha256)
);
```

The table ships in a new migration numbered with the next free `NNNN` at
rebase. `verifyChecksum` (`go/internal/store/store.go`) refuses an edited
`0001_init.sql`, and `checkContiguous` refuses gaps. Because the `0001` DO
loops ran before this table existed, the migration repeats their work:

- the dynamic `current_schema()` GRANT to `compass_app` and `compass_system`
  (pgtests use per-test schemas);
- `ENABLE` and `FORCE ROW LEVEL SECURITY`;
- the `tenant_isolation` policy;
- the `set_updated_at` trigger.

The object key is global, as ruled, so isolation lives in the index:
`ReadSessionBlob(ctx, sessionID, sha)` checks the session's row under the
tenant transaction before `GetSegment`.

### D4 Resume: the Runner pulls referenced blobs

The Runner pulls blobs inside `agentHost.Start` (`go/internal/runner/host.go`)
on an authorized resume. The steps run in this order:

1. Fetch secrets (unchanged).
2. Scan the body for refs, newest first and deduped.
3. Call `FetchSessionBlobs`.
4. Mark absent images (D5).
5. Write the resume file.
6. Write the blobs.
7. `bindAndStartAgent`.

The new RPC is a server stream, `RunnerService.FetchSessionBlobs`, in the
`FetchAgentConfig` shape: a header frame, then 512 KiB `chunk` frames
(`configChunkBytes`). A `ResumeBody` field would put every image into one
message on the `Sessions` stream and stall it.

Authorization copies `Hub.BindLifetime`
(`go/internal/runnerhub/bind_lifetime.go`):

- `AccountForContainer` must resolve; a miss is `CodePermissionDenied`.
- `AccountTenant` runs under the system role.
- The read runs under `WithTenant` with the session-ownership predicate.

The Server's response order is:

- One `absent` header for each requested hash that has no index row for this
  session.
- Then the indexed blobs, in request order, until the next one would cross
  `maxResumeBlobBytes` (128 MiB) or `maxResumeBlobs` (256).
- A failed `ReadSessionBlob` skips that blob and is logged.

The Runner enforces the same budget and drops any blob whose recomputed SHA-256
does not match its header. `AgentRuntime.WriteAgentFiles`, a sibling of
`WriteAgentFile` (`go/internal/runtime/agent.go`), writes the survivors in one
exec:

- `tar -x` runs as the agent uid under umask 077 into `.omp/agent/blobs`;
- entries are regular 0600 files named by the computed digest;
- `agent-image/toolchain.nix` adds `pkgs.gnutar`, since no tar is linked
  today, and the guest rootfs reuses that image.

Start-latency bound: one stream plus one exec. The fetch runs under
`resumeBlobFetchTimeout` (30 s) and the budget above. A timeout or any
non-`FailedPrecondition` error counts as transient, and Start continues. Only
cancellation of the caller's ctx fails the Start.

`Reload` reuses the container (`agentHost.reloadLocked`), so it fetches
nothing.

### D5 Marker: the Runner rewrites absent images

If nothing rewrites the body, `resolveImageData` (`session/blob-store.ts`) logs
and keeps the ref:

```ts
logger.warn("Blob not found for image reference", { hash });
return data; // Return the ref as-is; downstream will see invalid base64 but won't crash
```

The provider clamp then emits
`[image omitted: undecodable ${mimeType} data (${reason})]`
(`replaceUnreadableContent`, `session/provider-image-budget.ts`). That is the
warning the ruling replaces.

The Runner rewrites the resume body in Go before writing it. It replaces each
`content` image block whose ref is in the **absent set** with
`{"type":"text","text":"[image unavailable after resume]"}`. A hash is absent
only in two cases:

- the Server sent an `absent` header for it;
- the whole fetch returned `FailedPrecondition` (no object store).

Every transient case keeps the ref: a timeout, a stream error, over budget, a
digest mismatch, or a Server read failure. This lifetime sees the SDK's text,
and a later resume can still recover the image.

Marking is skipped when the container has already run an agent, because its
blob dir may hold files that were never uploaded. A per-container flag set in
`bindAndStartAgent` tracks this.

Only lines containing `blob:sha256:` are decoded (`UseNumber`, re-encoded with
`SetEscapeHTML(false)`). Unchanged lines stay byte-identical.

Rejected: an agent-side rewrite before tee init. The agent cannot tell absent
from transient, so a flaky resume would mark stored images and the next
checkpoint would erase their refs. A `context` extension handler is rejected
too, because `emitContext` `structuredClone`s every message on each LLM call.
See OQ-2.

## Global Constraints

- **Size cap.** A message is at most `agentmsg.MaxBytes` (16 MiB). A blob is at
  most `MaxSessionBlobBytes` (15 MiB raw), and TS `MAX_SESSION_BLOB_BYTES`
  matches it. A larger image is skipped, never split.
- **Best-effort.** No blob path fails a turn, a Start, a resume, or the
  transcript lane, and the R4 latch never fires for a blob.
- **No object store.** `CodeFailedPrecondition` means only "surface absent".
  Transient faults are `Unavailable` or `Internal`.
- **Tenant isolation.** `agent_session_blobs` has the standard `ENABLE` +
  `FORCE` RLS policy. Every store call runs under `store.WithTenant`. Reads are
  keyed by (session, hash) and need that session's row.
- **No existence probe.** `PutSessionBlob` never asks whether an object
  exists. Its result and timing depend only on this session's own row.
- **Verified bytes.** The Server and the Runner both recompute SHA-256. File
  names come from the computed digest.
- **No ids in model-facing text.** The marker is the fixed string
  `[image unavailable after resume]`.
- **Storage boundary.** The agent holds no storage credentials (DL-089).
- **SDK.** `^18.0.11`, unpatched. Refs match `blob:sha256:[a-f0-9]{64}`.
- **Schema.** New migrations only, numbered at rebase.
- **Public repo.** Cite only public paths.

## Plan

### T1 proto: blob RPCs (lane proto)

```proto
// agent_gateway.proto, service AgentGateway
rpc PutSessionBlob(PutSessionBlobRequest) returns (PutSessionBlobResponse);
message PutSessionBlobRequest { string sha256 = 1; bytes data = 2; }
message PutSessionBlobResponse {}

// runner.proto, service RunnerService
rpc RelaySessionBlob(RelaySessionBlobRequest) returns (RelaySessionBlobResponse);
message RelaySessionBlobRequest { string session_id = 1; PutSessionBlobRequest blob = 2; }
message RelaySessionBlobResponse {}

rpc FetchSessionBlobs(FetchSessionBlobsRequest) returns (stream FetchSessionBlobsResponse);
message FetchSessionBlobsRequest {
  string container_name = 1;
  string session_id = 2;
  repeated string sha256 = 3; // newest first
}
message FetchSessionBlobsResponse { oneof frame { SessionBlobHeader header = 1; bytes chunk = 2; } }
message SessionBlobHeader { string sha256 = 1; uint64 size_bytes = 2; bool absent = 3; }
```

Also rewrite the `ResumeBody` comment, which says blobs are "OUT of MVP scope
(RIG-1582)", and run `moon run proto:gen`.

- Test: `moon run proto:ci` is green.

### T2 store: index and blob methods (lane compass-server)

```go
// go/internal/agentmsg
const MaxSessionBlobBytes = MaxBytes - 1<<20

// go/internal/store
type SessionBlobRow struct{ SHA256 string; SizeBytes int64 }
func (s *Store) PutSessionBlob(ctx context.Context, sessionID, sha256Hex string, data []byte) error
func (s *Store) ResumeSessionBlobs(ctx context.Context, sessionID string, account AccountID, sha256s []string) ([]SessionBlobRow, error)
func (s *Store) ReadSessionBlob(ctx context.Context, sessionID, sha256Hex string) ([]byte, error) // ErrNotFound without this session's row
```

Add the migration (D3) and `queries/agent_session_blobs.sql` (row exists,
insert, ownership-filtered select), then regenerate sqlc. Add
`agent_session_blobs` to `tenantOwned` in `TestRLSCatalogEnabledAndForced`.

Tests (pgtest with the store's fake `ObjectStore`):

- A nil object store returns `ErrFailedPrecondition`.
- A mismatch causes no PUT and no row.
- A repeat call does not PUT again.
- A row is invisible to another tenant.
- `ReadSessionBlob` for another session's hash returns `ErrNotFound`.

### T3 hub and handler (lane compass-server)

```go
// go/internal/runnerhub
type SessionBlobStore interface {
    AccountTenant(ctx context.Context, account store.AccountID) (store.TenantID, error)
    PutSessionBlob(ctx context.Context, sessionID, sha256Hex string, data []byte) error
    ResumeSessionBlobs(ctx context.Context, sessionID string, account store.AccountID, sha256s []string) ([]store.SessionBlobRow, error)
    ReadSessionBlob(ctx context.Context, sessionID, sha256Hex string) ([]byte, error)
}
func (h *Hub) SetSessionBlobStore(b SessionBlobStore)
func (h *Hub) RelaySessionBlob(ctx context.Context, runnerID string, req *compassv1internal.RelaySessionBlobRequest) (*compassv1internal.RelaySessionBlobResponse, error)
func (h *Hub) FetchSessionBlobs(ctx context.Context, runnerID string, req *compassv1internal.FetchSessionBlobsRequest, send func(*compassv1internal.FetchSessionBlobsResponse) error) error
// Handler.RelaySessionBlob / Handler.FetchSessionBlobs: runnerSubjectFrom guard, delegate.
```

Wire `*store.Store` next to `SetTranscriptStore`.

Tests:

- Every guard maps to its code.
- A pgtest checks that a tenant-B session's blob row is stamped tenant B.
- Absent headers come first, then header and chunks per blob.
- The budget stops before the blob that would cross it.
- A foreign container gets `PermissionDenied`.
- A read failure skips that blob, and the stream still ends OK.

### T4 Runner gateway forwarder (lane compass-runner)

```go
// go/internal/runner/gateway
type SessionBlobRelay interface {
    RelaySessionBlob(ctx context.Context, req *connect.Request[compassv1internal.RelaySessionBlobRequest]) (*connect.Response[compassv1internal.RelaySessionBlobResponse], error)
}
func (g *Gateway) PutSessionBlob(ctx context.Context, req *connect.Request[compassv1internal.PutSessionBlobRequest]) (*connect.Response[compassv1internal.PutSessionBlobResponse], error)
```

Add `Deps.SessionBlobs` and wire it where `Deps.Board` is wired.

Tests:

- An unbound container gets `PermissionDenied`, with no relay call.
- The forwarded request carries the bound session id.
- A Server `FailedPrecondition` passes through unchanged.

### T5 Runner resume fetch and write (lane compass-runner)

```go
// go/internal/runner
type SessionBlob struct{ SHA256 string; Data []byte }
type SessionBlobFetch struct{ Blobs []SessionBlob; Absent map[string]struct{} }
func (l *ServerLink) FetchSessionBlobs(ctx context.Context, containerName, sessionID string, sha256s []string) (SessionBlobFetch, error)
func blobRefsNewestFirst(resumeBody string) []string

// go/internal/runtime
type AgentFile struct{ Name string; Data []byte }
func (r *AgentRuntime) WriteAgentFiles(ctx context.Context, id WorkloadID, uid uint32, homeDir, relDir string, files []AgentFile) error
```

The `FetchSessionBlobs` client follows `ServerLink.FetchAgentConfig`: a chunk
before any header is a contract skew. It enforces the budget, drops digest
mismatches, and runs under `resumeBlobFetchTimeout`. Add `pkgs.gnutar` to
`agent-image/toolchain.nix`. Wire it into `agentHost.Start` in the D4 order.

Tests:

- Blobs land under `.omp/agent/blobs` named by the computed digest, in one
  exec.
- A mismatched blob is dropped.
- A timeout or a mid-stream error still starts the agent.
- A fresh Start never fetches.

### T6 agent capture and upload (lane compass-agent)

```ts
// transport/index.ts, on RunnerTransport
putSessionBlob(req: PutSessionBlobRequest): Promise<void>;
// session-tee.ts
interface TranscriptTeeOptions { readonly onCommittedLine?: (line: string) => void }
// session-blobs.ts
export const MAX_SESSION_BLOB_BYTES = 15 * 1024 * 1024;
export const SESSION_BLOB_QUEUE_MAX = 16;
export interface SessionBlobUploader { offer(line: string): void; close(): Promise<void> }
export function createSessionBlobUploader(deps: {
  put: (req: PutSessionBlobRequest) => Promise<void>;
  blobsDir: string; // getBlobsDir()
}): SessionBlobUploader;
```

Wiring:

- `append` and `writeFull` call `onCommittedLine` after the local write.
- `main` passes `offer` into `createTeeSessionStorage` and closes the uploader
  in the existing drain-then-close `finally`.
- Reserve `PI_CODING_AGENT_DIR` and `XDG_DATA_HOME` in `isReservedEnvKey`.
- Counters follow `transport/otel-metrics.ts`:
  `compass_agent.session_blobs.{uploaded,dropped,oversize,missing,failed}`.

Tests:

- An appended line and a checkpoint body each upload a hash once.
- A `failed` hash is re-offered by a later line, while an uploaded hash is not.
- An oversize blob is counted and marked done.
- ENOENT counts as `missing`, not `failed`.
- Overflow drops the oldest entry.
- `FailedPrecondition` disables the uploader.
- A rejecting `put` or a throwing `offer` never fails `append`.

### T7 Runner unavailable marker (lane compass-runner)

```go
// go/internal/runner
const imageUnavailableText = "[image unavailable after resume]"
func markAbsentBlobs(resumeBody string, absent map[string]struct{}) (string, int)
```

`Start` calls `markAbsentBlobs` between the fetch and the resume-file write. It
skips the call when the container's started flag is set.

Tests:

- An absent content image becomes the marker.
- A transient (unlisted) hash is left alone.
- Unchanged lines are byte-identical, and large numbers survive.
- A reused container is not marked.

### T8 end-to-end resume with an image (lane compass-server e2e)

Extend the `go/e2e` leg that resumes into a fresh container:

- With an object store, the image bytes are back in the new blob dir.
- Without one, the resumed transcript carries the marker.

## Tasks

- [ ] T1 proto: three RPCs, `ResumeBody` comment, regenerate
- [ ] T2 store: migration, queries, sqlc, blob methods, RLS catalog entry
- [ ] T3 hub and handler: relay, fetch, wiring
- [ ] T4 Runner gateway: `PutSessionBlob` forwarder
- [ ] T5 Runner resume: fetch client, `WriteAgentFiles`, gnutar, `Start` wiring
- [ ] T6 agent: tee hook, uploader, transport, reserved env keys, counters
- [ ] T7 Runner: `markAbsentBlobs` and the reused-container skip
- [ ] T8 e2e: image survives with an object store; marker without one

## Open Questions

- **OQ-1, retention and erasure (not load-bearing).** `blobs/<sha256>` keys
  are shared across sessions and tenants. Deleting one, whether for cost or to
  erase a tenant's images at offboarding, needs a system-role reference sweep
  across every tenant's index. This record deletes nothing. A follow-up issue
  should own GC and erasure together.
- **OQ-2, marker placement (load-bearing; Matt's call).** The default is the
  Runner marker (D5). It marks only Server-confirmed absences and keeps
  transient refs recoverable, at the cost of skipping reused containers. The
  alternative is an agent-side rewrite before the tee storage initializes. It
  is simpler and covers reused containers, but it cannot tell absent from
  transient, so one flaky resume durably erases images that are stored. The
  recommendation is the Runner marker.
- **OQ-3, marker coverage (not load-bearing).** Upload covers every ref
  position. The marker rewrites only `content` image blocks. Absent refs in
  `images[]`, snapcompact `frames[]`, `image_url`, and
  `image_generation_call.result` still show the SDK's text. Widen it only if
  those positions show up in real resumes.
- **OQ-4, budget values (not load-bearing).** The defaults are 15 MiB per
  blob, 128 MiB and 256 blobs per resume, a 30 s fetch, and a 16-entry queue.
  They are tuning values.
