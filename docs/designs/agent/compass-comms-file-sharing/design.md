# Compass comms file sharing

## Problem

Issue: RIG-2669. An agent can post channel messages, but it cannot share a file (a diff, a log, a design draft, a screenshot, a data blob) with a channel, a topic, or a DM peer. The operator cannot download one either. Agents today fall back on a shared filesystem path, which Compass does not have.

This record adds file attachments to comms: a file is posted with a message, stored server-side, carried as a block in that message, and read by anyone who can read the message. Agents attach and fetch with two new tools. The operator downloads from the UI and the CLI.

## Approach

An attachment is uploaded first and posted second. Upload stores the bytes and returns an attachment id. The post carries a new `attachment` message block that names the id, and binds the attachment to that one message. After the bind, a read is allowed exactly when the reader can read that message. Before the bind, only the uploader can read it.

The design reuses the session-blob path (DL-426, `docs/designs/agent/compass-agent-session-blobs/design.md`) for bytes, and the live stream's topic-participant probe for access. It adds:

- one index table and a directory-backed `ObjectStore` for local stacks;
- one block arm, two CommsService RPCs and two agent call arms;
- two agent tools, one UI chip, one native save method and two CLI verbs.

### D1 Storage reuses the ObjectStore seam

Bytes go through the existing `ObjectStore` seam under the content-addressed key `blobs/<sha256>`, the same key space as session blobs. Copy the pattern of `Store.PutSessionBlob` in `go/internal/store/agent_session_blobs.go`:

- with no object store it refuses: `ErrFailedPrecondition` "session blob object store is not configured";
- it checks the size and verifies the digest;
- it writes `s.objectStore.PutSegment(ctx, "blobs/"+sha256Hex, data)` before the index row, so a retry repairs a half-write.

`ObjectStore` (`go/internal/store/agent_transcripts.go`) has only `PutSegment` and `GetSegment`. This design needs nothing more.

**Local stacks.** Today `openStore` in `go/server/serve.go` sets an object store only for S3: "When the S3 config is ABSENT (no endpoint/bucket) the seam is left nil". `compass-stack` exposes no S3 flags. `serverSpec` in `go/internal/stack/spec.go` passes S3 env to the server only `if cfg.S3Endpoint != ""`. So the default desktop stack has no object store and could not store an attachment.

This record adds `store.DirObjectStore`, a second implementation of the same two-method seam that writes files under a root directory. `compass-server` gains `--object-dir` / `COMPASS_OBJECT_DIR`, used only when S3 is not configured. Session blobs and the transcript archive flush use the same seam, so they also start to work on a local stack. This is OQ-1 option (b); OQ-1 states what changes if Matt picks another option.

Every launcher of `compass-server` gets storage as follows:

| Launcher | Storage |
| --- | --- |
| `serverSpec` (`go/internal/stack/spec.go`), used by `compass-stack` and so by the compass-app embedded mode | S3 env when `cfg.S3Endpoint != ""`; otherwise T0 adds `--object-dir <StateDir>/objects` |
| e2e fixture (`go/e2e/fixture.go`) | builds a `stack.Config`, so `serverSpec` applies: garage S3 when `fc.objectStore != nil`, otherwise the object dir; the new `WithoutObjectStore()` option sets `cfg.NoObjectStore` for no-store tests |
| devenv dogfood process (`devenv.nix`, `exec "$bin" --socket …`) | passes no S3 today; T0 adds `--object-dir "${config.devenv.state}/compass/objects"` |
| flake boot check (`tools/flake-gate/compass-server-secretspec.nix`) | none; it only checks that boot resolves secrets, and it uploads nothing |
| a self-hosted release binary run by hand | the operator passes S3 flags or `--object-dir`; with neither, upload is FailedPrecondition |

The index is a new table, `comms_attachments`. `agent_session_blobs` cannot carry it: its key is `(session_id, sha256)`, and `session_id REFERENCES agent_sessions ON DELETE RESTRICT`. A UI or CLI upload has no session. The new table:

```sql
CREATE TABLE comms_attachments (
    id                  TEXT        PRIMARY KEY,
    uploader_account_id TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    message_id          TEXT        REFERENCES messages (id) ON DELETE RESTRICT,
    sha256              TEXT        NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    size_bytes          BIGINT      NOT NULL CHECK (size_bytes > 0),
    content_type        TEXT        NOT NULL CHECK (content_type <> '' AND length(content_type) <= 255),
    filename            TEXT        NOT NULL CHECK (filename <> '' AND octet_length(filename) <= 255),
    tenant_id           TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Serves the unbound-upload bound (D4) and a later GC sweep (OQ-2).
CREATE INDEX comms_attachments_unbound_idx ON comms_attachments (uploader_account_id, created_at)
    WHERE message_id IS NULL;
```

- `id` is server-minted (the 0001 convention: "text ids are server-assigned").
- `message_id` is NULL until a post binds it.
- The table joins the RLS `tenant_tables` loop and the `set_updated_at` trigger list, as `agent_session_blobs` does.
- `size_bytes > 0` means an empty file cannot be shared. Upload returns InvalidArgument "empty file".

With no object store at all (no S3 and no `--object-dir`), `UploadAttachment` returns FailedPrecondition and writes nothing. This is the DL-426 rule.

### D2 The wire is an upload RPC and a block arm

`CommsService` gains two unary RPCs. The UI and the CLI call both, so both are on the public `@compass/client` surface:

