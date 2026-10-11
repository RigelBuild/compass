# Compass comms file sharing

## Problem

Issue: RIG-2669. An agent can post channel messages, but it cannot share a file (a diff, a log, a design draft, a screenshot, a data blob) with a channel, a topic, or a DM peer. The operator cannot download one either. Agents today fall back on a shared filesystem path, which Compass does not have.

This record adds file attachments to comms: a file is posted with a message, stored server-side, carried as a block in that message, and read by anyone who can read the message. Agents attach and fetch with two new tools. The operator downloads from the UI and the CLI.

## Approach

An attachment is uploaded first and posted second. Upload stores the bytes and returns an attachment id. The post carries a new `attachment` message block that names the id. The post binds the attachment to that one message. A read is allowed exactly when the reader can read that message.

The design reuses the session-blob path (DL-426, `docs/designs/agent/compass-agent-session-blobs/design.md`) for bytes, and the message read predicate for access. It adds one index table, one block arm, two CommsService RPCs, two agent call arms, two agent tools, one UI chip, one native save method and two CLI verbs.

### D1 Storage reuses the ObjectStore seam

Bytes go through the existing `ObjectStore` seam under the content-addressed key `blobs/<sha256>`, the same key space as session blobs. `Store.PutSessionBlob` in `go/internal/store/agent_session_blobs.go` is the pattern to copy:

- it refuses with no object store: `ErrFailedPrecondition` "session blob object store is not configured";
- it checks the size and verifies the digest;
- it writes `s.objectStore.PutSegment(ctx, "blobs/"+sha256Hex, data)` before the index row, so a retry repairs a half-write.

`ObjectStore` (`go/internal/store/agent_transcripts.go`) has only `PutSegment` and `GetSegment`. This design needs nothing more.

The index is a new table, `comms_attachments`. `agent_session_blobs` cannot carry it: its key is `(session_id, sha256)` and `session_id REFERENCES agent_sessions ON DELETE RESTRICT`. A UI or CLI upload has no session. The new table:

```sql
CREATE TABLE comms_attachments (
    id                  TEXT        PRIMARY KEY,
    uploader_account_id TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    message_id          TEXT        REFERENCES messages (id) ON DELETE RESTRICT,
    sha256              TEXT        NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    size_bytes          BIGINT      NOT NULL CHECK (size_bytes > 0),
    content_type        TEXT        NOT NULL,
    filename            TEXT        NOT NULL CHECK (filename <> ''),
    tenant_id           TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX comms_attachments_message_idx ON comms_attachments (message_id);
```

`id` is server-minted (the 0001 convention: "text ids are server-assigned"). `message_id` is NULL until a post binds it. The table joins the RLS `tenant_tables` loop and the `set_updated_at` trigger list, as `agent_session_blobs` does.

With no object store, `UploadAttachment` returns FailedPrecondition and nothing is written. This is the DL-426 rule. `compass-stack` (`go/cmd/compass-stack/main.go`) has no S3 flags today, so a default local stack has no attachments until OQ-1 is decided.

### D2 The wire is an upload RPC and a block arm

`CommsService` gains two unary RPCs. Both are on the public `@compass/client` surface, because the UI and the CLI call them:

- `UploadAttachment(UploadAttachmentRequest) returns (UploadAttachmentResponse)`. The request carries `filename`, `content_type`, `bytes data` and `sha256`. The response carries the stored `Attachment`.
- `GetAttachment(GetAttachmentRequest) returns (GetAttachmentResponse)`. The request carries `attachment_id`. The response carries the `Attachment` and `bytes data`.

`MessageBlock` today is:

```proto
oneof block { string text = 1; Ask ask = 2; AskAnswerBlock ask_answer = 3; }
```

It gains `Attachment attachment = 4`. No open PR touches this oneof. `Attachment` is `{ string attachment_id = 1; string filename = 2; string content_type = 3; int64 size_bytes = 4; string sha256 = 5; }`.