- `UploadAttachment(UploadAttachmentRequest) returns (UploadAttachmentResponse)`. The request carries `filename`, `content_type`, `bytes data` and an optional `sha256`. The server always hashes the bytes. When `sha256` is set, it must match, else InvalidArgument. The response carries the stored `AttachmentBlock`.
- `GetAttachment(GetAttachmentRequest) returns (GetAttachmentResponse)`. The request carries `attachment_id`. The response carries the `AttachmentBlock` and `bytes data`.

Upload rejects these with InvalidArgument:

- a filename that is not valid UTF-8, is longer than 255 bytes, contains `/`, `\`, NUL or another control or format character, is `.` or `..`, or starts with a dot;
- a content type that `mime.ParseMediaType` rejects. The store keeps only the lowercase media type (`type/subtype`, parameters dropped), at most 255 bytes;
- an empty file or one over `MaxAttachmentBytes`.

These rules live in a new leaf package, `go/internal/attachment` (`ValidFilename`, `NormalizeContentType`). The store applies them on upload, and `cmd/compass` applies the filename rule before it writes a file. The CLI must not import the store, so the rules need a package of their own. The package holds no state.

`MessageBlock` today is:

```proto
oneof block { string text = 1; Ask ask = 2; AskAnswerBlock ask_answer = 3; }
```

It gains `AttachmentBlock attachment = 4`, named like `AskAnswerBlock`. The field number collides with nothing. No open PR touches this oneof. The `= 4` fields of #2078 (`Ask.ref`) and #2139 (`Ask.cancelled`) are on `Ask`. `AttachmentBlock` is `{ string attachment_id = 1; string filename = 2; string content_type = 3; int64 size_bytes = 4; string sha256 = 5; }`.

**Fill, insert, bind.** A post may send only `attachment_id`. Inside the `AppendMessage` transaction, in this order:

1. **Fill.** For each attachment block, the store runs `SELECT … FROM comms_attachments WHERE id = $att AND uploader_account_id = $author`. If no row matches, the post fails with InvalidArgument "unknown attachment". Otherwise the server copies filename, content type, size and digest from the row and ignores the client's values. This step has no `message_id` predicate, so an idempotent replay passes it.
2. **Insert.** `insertMessageTx` serializes the filled blocks (`marshalBlocks`) and inserts the row. On the `client_request_id` conflict (`errMessageInsertConflict`), `AppendMessage` returns the stored message and skips step 3.
3. **Bind, on a fresh insert only.** The store runs `UPDATE comms_attachments SET message_id = $msg WHERE id = $att AND uploader_account_id = $author AND message_id IS NULL`. If no row is updated, the post fails with InvalidArgument "attachment already posted" and the transaction rolls back.

The bind is the single gate. It has two consequences:

- Nobody can post an attachment id they did not upload. Without this, a reader could re-post another account's attachment into a channel it controls and so widen the read grant.
- An attachment belongs to exactly one message. Sharing the same file again means a new upload. The bytes de-duplicate under the content-addressed key, so the cost is one row.

A held post (#2120) or a stale-post refusal (#2136) inserts nothing. The row stays unbound, and a later confirm or re-post binds it.

Fill and bind run inside `AppendMessage`, and fill rewrites the blocks in memory before `insertMessageTx`. So the record adds no block writer. #2139's `TestNoNewMessageBlockWriters` allowlists "exactly `AppendMessage`" among `*Store` methods that take a block-bearing type, and "exactly `InsertMessage` and `UpdateMessageBlocks`" among `db.Querier` methods with a `Blocks` param field. The new store methods take no `Message`, `MessageBlock`, `Ask` or `AskAnswerBlock` parameter, and no new sqlc params struct has a `Blocks` field. If #2139 has landed, the test stays green with both allowlists unchanged. An implementation that seems to need a new block-taking method is out of this record's scope. #2139 states that "a new block-rewrite path needs its own design record".

**Write-path edges** in `go/internal/comms/mapping.go`:

- `blocksFromWire` accepts the arm on POST.
- `blocksToWire` emits it.
- The update edge depends on #2139, which "makes asks immutable by deleting the dead author-update path (only tests call it)". If #2139 lands first, there is no update edge to change. If it has not landed, `CommitAgentUpdate` checks attachment blocks the way `reconcileUpdateAskIDs` checks asks. That function's premise is "a streaming turn re-sends its FULL current block set in stable order". The frame's attachment ids must equal the stored row's attachment ids, in order, or the update is InvalidArgument. So an update cannot add, drop or change an attachment, and a bound row always matches its message's blocks.

In `go/internal/store/blocks.go`, `marshalBlocks` gains a `blockKindAttachment` case and a `storedAttachment` JSONB shape. `textContent` appends the filename, so `SearchMessages` finds a message by attachment name. Three comments still say the oneof is narrowed to text + ask, and they are updated:

- the `blocks.go` header;
- the `messages` comment in `0001_init.sql`;
- the `ConvBlock` doc in `apps/ui/src/comms-stub.ts` ("text + ask only").

The agent call envelopes in `proto/compass/v1/agent_gateway.proto` gain `upload_attachment = 16` and `get_attachment = 17` on both `CommsCallRequest.call` and `CommsCallResult.result`. Arms 13 and 14 belong to #2078 (`update_board`, `boards`). #2139's `cancel_ask = 13` collides with #2078 and is expected to move to 15. This record takes 16 and 17. Proto numbers are never renumbered down.

### D3 Read access is message read access

`Store.ReadCommsAttachment` runs three steps under tenant RLS:

1. Load the row by id, with its message's `topic_id` (LEFT JOIN `messages`).
2. Authorize. An unbound row is readable only by its uploader. A bound row is readable when `Store.IsTopicChannelMember(actor, topic_id)` is true. The uploader has no extra right to a bound row.
3. Only then call `GetSegment("blobs/"+sha256)`.

`IsTopicChannelMember` is the probe the live stream uses to deliver the message that carries the block. In `go/internal/comms/subscribe.go`: `case *compassv1.SubscribeCommsResponse_MessagePosted: return vis.IsTopicChannelMember(ctx, actor, p.MessagePosted.GetMessage().GetTopicId())`. Its query, `TopicChannelParticipant`, is "ChannelParticipant on the channel that owns the topic". The `messages.sql` header requires every `participating` CTE to be "equal to ChannelParticipant (channels.sql)". So an attachment is readable where its block was delivered, and the record adds no new copy of that CTE.

No test checks that those copies agree, so T1 adds a parity pgtest: for each reader, attachment readability equals `ListMessages` visibility of the bound message. It covers four readers:

- a member row;
- a TREE-derived participant;
- an owner-set non-member (DL-345 grants the channel row, not history, so both must be false);
- a removed member.

A future change to the message-read predicate must keep `TopicChannelParticipant` in step. The parity test fails if they drift.

A DM is a two-member channel, so a DM grant is a member row and needs no special case.

An unknown id, an unbound id that belongs to another account, and an id the caller cannot read all return the same NotFound. This is the D9 not-found/forbidden merge, so a refusal reveals nothing. The row lookup runs under RLS before `GetSegment`, as `ReadSessionBlob` does ("a read needs this session's row first"). Knowing a `sha256` from another tenant therefore reads nothing.

### D4 Size cap and retention

The cap is `agentmsg.MaxAttachmentBytes = MaxBytes - 1<<20` (15 MiB), the same value as `MaxSessionBlobBytes`. Every relay and network hop already admits a 16 MiB message:

- `siblingServiceMaxReadBytes = 16 << 20` on the network door's Comms handler;
- `maxAgentMessageBytes = agentmsg.MaxBytes` on the Runner gateway;
- `runnerMaxReadBytes = agentmsg.MaxBytes + 1<<20` in `go/internal/runnerhub/handler.go`, which T2 moves to `agentmsg.RunnerMaxReadBytes`.

1 MiB of headroom covers the envelope.

The socket and dev Comms mount in `go/server/serve.go` sets no read cap: `compassv1connect.NewCommsServiceHandler(commsSvc, connect.WithInterceptors(otelIC, otel.NewTraceResponseInterceptor()))`. connect-go decodes the whole request before the handler runs, so a store check cannot protect memory there. T2 adds `connect.WithReadMaxBytes(siblingServiceMaxReadBytes)` to that mount, which serves both the socket and dev muxes. It also updates the constant's comment, which today says these mounts carry only small messages. The store check stays as the semantic cap. The agent mirrors the cap as `MAX_ATTACHMENT_BYTES = 15 * 1024 * 1024` and refuses before it sends.

One file per upload, unary bytes, no streaming, no chunking. This keeps `S3ObjectStore.GetSegment`, which reads the whole object into memory, within a known bound.

**Body-read deadline.** The network door cuts any request body that takes longer than `networkBodyReadTimeout = 30 * time.Second` to arrive. Only the two Runner streams in `bodyDeadlineExempt` are exempt, because the comment assumes every other RPC "carries a small control-plane protobuf". At 30 s, a 15 MiB upload needs about 4 Mbit/s, so a legal upload from a slow link would fail mid-body.

T2 gives three paths a size-scaled deadline: `CommsService/UploadAttachment`; `RunnerService/RelayCommsCall`, which carries an agent's upload from the Runner; and `RunnerService/RelaySessionBlob`, which carries the same 15 MiB size.

- The deadline is 30 s plus 1 s per 64 KiB of the request's `Content-Length`, never above 10 minutes.
- When the length is unknown or above the path's read cap, it uses the read cap: 16 MiB gives 286 s, and 17 MiB (`agentmsg.RunnerMaxReadBytes`) gives 302 s.
- A small call on any of the three paths keeps about 30 s. After the first 30 s, the effective floor is 64 KiB/s (512 kbit/s).

Why not state 30 s as a limit: 15 MiB is a legal size, so a legal upload should not depend on link speed above a stated floor. Why not a full exemption: the exempt-set comment records that an unauthenticated drip on an exempt path has no deadline at all. A scaled deadline keeps that window bounded.

The residual risk is the same class as the RIG-1298 follow-up in that comment. The deadline arms before auth, so an unauthenticated client can hold one stream on these paths for up to about 5 minutes. For `RelaySessionBlob` the server entry only bounds that drip: the agent's `SESSION_BLOB_PUT_TIMEOUT_MS` (30 s) rides the request context through the Runner gateway, so a legitimate session-blob upload stays capped at 30 s end to end. This record leaves that client bound unchanged; attachment relays set no client timeout, so they get the full scaled deadline.

**Unbound-upload bound.** Every failed post after a successful upload leaves an unbound row and an object. Examples: a topic name-miss, a #2120 hold that is never confirmed, a #2136 stale refusal, a non-member channel. The per-file cap does not bound the total. So:

- `UploadAttachment` returns ResourceExhausted when the uploader already holds `maxUnboundAttachments` (8) unbound rows created within `unboundAttachmentWindow` (24 h).
- The partial index serves the count.
- The count runs after `pg_advisory_xact_lock(hashtext('attachments:' || uploader))`, as in `coordination.sql` (`SELECT pg_advisory_xact_lock(hashtext('coordination:' || $1));`), so concurrent uploads cannot pass the bound together.

This limits unbound uploads to about 120 MiB per account per day. It is a capability bound, not a quota. The agent also reuses an unbound id on retry (D5).

Retention: an attachment lives as long as its message. Nothing deletes messages today, and `ObjectStore` has no delete. Garbage collection of old unbound rows, and erasure, are deferred together, as DL-426's OQ-1 deferred them ("`blobs/<sha256>` keys are shared across sessions and tenants. Deleting one … needs a system-role reference sweep"). See OQ-2.

Per-tenant storage quotas and billing are product policy and are out of scope here (`docs/concepts/self-host-and-managed.md`, `docs/designs/meta/oss-core-managed-boundary/design.md`).

### D5 Agent tools attach and read

`createCommsTools` in `packages/compass-agent/src/comms.ts` gains a `workdir` parameter. `cli.ts` passes its resolved `cwd` (`env.COMPASS_WORKDIR?.trim() || process.cwd()`). It defaults to `process.cwd()`. Two tools join the set. #2120 adds two more, so the count is two above whatever has landed.

`comms_attach_file` (`approval: "write"`) takes `{ path, channel, topic, text?, create_topic?, content_type? }`:

- It resolves a relative `path` against `workdir`. It reads the file, refuses an empty file or one above `MAX_ATTACHMENT_BYTES`, and hashes it.
- **Id reuse.** A session-local map goes from (absolute path, sha256) to an unbound attachment id, with at most 32 entries (oldest evicted). On a hit, the tool skips the upload. The entry is removed when its post succeeds. If the server reports a cached id as "attachment already posted", the tool drops the entry and uploads once more.
- On a miss, it sends `upload_attachment`. Then it sends the existing `post` arm. With `text` set, the blocks are `[text, attachment]` in that order; otherwise `[attachment]`.
- The caption is an ordinary text block, so it behaves like any post's text. `mentionHandles` in `go/internal/delivery/consumer.go` parses only `b.GetText()`, so a mention in the caption routes and the filename never mentions anyone. #2120's length cap counts text bytes through `textContent` in `go/internal/store/blocks.go` ("Text bytes cover text blocks and ask question and option text (`textContent` …)"). D2 appends the filename to `textContent`, so the cap counts the caption plus at most 255 filename bytes, and never the file's bytes.
- The post step uses the same result handling as `comms_post_message`. A #2120 held result stores the request, with the attachment id, under a confirm handle. `compass_message_confirm` re-sends it and binds the same row. A #2136 stale refusal renders the unseen messages and never retries on its own. Without this, a hold reads as "protocol violation — post result carried no message".
- Post keeps its rules: `channel` and `topic` are required names, there is no home default, and a new topic needs `create_topic: true`.
- `content_type` defaults from the file extension, else `application/octet-stream`.

`comms_read_file` (`approval: "read"`) takes `{ attachment_id, save_as? }` and sends `get_attachment`. It returns:

- an image part only when the bytes are at most 5 MiB and their magic bytes match PNG, JPEG, GIF or WebP. The declared type alone never selects an image part;
- text when the bytes are valid UTF-8 and at most 32 KiB;
- otherwise, or when `save_as` is set, it writes the bytes to disk and returns the path. For UTF-8 text over 32 KiB it also returns the first 8 KiB as an excerpt.

The default save path is `<os.tmpdir()>/compass-attachments/<attachment_id>/<name>`, so nothing lands in the git workdir. `<name>` is the filename if it passes the server's filename rule, else `attachment`. `save_as` resolves against `workdir`.

Today both agent renderers drop unknown block arms (`return [];`), so an agent would not know a file arrived. Each gains an attachment marker line:

- The `comms_list_messages` renderer follows its rule "THE MARKERS CARRY THE FENCE too". The marker is `[attachment ${fence}] id=${attr(id, fence)} type=${flat(contentType)} size=${sizeBytes} name: ${flat(filename)}`. Free text comes last, as in `` `[ask ${fence}] ${text}` ``. A message body cannot forge the marker without the per-render fence.
- `formatDeliversForPrompt` in `agent.ts` has no fence, so it prints the same marker without one. A forged marker in a delivered text body is accepted there. This is a known limit of that renderer.

On the Server, `CommsCaller` (`go/internal/runnerhub/hub.go`) gains `UploadAttachmentAsAccount` and `GetAttachmentAsAccount`, and `executeCall` (`relay_comms.go`) gains the two cases. They run under the session-resolved account, like every other arm.

### D6 The operator downloads from the UI and the CLI

UI:

- `ConvBlock` (`apps/ui/src/comms-stub.ts`) gains `{ kind: "attachment"; attachment: AttachmentRef }`, and `adaptBlock` (`apps/ui/src/live/adapt.ts`) maps it.
- `ChannelView` renders a chip with filename, type and size as text, and a Download action. This record renders no inline preview.
- **Browser.** The UI calls `GetAttachment` and always builds the `Blob` as `application/octet-stream`, never the declared type. It clicks a temporary `<a download>` and revokes the object URL right after. It never navigates to or opens a blob URL. A blob URL has the app's origin, so an uploaded `text/html` or `image/svg+xml` opened in place would run script in that origin.
- **Native.** The WebView has no download support. The compass-app shell gains `fileService.SaveAttachment(attachmentID) (string, error)`.
  - It fetches with `GetAttachment` over the bridge's current connection target (`Target.Client()` "exposes the target's underlying *http.Client and base URL"). The bytes go from Go to disk and never cross the JS bridge.
  - Embedded mode has no target today: `launch` in `main.go` builds `&connection{mode: cfg.Mode.String(), pump: bridge.NewPump(bridge.NewUnixTarget(socket))}`. T5 keeps `t := bridge.NewUnixTarget(socket)` and stores it as `target: t, pump: bridge.NewPump(t)`. `Connect` stays unavailable in embedded mode: its guard is `conn.target == nil || s.tokens == nil`, and embedded passes nil tokens (`newBridgeService(conn, nil, nil)`).
  - It then opens `app.Dialog.SaveFile()` with the filename and writes the file. It returns `""` when the user cancels.
  - It is registered in every non-setup mode, embedded and client. Today `main.go` adds extra services only `if setup != nil`, and embedded is the default desktop setup.
  - Its file carries the build tag of `dialog_service.go`, `//go:build (linux && gtk4) || darwin`.

The UI sends no upload in this record (see OQ-3).

CLI, under `newMessageCmd` in `go/cmd/compass/message.go`:

- `compass message attach --channel <id> --topic <name> <path>` uploads and posts. It sets `CreateTopic` like `message post`, sends no `ClientRequestId` (same reason as `message post`), and prints the message id and the attachment id.
- `compass message get <attachment-id> [--output <path>|-]` downloads.
  - The default output is `./<filename>`, used only when the filename passes `attachment.ValidFilename`. That rule rejects a leading dot, so a poster cannot choose a name like `.envrc`. When the name fails, the command refuses and asks for `--output`.
  - It never overwrites an existing file.
  - `-` writes to stdout.

`AdminGate` (`go/internal/auth/admin_gate.go`) lists both RPCs with the other CommsService methods as `authenticatedOpen{}` ("the service body enforces per-account authorization"). `TestClassifyProcedureCoversEveryGeneratedProcedure` fails until they are listed.

## Alternatives considered

- **Reuse `agent_session_blobs`.** Rejected. Its key and FK are per session, and a UI or CLI upload has no session.
- **Inline bytes in the message block (JSONB).** Rejected. 15 MiB rows would bloat `messages`, every `ListMessages` page and every `SubscribeComms` frame.
- **One post that carries the bytes** (a new `PostMessageRequest` field, number 12 or later). This option has no unbound rows and no bind race. Rejected: #2120's hold-and-confirm and #2136's stale refusal both re-send the post, so every re-send would carry up to 15 MiB again. With two phases, a re-sent post binds the same id, and a later UI upload (OQ-3) stays cheap.
- **Presigned object-store URLs.** Rejected. The agent holds no object-store credentials and has no egress path to the store (DL-084 superseded agent-direct S3, DL-063/DL-083). A browser URL would also bypass the participation check.
- **A new `chain` + `participating` CTE copy for the read** (`GetAttachmentForReader` in `messages.sql`). Rejected. It is one more hand-synced copy, and nothing enforces that the copies match. The stream's probe already answers the question.
- **Read authz by JSONB containment** (`blocks @> …`, the `FindAskMessage` shape). Rejected. There is no index on `messages.blocks`, and an attachment could be referenced from many messages.
- **A Postgres `bytea` store on local stacks** (OQ-1 option (c)). Not chosen. It grows the database by every file and helps only attachments.
- **A streaming upload RPC.** Deferred. Unary within the existing 16 MiB caps needs no new transport. Larger files are OQ-4.