A post may send only `attachment_id`. The server fills the other fields from the `comms_attachments` row and ignores the client's values. The stored block therefore never claims a size or a type the bytes do not have.

Bind rule: inside the `AppendMessage` transaction, on the fresh-insert branch only, the store runs
`UPDATE comms_attachments SET message_id = $msg WHERE id = $att AND uploader_account_id = $author AND message_id IS NULL`.
No row updated is InvalidArgument "attachment not found or already posted", and the post rolls back. Two consequences:

- Nobody can post an attachment id they did not upload. Without this, a reader could re-post another account's attachment into a channel it controls and so widen the read grant.
- An attachment belongs to exactly one message. Sharing the same file again means a new upload. The bytes de-duplicate under the content-addressed key, so the cost is one row.

An idempotent replay (the `client_request_id` conflict branch) returns the stored message and does not re-bind.

Write-path edges in `go/internal/comms/mapping.go`:

- `blocksFromWire` accepts the arm on POST.
- `updateBlocksFromWire` rejects it (InvalidArgument). The agent UPDATE path (`CommitAgentUpdate` → `Store.UpdateMessageBlocksAsAuthor`) streams conversation frames, which never attach a file. A bound row stays bound even if an update drops the block, so read access cannot change on update.
- `blocksToWire` emits it.

In `go/internal/store/blocks.go`, `marshalBlocks` gains a `blockKindAttachment` case and a `storedAttachment` JSONB shape. `textContent` appends the filename, so `SearchMessages` finds a message by attachment name.