## Plan

### Global Constraints

- **Public repo:** no private-monorepo names or paths (`rule://private-repo-boundary`). Quotas and billing stay out (D4).
- **Proto:**
  - Changes are additive only: `MessageBlock.attachment = 4` (`AttachmentBlock`), agent arms 16 and 17. Never renumber down. `buf lint` and `buf breaking` stay green.
  - No proto change lands without its handler, and there are no Unimplemented stubs. `Comms` asserts `var _ compassv1connect.CommsServiceHandler = (*Comms)(nil)`.
- **Cap:** `agentmsg.MaxAttachmentBytes` (15 MiB) is the one Go source of truth. The agent constant mirrors it with a comment that names the Go constant.
- **Errors:**
  - no object store → FailedPrecondition;
  - over the cap, empty, bad digest, bad filename or content type, unknown or already-posted attachment → InvalidArgument;
  - unbound-upload bound → ResourceExhausted;
  - any unreadable attachment → NotFound (the D9 merge).
- **Schema:**
  - Edit `go/internal/store/migrations/0001_init.sql` in place, as the session-blob table did ("Schema changes require a dev DB reset").
  - Place `comms_attachments` after `messages` ("Statement order is FK order").
  - Add it to the RLS loop and the trigger list.
- **Authz:** the read uses `IsTopicChannelMember`. Add no new `participating` CTE copy.
- **Untrusted text:**
  - Filename and content type pass `flat`/`attr` in every agent renderer.
  - The list marker carries the fence.
  - The UI renders both as text, downloads as `application/octet-stream`, and never opens a blob URL.