The agent call envelopes in `proto/compass/v1/agent_gateway.proto` gain `upload_attachment = 16` and `get_attachment = 17` on both `CommsCallRequest.call` and `CommsCallResult.result`. Arms 13–15 are claimed by open PRs (#2078, #2139, #2136), so new arms start at 16. Proto numbers are never renumbered down.

### D3 Read access is message read access

`GetAttachment` succeeds when one of two things is true:

- the caller can read the bound message; or
- the caller is the uploader. This lets a client retry a post or check an upload before it posts.

"Can read the message" is the predicate `Store.ListMessages` already uses: "the channel must be one the actor participates in (member row or TREE derivation)". The new sqlc query `GetAttachmentForReader` goes in `go/internal/store/queries/messages.sql`. It joins `comms_attachments → messages → topics → participating` and carries a copy of the `chain` + `participating` CTE. The file header requires this: "every `chain` + `participating` CTE in this file and topics.sql MUST stay identical … It is participation, not channel visibility: never widen it to the owner-set predicate."

So an attachment is readable exactly where its message is readable. A DM is a two-member channel, so a DM grant is a member row and needs no special case. DL-345 (`docs/designs/ui/compass-channels-in-agent-tree/design.md` §2) sets the owner-set grant for channel visibility. If message reads ever adopt that grant, the attachment read inherits it with no separate change, because both use the same CTE copy.

An unknown id, an unbound id that belongs to another account, and an id the caller cannot read all return the same NotFound. This is the D9 not-found/forbidden merge, so a refusal reveals nothing. The row lookup runs under tenant RLS before `GetSegment`, as `ReadSessionBlob` does ("a read needs this session's row first"). Knowing a `sha256` from another tenant therefore reads nothing.

### D4 Size cap and retention

The cap is `agentmsg.MaxAttachmentBytes = MaxBytes - 1<<20` (15 MiB), the same value as `MaxSessionBlobBytes`. Every hop already admits a 16 MiB message:

- `siblingServiceMaxReadBytes = 16 << 20` on the network door's Comms handler;
- `maxAgentMessageBytes = agentmsg.MaxBytes` on the Runner gateway;
- `runnerMaxReadBytes = agentmsg.MaxBytes + 1<<20` in `go/internal/runnerhub/handler.go`.

1 MiB of headroom covers the envelope. The store checks the cap, not only the transport, because the UDS comms handler in `go/server/serve.go` sets no `ReadMaxBytes`. The agent mirrors it as `MAX_ATTACHMENT_BYTES = 15 * 1024 * 1024` and refuses before it sends.

One file per upload and unary bytes. No streaming and no chunking. This keeps `S3ObjectStore.GetSegment` (it reads the whole object into memory) within a known bound.

Retention: an attachment lives as long as its message. Nothing deletes messages today, and `ObjectStore` has no delete. An upload that is never posted leaves an unbound row and an object. Both are bounded by the cap and owned by the uploader. Garbage collection and erasure are deferred together, as DL-426's OQ-1 deferred them ("`blobs/<sha256>` keys are shared across sessions and tenants. Deleting one … needs a system-role reference sweep"). See OQ-2.

Per-tenant storage quotas and billing are product policy and are out of scope here (`docs/concepts/self-host-and-managed.md`, `docs/designs/meta/oss-core-managed-boundary/design.md`). The cap above is a capability bound, not a quota.

### D5 Agent tools attach and read

Two native tools join `createCommsTools` in `packages/compass-agent/src/comms.ts` (its doc comment "Twelve tools" becomes fourteen).

`comms_attach_file` (`approval: "write"`) takes `{ path, channel, topic, text?, create_topic?, content_type? }`.

- It resolves a relative `path` against `COMPASS_WORKDIR` (the variable `cli.ts` already reads).
- It reads the file, refuses above `MAX_ATTACHMENT_BYTES`, and hashes it.
- It sends `upload_attachment`, then the existing `post` arm with an `attachment` block, plus a `text` block when `text` is set.
- Post keeps its rules: `channel` and `topic` are required names, there is no home default, and a new topic needs `create_topic: true`.
- `content_type` defaults from the file extension, else `application/octet-stream`.

`comms_read_file` (`approval: "read"`) takes `{ attachment_id, save_as? }` and sends `get_attachment`. It returns:

- an image content part for `image/png`, `image/jpeg`, `image/gif` and `image/webp`;
- text for valid UTF-8 up to 256 KiB;
- otherwise, or when `save_as` is set, it writes the bytes to disk and returns one line naming the path. The default path is `<os.tmpdir()>/compass-attachments/<attachment_id>/<basename(filename)>`, so nothing lands in the git workdir. `save_as` resolves against `COMPASS_WORKDIR`.

Both renderers show an attachment as one marker line, `[attachment <id> <filename> <content_type> <size>]`: `formatDeliversForPrompt` in `agent.ts` and the `comms_list_messages` body in `comms.ts`. Those switches drop unknown arms today (`return [];`), so without this an agent would not know a file arrived. The filename is poster-controlled and passes through `flat`/`attr`, as ask labels already do.

On the Server, `CommsCaller` (`go/internal/runnerhub/hub.go`) gains `UploadAttachmentAsAccount` and `GetAttachmentAsAccount`, and `executeCall` (`relay_comms.go`) gains the two cases. They run under the session-resolved account, like every other arm.

### D6 The operator downloads from the UI and the CLI

UI:

- `ConvBlock` (`apps/ui/src/comms-stub.ts`) gains `{ kind: "attachment"; attachment: AttachmentRef }`, and `adaptBlock` (`apps/ui/src/live/adapt.ts`) maps it.
- `ChannelView` renders a chip with filename, type, size and a Download action. The action calls `GetAttachment` over the existing transport.
- In a browser, the UI saves through a `Blob` object URL and an `<a download>` click.
- The native WebView has no download support. The compass-app shell gains a `fileService.SaveFile(suggestedName, dataBase64) (string, error)` bound method that opens `app.Dialog.SaveFile()` and writes the file. Unlike `dialogService` (setup mode only), it is registered in client mode.

The UI sends no upload in this record (see OQ-3).

CLI, under `newMessageCmd` in `go/cmd/compass/message.go`:

- `compass message attach --channel <id> --topic <name> <path>` uploads and posts. It sets `CreateTopic` like `message post`, sends no `ClientRequestId` (same reason as `message post`), and prints the message id and the attachment id.
- `compass message get <attachment-id> [--output <path>|-]` downloads. The default output is `./<basename(filename)>`, and it refuses to overwrite an existing file. `-` writes to stdout.

`AdminGate` (`go/internal/auth/admin_gate.go`) lists both RPCs with the other CommsService methods as `authenticatedOpen{}` ("the service body enforces per-account authorization"). `classify_exhaustive_test` fails until they are listed.

## Alternatives considered

- **Reuse `agent_session_blobs`.** Rejected. Its key and FK are per session, and a UI or CLI upload has no session.
- **Inline bytes in the message block (JSONB).** Rejected. 15 MiB rows would bloat `messages`, every `ListMessages` page, and every `SubscribeComms` frame.
- **Presigned object-store URLs.** Rejected. The agent holds no object-store credentials and has no egress path to the store (DL-084 superseded agent-direct S3, DL-063/DL-083). A browser URL would also bypass the participation check.
- **Read authz by JSONB containment** (`blocks @> …`, the `FindAskMessage` shape). Rejected. There is no index on `messages.blocks`, and an attachment could be referenced from many messages. The bound `message_id` gives one indexed join and one owner.
- **A streaming upload RPC.** Deferred. Unary within the existing 16 MiB caps needs no new transport. Larger files are OQ-4.

## Plan

### Global Constraints

- Public repo: no private-monorepo names or paths (`rule://private-repo-boundary`). Quotas and billing stay out (D4).
- Proto: additive only. `MessageBlock.attachment = 4`. Agent arms `upload_attachment = 16` and `get_attachment = 17`. Never renumber down. `buf lint` and `buf breaking` stay green.
- Cap: `agentmsg.MaxAttachmentBytes` (15 MiB) is the one Go source of truth. The agent constant mirrors it with a comment naming the Go constant.
- Errors: no object store → FailedPrecondition. Over the cap, bad digest, empty filename or a bad bind → InvalidArgument. Any unreadable attachment → NotFound (D9 merge).
- Schema: edit `go/internal/store/migrations/0001_init.sql` in place, as the session-blob table did ("Schema changes require a dev DB reset"). Place `comms_attachments` after `messages` ("Statement order is FK order"). Add it to the RLS loop and the trigger list.
- SQL: the new reader query carries an identical `chain` + `participating` CTE copy (the messages.sql header rule).
- Untrusted text: filename and content type pass the render guards (`flat`/`attr`) in every agent renderer. The UI renders them as text, never HTML.
- Tests: red → green. Postgres tests use the existing pgtest harness. The object-store path uses the garage e2e fixture (`go/e2e/fixture.go`, `cfg.S3Endpoint = garage.endpoint`).

### T1 Proto

Edit `proto/compass/v1/comms.proto` and `proto/compass/v1/agent_gateway.proto`, then regenerate the Go, public TS and agent TS outputs.

Interfaces:

```proto
message Attachment { string attachment_id = 1; string filename = 2; string content_type = 3; int64 size_bytes = 4; string sha256 = 5; }
// MessageBlock.block: Attachment attachment = 4;
message UploadAttachmentRequest { string filename = 1; string content_type = 2; bytes data = 3; string sha256 = 4; }
message UploadAttachmentResponse { Attachment attachment = 1; }
message GetAttachmentRequest { string attachment_id = 1; }
message GetAttachmentResponse { Attachment attachment = 1; bytes data = 2; }
rpc UploadAttachment(UploadAttachmentRequest) returns (UploadAttachmentResponse);
rpc GetAttachment(GetAttachmentRequest) returns (GetAttachmentResponse);
// CommsCallRequest.call:  UploadAttachmentRequest upload_attachment = 16; GetAttachmentRequest get_attachment = 17;
// CommsCallResult.result: UploadAttachmentResponse upload_attachment = 16; GetAttachmentResponse get_attachment = 17;
```

Test: buf lint and buf breaking pass. The generated code compiles.

### T2 Store

Add the `comms_attachments` table (D1), its RLS and trigger entries, and `comms_attachments` in `tenantOwned` in `TestRLSCatalogEnabledAndForced`. Add sqlc queries `InsertCommsAttachment` and `BindCommsAttachment`, and `GetAttachmentForReader` in messages.sql (D3). Add the block kind and `textContent` case. Add the bind inside `AppendMessage`, fresh-insert branch only.

Interfaces:

```go
// agentmsg
const MaxAttachmentBytes = MaxBytes - 1<<20
// store
type AttachmentRef struct { ID, Filename, ContentType, SHA256 string; SizeBytes int64 }
// MessageBlock gains: Attachment *AttachmentRef
func (s *Store) PutCommsAttachment(ctx context.Context, uploader AccountID, filename, contentType, sha256Hex string, data []byte) (AttachmentRef, error)
func (s *Store) ReadCommsAttachment(ctx context.Context, actor AccountID, id string) (AttachmentRef, []byte, error)
```

`AppendMessage` fills each attachment block from its row and binds it. Tests (pgtest):

- put then read as the uploader;
- read as a channel member after the post; NotFound for a non-member, and for another account before the post;
- binding an id uploaded by another account fails, and so does a second bind;
- an idempotent replay does not fail;
- over the cap and a bad digest are InvalidArgument;
- a nil object store is FailedPrecondition;
- the row is stamped with the caller's tenant.

### T3 Comms service

Add the `UploadAttachment` and `GetAttachment` handlers to the comms service. Add the `blocksFromWire`, `blocksToWire` and `updateBlocksFromWire` cases (D2). Add `AdminGate` classification. Add `UploadAttachmentAsAccount` and `GetAttachmentAsAccount` on `*Comms` in `go/internal/comms/agent_caller.go`, mapping store errors as the other `*AsAccount` methods do.

Interfaces:

```go
func (c *Comms) UploadAttachment(ctx context.Context, req *connect.Request[compassv1.UploadAttachmentRequest]) (*connect.Response[compassv1.UploadAttachmentResponse], error)
func (c *Comms) GetAttachment(ctx context.Context, req *connect.Request[compassv1.GetAttachmentRequest]) (*connect.Response[compassv1.GetAttachmentResponse], error)
func (c *Comms) UploadAttachmentAsAccount(ctx context.Context, account store.AccountID, req *compassv1.UploadAttachmentRequest) (*compassv1.UploadAttachmentResponse, error)
func (c *Comms) GetAttachmentAsAccount(ctx context.Context, account store.AccountID, req *compassv1.GetAttachmentRequest) (*compassv1.GetAttachmentResponse, error)
```

Tests:

- a post with a client-supplied size or type stores the row's values;
- an `attachment` arm on the update path is InvalidArgument;
- `classify_exhaustive_test` is green.

### T4 Runner relay

Add the two methods to the `CommsCaller` interface in `go/internal/runnerhub/hub.go`, the two `executeCall` cases in `relay_comms.go`, and the arm names to its "no recognized variant" error text.

Interfaces: the `CommsCaller` methods match the T3 `*AsAccount` signatures.

Tests: a runnerhub test drives both arms through `executeCall` with a fake caller. A 15 MiB upload passes `runnerMaxReadBytes`.

### T5 Agent tools and renderers

Add `comms_attach_file` and `comms_read_file` with arktype parameters, following `postParameters`. Add `MAX_ATTACHMENT_BYTES`. Add the attachment marker line to `formatDeliversForPrompt` and the `comms_list_messages` renderer. Document both tools in `docs/concepts/tools.md`.

Interfaces:

```ts
export const attachFileParameters = type({ path: "string", channel: "string", topic: "string", "text?": "string", "create_topic?": "boolean", "content_type?": "string" });
export const readFileParameters = type({ attachment_id: "string", "save_as?": "string" });
export const MAX_ATTACHMENT_BYTES = 15 * 1024 * 1024;
```

Tests (bun:test, fake broker):

- attach sends upload then post, with the returned id in the block;
- an oversize file fails before any call;
- read returns an image part for PNG, text for UTF-8, and a saved path for binary;
- a newline in a filename does not add a marker line.

### T6 UI and native save

Add the `ConvBlock` attachment kind, the `adaptBlock` case, the `ChannelView` chip and a download helper (browser `Blob` path; native `fileService.SaveFile`). Add `fileService` in `go/cmd/compass-app`, registered in client mode.

Interfaces:

```ts
interface AttachmentRef { id: string; filename: string; contentType: string; sizeBytes: number }
// ConvBlock: | { kind: "attachment"; attachment: AttachmentRef }
```

```go
func (f *fileService) SaveFile(ctx context.Context, suggestedName, dataBase64 string) (string, error) // "" when the user cancels
```

Tests: an adapt test for the new kind, and a component test that the chip renders and calls `GetAttachment`. A Go test for `SaveFile` writes through an injected dialog.

### T7 CLI verbs

Add `newMessageAttachCmd` and `newMessageGetCmd` under `newMessageCmd`, and change its `Short` to "(post, attach, get)".

Interfaces:

```go
func runMessageAttach(ctx context.Context, client compassv1connect.CommsServiceClient, args messageAttachArgs, out io.Writer) error
func runMessageGet(ctx context.Context, client compassv1connect.CommsServiceClient, id, output string, out io.Writer) error
```

Tests: a fake client checks upload then post with the block; get refuses to overwrite; `-` writes to stdout.

### T8 End to end

An e2e on the garage fixture: an agent runs `comms_attach_file` into a channel, then a member operator runs `compass message get` and gets the same bytes. A non-member gets NotFound. A stack without S3 returns FailedPrecondition on upload.

## Tasks

- [ ] T1 Proto: `Attachment`, `MessageBlock.attachment = 4`, two CommsService RPCs, agent arms 16 and 17.
- [ ] T2 Store: `comms_attachments` table, RLS, queries, put/read, bind in `AppendMessage`, block codec.
- [ ] T3 Comms service: handlers, mapping, `AdminGate`, `*AsAccount` methods.
- [ ] T4 Runner relay: `CommsCaller` methods and `executeCall` cases.
- [ ] T5 Agent: two tools, renderers, `docs/concepts/tools.md`.
- [ ] T6 UI: attachment chip, download, native `fileService.SaveFile`.
- [ ] T7 CLI: `compass message attach` and `compass message get`.
- [ ] T8 E2E on garage: attach, get, the authz refusal, and no object store.

## Open Questions

- **OQ-1, default local stack (blocks the feature on desktop).** `compass-stack` has no S3 flags, so a default local stack refuses uploads. Options: (a) ship ObjectStore-only and wire S3 flags into `compass-stack` later; (b) add a directory-backed `ObjectStore` for local stacks in this record; (c) add a Postgres `bytea` fallback. **Recommendation:** (b). It implements the existing seam with no new abstraction, and session blobs get it for free. Decision needed: which option, and whether it lands in this record or a follow-up.
- **OQ-2, retention and erasure.** Nothing deletes attachments. Unposted uploads stay. A follow-up should own GC of unbound rows, erasure, and the shared-key sweep, together with DL-426 OQ-1. **Recommendation:** one follow-up issue for both blob kinds.
- **OQ-3, UI upload.** This record downloads in the UI but does not upload from it. The issue asks only for operator download. **Recommendation:** keep upload out; add a composer attach button later if Matt wants it.
- **OQ-4, files over 15 MiB.** These need streaming or chunking on every hop. **Recommendation:** out of scope until a real case appears.