- **Attach post:** it uses the same held and stale handling as `comms_post_message` (#2120, #2136).
- **Tests:**
  - Red → green.
  - Postgres tests use the existing pgtest harness.
  - The S3 path uses the garage e2e fixture (`go/e2e/fixture.go`, `cfg.S3Endpoint = garage.endpoint`).

### Ordering

- **Within this record:** T0 → T1 → T2 → T3 → T4. T5 and T6 need T2. T7 runs last.
- **#2120 and #2136:** T4 lands after both merge. It reuses their held and stale handling for the attach post. The tool count is two above whatever has landed.
- **#2139:** if it merges before T2, T2 drops the update-edge check (the path is gone). If not, T2 adds it (D2). Either way, T1 keeps `TestNoNewMessageBlockWriters` green with its allowlists unchanged (D2).
- **#2078 and #2139:** they own agent arms 13–15. This record's 16 and 17 do not depend on their order.

### T0 Directory ObjectStore

Add `store.DirObjectStore`, the `--object-dir` flag, the `openStore` branch, the `serverSpec` argument, and the devenv.nix argument (D1).

- Keys map to paths under the root.
- A key that is absolute or contains `..` is an error.
- `PutSegment` writes a temp file in the target directory, then renames it, so a reader never sees a partial file.
- S3 wins when both S3 and the object dir are configured.
- `stack.Config.NoObjectStore` suppresses the default, so tests can still run a server with no store. The existing `TestLegSessionImageResume/without_object_store` moves to the fixture's `WithoutObjectStore()` option; without that it would start persisting blobs and fail.

Interfaces:

```go
// store
func NewDirObjectStore(root string) (*DirObjectStore, error) // creates root 0700
func (d *DirObjectStore) PutSegment(ctx context.Context, key string, body []byte) error
func (d *DirObjectStore) GetSegment(ctx context.Context, key string) ([]byte, error)
// server: ServeConfig.ObjectDir string — --object-dir / COMPASS_OBJECT_DIR; ignored when S3 is set
// stack: Config.NoObjectStore bool; serverSpec appends "--object-dir", filepath.Join(cfg.StateDir, "objects") when cfg.S3Endpoint == "" && !cfg.NoObjectStore
// e2e: func WithoutObjectStore() fixtureOption — sets cfg.NoObjectStore
// devenv.nix compass-server exec: --object-dir "${config.devenv.state}/compass/objects"
```

Tests:

- a put and get round trip;
- a traversal key is rejected;
- putting the same key twice returns the same bytes;
- `openStore` prefers S3;
- `serverSpec` adds the flag only without S3;
- the devenv dogfood stack uploads and reads an attachment (a manual smoke check, as devenv has no CI lane).

### T1 Store

Add:

- the `comms_attachments` table, its index, and its RLS and trigger entries;
- `comms_attachments` in `tenantOwned` in `TestRLSCatalogEnabledAndForced`;
- the `go/internal/attachment` rules;
- sqlc queries for insert, fill, bind, the unbound count and the read row;
- the block kind and the `textContent` case;
- fill, insert and bind in `AppendMessage` (D2);
- the unbound bound (D4);
- `ReadCommsAttachment` (D3).

Interfaces:

```go
// agentmsg
const MaxAttachmentBytes = MaxBytes - 1<<20
// attachment (new leaf package)
func ValidFilename(name string) error
func NormalizeContentType(ct string) (string, error)
// store
var ErrResourceExhausted = errors.New("store: resource exhausted")
const maxUnboundAttachments = 8
const unboundAttachmentWindow = 24 * time.Hour
type AttachmentRef struct { ID, Filename, ContentType, SHA256 string; SizeBytes int64 }
// MessageBlock gains: Attachment *AttachmentRef
func (s *Store) PutCommsAttachment(ctx context.Context, uploader AccountID, filename, contentType, sha256Hex string, data []byte) (AttachmentRef, error) // sha256Hex "" = not checked
func (s *Store) ReadCommsAttachment(ctx context.Context, actor AccountID, id string) (AttachmentRef, []byte, error)
func (s *Store) MessageAttachmentIDs(ctx context.Context, actor AccountID, id MessageID) ([]string, error) // only while the author-update path exists (#2139)
```

Tests (pgtest):

- **Reads:**
  - put, then read as the uploader before the post;
  - after the post, a member reads it; a non-member gets NotFound, and so does an uploader who left the channel.
- **Parity:** the D3 parity test across its four readers.
- **Bind:**
  - posting another account's id → InvalidArgument;
  - a second bind of the same id → InvalidArgument, and the post rolls back.
- **Replay:** post with an attachment and a `client_request_id`, then replay the same request. It returns the same message and does not fail.
- **Validation:** over the cap, empty, a bad digest, a bad filename (`..`, `.envrc`, `a/b`, NUL, 256 bytes) or a bad content type → InvalidArgument.
- **Unbound bound:** the 9th unbound upload in the window → ResourceExhausted; binding one frees a slot.
- **Setup:**
  - a nil object store → FailedPrecondition;
  - the row is stamped with the caller's tenant.
- **#2139 pin:** if #2139 has landed, `TestNoNewMessageBlockWriters` passes with its allowlists unchanged.

### T2 Proto and comms service

Make the `comms.proto` changes and regenerate the Go and public TS outputs. Add:

- the two handlers and the two `*AsAccount` methods (in `go/internal/comms/agent_caller.go`);
- the `blocksFromWire` and `blocksToWire` cases, and the update-edge check unless #2139 has landed;
- an `edgeError` case `store.ErrResourceExhausted → connect.CodeResourceExhausted`;
- the read cap on the socket and dev Comms mount in `serve.go` (D4);
- the size-scaled body deadline in `go/server/network_door.go` (D4), and the move of `runnerMaxReadBytes` to `agentmsg.RunnerMaxReadBytes`, including its use in `go/internal/runnerhub/read_cap_test.go`;
- the `AdminGate` entries.

Interfaces:

```proto
message AttachmentBlock { string attachment_id = 1; string filename = 2; string content_type = 3; int64 size_bytes = 4; string sha256 = 5; }
// MessageBlock.block: AttachmentBlock attachment = 4;
message UploadAttachmentRequest { string filename = 1; string content_type = 2; bytes data = 3; string sha256 = 4; } // sha256 optional
message UploadAttachmentResponse { AttachmentBlock attachment = 1; }
message GetAttachmentRequest { string attachment_id = 1; }
message GetAttachmentResponse { AttachmentBlock attachment = 1; bytes data = 2; }
rpc UploadAttachment(UploadAttachmentRequest) returns (UploadAttachmentResponse);
rpc GetAttachment(GetAttachmentRequest) returns (GetAttachmentResponse);
```

```go
func (c *Comms) UploadAttachment(ctx context.Context, req *connect.Request[compassv1.UploadAttachmentRequest]) (*connect.Response[compassv1.UploadAttachmentResponse], error)
func (c *Comms) GetAttachment(ctx context.Context, req *connect.Request[compassv1.GetAttachmentRequest]) (*connect.Response[compassv1.GetAttachmentResponse], error)
func (c *Comms) UploadAttachmentAsAccount(ctx context.Context, account store.AccountID, req *compassv1.UploadAttachmentRequest) (*compassv1.UploadAttachmentResponse, error)
func (c *Comms) GetAttachmentAsAccount(ctx context.Context, account store.AccountID, req *compassv1.GetAttachmentRequest) (*compassv1.GetAttachmentResponse, error)
```

```go
// go/server/network_door.go
const maxScaledBodyReadTimeout = 10 * time.Minute
// path → read cap in bytes: UploadAttachment → siblingServiceMaxReadBytes; RelayCommsCall and RelaySessionBlob → agentmsg.RunnerMaxReadBytes
// (runnerhub's runnerMaxReadBytes moves to agentmsg.RunnerMaxReadBytes so both read one constant)
var bodyDeadlineScaled map[string]int64
func bodyReadTimeout(path string, contentLength int64, base time.Duration) time.Duration
```

Tests:

- client-supplied size or type is replaced by the row's values;
- while the update path exists: an update that adds, drops or changes an attachment id → InvalidArgument, and an unchanged set passes;
- `TestClassifyProcedureCoversEveryGeneratedProcedure` is green;
- a socket-door test, like `TestRunnerDoorCapsInboundMessageSize`, shows that a 17 MiB `UploadAttachment` is refused before the handler runs;
- `bodyReadTimeout` table: a non-scaled path gets the base; a scaled path gets base + 1 s per 64 KiB; unknown length uses the cap; the result never exceeds 10 minutes;
- a drip test in `network_door_deadline_test.go`, with a 200 ms base: a 1 MiB body dripped over 2 s completes on `UploadAttachment` and is cut on a non-scaled path;
- a post of `[text "@peer see this", attachment named "@other.log"]` routes a mention to `peer` only.

### T3 Runner relay

Add the two arms to `agent_gateway.proto` and regenerate the internal Go and agent TS outputs. Then update these sites:

- the two methods on the `CommsCaller` interface, and its `nolint` arm list;
- the two `executeCall` cases and its "no recognized variant" error text;
- the `commsArmCases` rows in `relay_arm_coverage_test.go`;
- the two methods on `fakeCommsCaller` (`helpers_test.go`).

Interfaces: the `CommsCaller` methods match the T2 `*AsAccount` signatures.

Tests:

- `requireEveryArmCovered` is green;
- a 15 MiB upload passes through `executeCall` within `agentmsg.RunnerMaxReadBytes`.

### T4 Agent tools and renderers

Add the two tools, the id-reuse map, the held and stale handling, the sniffer and the two markers (D5). Wire `workdir` from `cli.ts`. Update the sites that name the tool set:

- the "exactly the twelve comms tools" test in `comms.test.ts`;
- `packages/compass-agent/AGENTS.md`;
- the "all twelve native comms tools" comment in `transport/index.ts`;
- the read and write lists in `docs/concepts/tools.md`.

Interfaces:

```ts
export function createCommsTools(broker: CommsBroker, turnSequence?: TurnSequence, workdir?: string): AgentTool[];
export const attachFileParameters = type({ path: "string", channel: "string", topic: "string", "text?": "string", "create_topic?": "boolean", "content_type?": "string" });
export const readFileParameters = type({ attachment_id: "string", "save_as?": "string" });
export const MAX_ATTACHMENT_BYTES = 15 * 1024 * 1024; // mirrors agentmsg.MaxAttachmentBytes
export function sniffRasterImage(data: Uint8Array): "image/png" | "image/jpeg" | "image/gif" | "image/webp" | undefined;
```

Tests (bun:test, fake broker):

- **Attach:**
  - it sends upload, then post with the returned id;
  - with `text`, the post's blocks are `[text, attachment]` in that order;
  - an oversize or empty file fails before any call;
  - a retry after a failed post reuses the id and does not upload again;
  - a held result stores the request with the attachment id.
- **Read:**
  - PNG bytes return an image part;
  - bytes declared `image/png` that are not PNG return a saved path;
  - UTF-8 text up to 32 KiB returns inline text; larger returns an excerpt and a path;
  - a `..` filename saves as `attachment`.
- **Markers:**
  - a text body that spells the marker without the fence does not render as an attachment;
  - a newline in a filename stays on one line.

### T5 UI and native save

Add:

- the `ConvBlock` kind and the `adaptBlock` case;
- the `ChannelView` chip;
- the browser download helper;
- the native call on the native boot path;
- `fileService` in `go/cmd/compass-app`, registered in every non-setup mode, with `app` set after `application.New` as `dialog.app` is;
- `target: t` on the embedded connection in `launch` (D6).

Interfaces:

```ts
interface AttachmentRef { id: string; filename: string; contentType: string; sizeBytes: number }
// ConvBlock: | { kind: "attachment"; attachment: AttachmentRef }
export async function downloadAttachment(client: CommsClient, ref: AttachmentRef): Promise<void>; // browser path
```

```go
type fileService struct { app *application.App; bridge *bridgeService }
func (f *fileService) SaveAttachment(ctx context.Context, attachmentID string) (string, error) // "" when the user cancels
```

Tests:

- an adapt test for the new kind;
- a component test that the chip renders;
- a `text/html` attachment downloads as a `Blob` of type `application/octet-stream`, with no navigation;
- a Go test for `SaveAttachment` fetches through a fake target and writes 15 MiB through an injected dialog;
- a Go test for `SaveAttachment` on an embedded-mode connection, built as `launch` builds it, against an h2c test server on a Unix socket;
- in that embedded test, `Connect` still returns "Connect is not available";
- a cancel returns `""`.

### T6 CLI verbs

Add `newMessageAttachCmd` and `newMessageGetCmd` under `newMessageCmd`, and change its `Short` to "(post, attach, get)".

Interfaces:

```go
func runMessageAttach(ctx context.Context, client compassv1connect.CommsServiceClient, args messageAttachArgs, out io.Writer) error
func runMessageGet(ctx context.Context, client compassv1connect.CommsServiceClient, id, output string, out io.Writer) error
```

Tests, with a fake client:

- attach sends upload, then post with the block;
- get with a `.envrc` filename refuses without `--output`;
- get never overwrites;
- `-` writes to stdout.

### T7 End to end

An e2e test on the garage fixture:

- An agent runs `comms_attach_file` into a channel.
- A member operator runs `compass message get` and gets the same bytes.
- A non-member gets NotFound.
- A server started with `WithoutObjectStore()` returns FailedPrecondition on upload.

## Tasks

- [ ] T0 Directory `ObjectStore`, `--object-dir`, `serverSpec` and devenv.nix defaults.
- [ ] T1 Store: table, index, RLS, attachment rules, fill/insert/bind, unbound bound, read with `IsTopicChannelMember`, parity test.
- [ ] T2 Proto and comms service: `AttachmentBlock`, two RPCs, mapping, update edge, `edgeError`, socket read cap, size-scaled body deadline, `AdminGate`.
- [ ] T3 Runner relay: arms 16 and 17, `CommsCaller`, `executeCall`, coverage table, fakes.
- [ ] T4 Agent: two tools, id reuse, held/stale handling, sniffer, fenced markers, docs and counts (after #2120 and #2136).
- [ ] T5 UI: attachment chip, octet-stream download, native `fileService.SaveAttachment`.
- [ ] T6 CLI: `compass message attach` and `compass message get`.
- [ ] T7 E2E on garage: attach, get, the authz refusal, and no object store.

## Open Questions

- **OQ-1, object store on local stacks (load-bearing).** The Plan builds option (b), a directory-backed `ObjectStore` (T0). **Recommendation:** (b). It is a second implementation of the existing two-method seam, adds no new abstraction, and also makes session blobs work locally. Note that it also turns on the transcript archive flush on local stacks. **Decision needed:** confirm (b), or pick one of these:
  - **(a) S3 only.** Drop T0. On `compass-stack` and the embedded desktop, every upload returns FailedPrecondition until a follow-up adds storage. Session blobs stay off locally.
  - **(c) Postgres `bytea`.** Replace T0 with a bytes table and a store branch for attachments only. The database grows by every file. Session blobs and the transcript archive stay off locally.
- **OQ-2, retention and erasure.** Nothing deletes attachments. Unbound rows older than the D4 window stay. **Recommendation:** one follow-up issue that owns GC of unbound rows, erasure and the shared-key sweep, for attachments and session blobs together (DL-426 OQ-1).
- **OQ-3, UI upload.** This record downloads in the UI but does not upload from it. The issue asks for operator download from the UI and the CLI. D6 covers both, and `compass message attach` and `compass message get` give the CLI both directions. **Recommendation:** keep UI upload out. Add a composer attach button later if Matt wants it.
- **OQ-4, files over 15 MiB.** These need streaming or chunking on every hop. A size-scaled body deadline (D4) also assumes the read caps. **Recommendation:** out of scope until a real case appears.
