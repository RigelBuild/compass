package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppendMessage stores a new message under a topic in channelID, assigning the
// row id and timestamp (comms.proto:463-479). The topic is resolved inside the
// insert tx: a TopicRef.Name is get-or-created on (channel_id, lower(name)); a
// TopicRef.ID names an existing topic, validated to live under channelID. The
// blocks are serialized to JSONB and their text content extracted for the
// full-text index. When clientRequestID is non-empty, a retry with the same key
// returns the already-stored message rather than duplicating (idempotency,
// comms.proto:470-474). The returned bool reports whether a row was genuinely
// inserted: it is false on the idempotent-retry return, so the caller
// suppresses a duplicate MessagePosted fan-out for a row that did not change. A
// message with no blocks, a TopicRef that is neither exactly-id nor
// exactly-name, or a TopicRef.ID naming a topic in another channel (or no
// topic) is ErrInvalidArgument. The membership check, topic resolution, insert,
// and last_seq denormalization all run in one transaction, so a membership
// revoked between them cannot slip a message into a channel the author can no
// longer read, and a get-or-created topic never outlives a rolled-back insert.
func (s *Store) AppendMessage(ctx context.Context, m Message, channelID string, topic TopicRef, clientRequestID string) (Message, bool, error) {
	if channelID == "" {
		return Message{}, false, fmt.Errorf("%w: message channel is required", ErrInvalidArgument)
	}
	if len(m.Blocks) == 0 {
		return Message{}, false, fmt.Errorf("%w: message has no blocks", ErrInvalidArgument)
	}
	// Exactly one of id / name identifies the target topic.
	if (topic.ID == "") == (topic.Name == "") {
		return Message{}, false, fmt.Errorf("%w: exactly one of topic id or name is required", ErrInvalidArgument)
	}
	// Mint ask ids before the insert so the returned (and stored) message
	// carries the ids RespondToAsk correlates against; insertMessageTx
	// serializes and validates the blocks inside the tx.
	mintAskIDs(m.Blocks)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, false, fmt.Errorf("store: begin append message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// D9 write-authz: the author must be a member of the target channel, so a
	// non-member cannot persist (and fan out) into a private channel it can't
	// see — the write-side mirror of the ListMessages/AnswerAsk read gate. A
	// non-member gets ErrNotFound (the not-found/forbidden merge), never a hint
	// that the channel exists. Checked in the same tx as the insert so a
	// concurrent removal cannot race between the gate and the write.
	if err := requireChannelMember(ctx, tx, m.AuthorAccountID, ChannelID(channelID)); err != nil {
		return Message{}, false, err
	}

	// T4 post policy: on an OWNER_ONLY channel, only owner_account_id may post.
	// A non-owner is refused with the SAME ErrNotFound a non-member gets (the
	// not-found/forbidden merge), so the policy leaks no oracle: a member who
	// may not post is indistinguishable from a non-member. Checked in this same
	// tx as the membership gate and the insert, under the committed policy.
	var (
		postPolicy int32
		ownerAcct  string
	)
	if err := tx.QueryRow(ctx,
		"SELECT post_policy, COALESCE(owner_account_id, '') FROM channels WHERE id = $1",
		channelID,
	).Scan(&postPolicy, &ownerAcct); err != nil {
		if noRows(err) {
			return Message{}, false, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
		}
		return Message{}, false, fmt.Errorf("store: read channel post policy: %w", err)
	}
	if ChannelPostPolicy(postPolicy) == ChannelPostPolicyOwnerOnly &&
		string(m.AuthorAccountID) != ownerAcct {
		return Message{}, false, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
	}

	at := time.Now().UTC()
	topicID, err := resolveTopicForAppend(ctx, tx, channelID, topic, m.AuthorAccountID, at.UnixMilli())
	if err != nil {
		return Message{}, false, err
	}

	inserted, err := insertMessageTx(ctx, tx, m, topicID, clientRequestID)
	switch {
	case errors.Is(err, errMessageInsertConflict):
		// ON CONFLICT DO NOTHING suppressed the insert: a message with this
		// idempotency key already exists (a retry). Nothing was written, so the
		// tx rolls back (unwinding the topic get-or-create too); return the
		// already-committed row with inserted=false so the handler suppresses a
		// duplicate MessagePosted. AnswerAsk does NOT share this arm — its
		// answer insert carries no idempotency key, so the conflict path is
		// unreachable there (see insertMessageTx).
		stored, err := s.getMessageByRequestID(ctx, m.AuthorAccountID, clientRequestID)
		return stored, false, err
	case pgErrIs(err, pgForeignKeyViolation):
		return Message{}, false, fmt.Errorf("%w: unknown author %q", ErrInvalidArgument, m.AuthorAccountID)
	case err != nil:
		return Message{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Message{}, false, fmt.Errorf("store: commit append message: %w", err)
	}
	return inserted, true, nil
}

// errMessageInsertConflict signals that insertMessageTx's INSERT was suppressed
// by the ON CONFLICT (author_account_id, client_request_id) idempotency index —
// a retry with a re-used non-empty client_request_id. It is an internal signal,
// never returned to a caller: AppendMessage catches it and re-reads the
// already-committed row (inserted=false). AnswerAsk cannot hit it — its answer
// insert passes clientRequestID="", so the partial index (WHERE
// client_request_id <> ”) is unreachable — and treats any insert error as a
// fatal invariant violation that rolls the whole answer back.
var errMessageInsertConflict = errors.New("store: message insert conflict")

// insertMessageTx is the tx-scoped insert core shared by AppendMessage and
// AnswerAsk: it inserts the message row under topicID and maintains the topic's
// denormalized last_seq (GREATEST) — nothing else. It does NOT port
// AppendMessage's ON CONFLICT handling (pool re-read + rollback-and-return):
// that arm cannot run inside AnswerAsk's tx without either losing the Answered
// flip on rollback or racing the still-uncommitted row on a pool read. So the
// helper only signals a conflict (errMessageInsertConflict) and leaves the
// caller to decide: AppendMessage re-reads the committed row; AnswerAsk treats
// it as an impossible-invariant error. It assigns the row id and timestamp and
// returns the populated Message; it does NOT commit — the caller owns the tx.
func insertMessageTx(ctx context.Context, tx pgx.Tx, m Message, topicID string, clientRequestID string) (Message, error) {
	blocksJSON, err := marshalBlocks(m.Blocks)
	if err != nil {
		return Message{}, err
	}
	id := newID()
	at := time.Now().UTC()
	const q = `
		INSERT INTO messages (id, topic_id, author_account_id, at_unix_ms, blocks, text_content, client_request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (author_account_id, client_request_id) WHERE client_request_id <> ''
		DO NOTHING
		RETURNING id, at_unix_ms, seq`
	var (
		storedID string
		atMS     int64
		seq      int64
	)
	err = tx.QueryRow(ctx, q,
		id, topicID, string(m.AuthorAccountID),
		at.UnixMilli(), blocksJSON, textContent(m.Blocks), clientRequestID,
	).Scan(&storedID, &atMS, &seq)
	switch {
	case noRows(err):
		return Message{}, errMessageInsertConflict
	case err != nil:
		return Message{}, fmt.Errorf("store: insert message: %w", err)
	}

	// Maintain the topic's denormalized activity marker in the same tx. GREATEST
	// so two concurrent appends to one topic converge on the higher seq
	// regardless of commit order (the row-lock serializes the two updates).
	if _, err := tx.Exec(ctx,
		`UPDATE topics SET last_seq = GREATEST(last_seq, $2) WHERE id = $1`,
		topicID, seq,
	); err != nil {
		return Message{}, fmt.Errorf("store: update topic last_seq: %w", err)
	}

	m.ID = MessageID(storedID)
	m.TopicID = topicID
	m.At = time.UnixMilli(atMS).UTC()
	return m, nil
}

// resolveTopicForAppend resolves the target topic for an append inside the
// insert tx, returning the resolved topics.id. An id-ref is validated to live
// under channelID: a topic in another channel — or an unknown id — is one
// indistinguishable ErrInvalidArgument, so the existence of a foreign topic
// never leaks (the same oracle-closing collapse the old same-channel-parent
// check applied).
//
// A name-ref resolves on (channel_id, lower(name)), gated by TopicRef.Create
// (peer-DM record R5). When Create is set the name is get-or-created via ON
// CONFLICT DO NOTHING + re-SELECT, so two racing posts converge on one topic row
// with neither surfacing a unique-violation. When Create is UNSET a name that
// resolves to no existing topic is ErrNotFound (in-band) rather than a silent
// mint — the tool-edge guard against topic sprawl. Either way, resolving to an
// archived topic clears its archived flag in the same tx (archive is a tidiness
// flag, not a lock — a post at a tidied-away name revives the conversation).
func resolveTopicForAppend(ctx context.Context, tx pgx.Tx, channelID string, topic TopicRef, author AccountID, atMS int64) (string, error) {
	if topic.ID != "" {
		var topicChannelID string
		switch err := tx.QueryRow(ctx,
			`SELECT channel_id FROM topics WHERE id = $1`, topic.ID,
		).Scan(&topicChannelID); {
		case noRows(err):
			return "", fmt.Errorf("%w: topic %q is not in this channel", ErrInvalidArgument, topic.ID)
		case err != nil:
			return "", fmt.Errorf("store: resolve topic: %w", err)
		}
		if topicChannelID != channelID {
			return "", fmt.Errorf("%w: topic %q is not in this channel", ErrInvalidArgument, topic.ID)
		}
		return topic.ID, nil
	}

	// Get-or-create is gated on Create (R5). When set, settle a concurrent
	// create by the unique index, never a naive SELECT-then-INSERT: a concurrent
	// inserter's uncommitted row makes this INSERT block until it commits, after
	// which DO NOTHING fires and the re-SELECT below reads the surviving row.
	// When unset, the mint is skipped entirely — a name that resolves to no row
	// below is ErrNotFound.
	if topic.Create {
		if _, err := tx.Exec(ctx,
			`INSERT INTO topics (id, channel_id, name, created_by_account_id, created_at_unix_ms)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (channel_id, lower(name)) DO NOTHING`,
			newID(), channelID, topic.Name, string(author), atMS,
		); err != nil {
			if pgErrIs(err, pgForeignKeyViolation) {
				return "", fmt.Errorf("%w: unknown channel %q or author %q", ErrInvalidArgument, channelID, author)
			}
			return "", fmt.Errorf("store: get-or-create topic: %w", err)
		}
	}
	var (
		topicID  string
		archived bool
	)
	if err := tx.QueryRow(ctx,
		`SELECT id, archived FROM topics WHERE channel_id = $1 AND lower(name) = lower($2)`,
		channelID, topic.Name,
	).Scan(&topicID, &archived); err != nil {
		if noRows(err) {
			// Create was unset (or a racing delete removed the row) and no topic
			// carries this name: an in-band ErrNotFound, never a silent mint. The
			// error names only the topic name, never whether the channel exists.
			return "", fmt.Errorf("%w: topic %q", ErrNotFound, topic.Name)
		}
		return "", fmt.Errorf("store: resolve topic: %w", err)
	}
	if archived {
		if _, err := tx.Exec(ctx,
			`UPDATE topics SET archived = FALSE WHERE id = $1`, topicID,
		); err != nil {
			return "", fmt.Errorf("store: revive archived topic: %w", err)
		}
	}
	return topicID, nil
}

// MessagesHeadSeq returns the current head of the message sequence — the
// highest messages.seq committed, or 0 on an empty store. This is the
// store-space snapshot boundary the server hands a client on the subscribe
// response (SubscribeCommsResponse.snapshot_seq, comms.proto:353-368): the
// client passes it back to each catch-up read RPC as Page.SnapshotSeq so every
// page reads one point-in-time view (design.md:807-817). It is deliberately
// store-space (messages.seq, durable) rather than bus-space: the event bus
// resets to seq 1 each boot, so a bus-space boundary would drop every durable
// row after a restart. Capture it AFTER subscribing to the bus so a message
// committing in the window between subscribe and this read lands on the live
// tail rather than falling between the snapshot and the tail.
// The head is instance-global (COALESCE(MAX(seq),0) over all messages), not
// actor- or channel-scoped: a single instance-wide token is the established
// contract's ratified shape (design.md:809-816), store-space so it survives
// restarts and covers the empty-ring bootstrap. The subscribe path sends it
// before visibility filtering, so a subscriber learns the instance-wide durable
// message count (one integer, no content) even for channels it cannot see; this
// count-metadata exposure is accepted as within the threat model, not a leak to
// close by scoping the boundary — that would be a different token (RIG-1333 OQ4).
func (s *Store) MessagesHeadSeq(ctx context.Context) (uint64, error) {
	var head uint64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM messages`,
	).Scan(&head); err != nil {
		return 0, fmt.Errorf("store: read messages head seq: %w", err)
	}
	return head, nil
}

// updateMessageBlocksExec is the shared block-write core, run against
// AnswerAsk's locked read-modify-write transaction. It
// re-serializes the full block set and refreshes the extracted text content so
// search stays consistent with the current blocks. An empty block set or an
// unknown message is rejected. ask_id is assigned once at append and immutable
// thereafter, so an ask block here must carry its existing id; an empty AskID is
// rejected rather than re-minted (a fresh id would orphan any pending
// RespondToAsk against the original).
//
// It performs NO membership or authorship check, so it is deliberately
// unexported: every exported write path must authorize before reaching it.
func updateMessageBlocksExec(ctx context.Context, db execer, id MessageID, blocks []MessageBlock) error {
	if len(blocks) == 0 {
		return fmt.Errorf("%w: message has no blocks", ErrInvalidArgument)
	}
	for i, b := range blocks {
		if b.Ask != nil && b.Ask.AskID == "" {
			return fmt.Errorf("%w: block %d ask has no ask_id (an update must carry the existing id)", ErrInvalidArgument, i)
		}
	}
	blocksJSON, err := marshalBlocks(blocks)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx,
		"UPDATE messages SET blocks = $1, text_content = $2 WHERE id = $3",
		blocksJSON, textContent(blocks), string(id),
	)
	if err != nil {
		return fmt.Errorf("store: update message blocks: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: message %q", ErrNotFound, id)
	}
	return nil
}

// UpdateMessageBlocksAsAuthor replaces a message's block set UNDER an acting
// account — the only update path safe for a message id that arrives from
// outside the Server's own trust boundary (a relayed agent MessageUpdated
// frame, RIG-1364 T3).
//
// Why it is a fork and not a flag on the shared core. updateMessageBlocksExec
// addresses the row by a bare MessageID with NO membership and NO authorship
// check. That is correct where it is used — AnswerAsk has already resolved the
// target through a membership JOIN and locked it FOR UPDATE, so re-checking
// would be redundant, and AnswerAsk deliberately permits a MEMBER who is not
// the author to answer. Folding this path onto that core would either strip the
// authz a relayed id requires or break every ask answered by anyone but the
// asker. The two predicates genuinely differ, so they are two statements.
//
// The predicate is membership AND authorship: the actor must be a member of the
// message's channel and be its author. Both halves are load-bearing — authorship
// alone would let an account edit its own past message in a channel it has since
// been removed from, and membership alone would let any member rewrite another
// account's words. A failure of EITHER half, and an id that names no row at all,
// return the same ErrNotFound: the D9 not-found/forbidden merge, so an actor
// cannot learn that a message it may not touch exists (the same collapse
// AnswerAsk documents at :307-310).
//
// The updated row is returned via RETURNING, so the caller fans out the
// post-update state without a second read that could observe a later write.
// Validation mirrors updateMessageBlocksExec (an empty block set is rejected; an
// ask block must carry its immutable ask_id, since a re-minted id would orphan a
// pending RespondToAsk) and adds one this path needs: an EMPTY message id is
// ErrInvalidArgument rather than a zero-rows ErrNotFound, because a relayed
// MessageUpdated whose message.id was never stamped is a malformed frame, not an
// edit aimed at a row the actor cannot see.
//
// Deliberately NOT built on RequireAgentSessionSubscriber (agent_sessions.go:91).
// That primitive resolves the whole session-ownership chain in a SINGLE EXISTS
// query returning only ErrNotFound-or-nil, so an unknown session and a
// known-but-foreign one are indistinguishable by error class AND by timing — a
// deliberate anti-enumeration property. Reusing its joins here would mean
// splitting it into a resolve-then-check pair, which would look like a clean
// refactor, pass every test, and reintroduce a session-enumeration oracle. This
// path needs a message-scoped predicate, not a session-scoped one, so it is its
// own single-statement gate and that primitive is left completely alone.
func (s *Store) UpdateMessageBlocksAsAuthor(ctx context.Context, actor AccountID, id MessageID, blocks []MessageBlock) (Message, error) {
	if id == "" {
		return Message{}, fmt.Errorf("%w: message id is required", ErrInvalidArgument)
	}
	if len(blocks) == 0 {
		return Message{}, fmt.Errorf("%w: message has no blocks", ErrInvalidArgument)
	}
	for i, b := range blocks {
		if b.Ask != nil && b.Ask.AskID == "" {
			return Message{}, fmt.Errorf("%w: block %d ask has no ask_id (an update must carry the existing id)", ErrInvalidArgument, i)
		}
	}
	blocksJSON, err := marshalBlocks(blocks)
	if err != nil {
		return Message{}, err
	}

	// One statement, so the authz predicate and the write cannot race: a
	// membership revoked concurrently either lands before the UPDATE (which then
	// matches no row) or after it, never between a separate check and the write.
	// The EXISTS subquery is the membership half and the author_account_id
	// equality the authorship half; both must hold for the row to match.
	const q = `
		UPDATE messages m
		SET blocks = $1, text_content = $2
		FROM topics t
		WHERE m.id = $3
		  AND t.id = m.topic_id
		  AND m.author_account_id = $4
		  AND EXISTS (
		    SELECT 1 FROM channel_members cm
		    WHERE cm.channel_id = t.channel_id AND cm.account_id = $4
		  )
		RETURNING m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks`
	rows, err := s.pool.Query(ctx, q, blocksJSON, textContent(blocks), string(id), string(actor))
	if err != nil {
		return Message{}, fmt.Errorf("store: update message blocks as author: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		// Unknown id, not the author, or no longer a member — one answer for all
		// three, so a refusal enumerates nothing.
		return Message{}, fmt.Errorf("%w: message %q", ErrNotFound, id)
	}
	return msgs[0], nil
}

// MessageAskIDs returns the ask_id of every ask block on the message, in block
// order, for the relayed-update write-through's ask_id reconciliation
// (comms.CommitAgentUpdate). It exists so the UPDATE path can source the
// server-owned ask_id from the stored row instead of stripping it (the POST
// path's mintAskIDs behavior, wrong for an update) or trusting a wire value.
//
// Safe as a SEPARATE statement from the authz UPDATE precisely because ask_id is
// immutable: mintAskIDs (blocks.go) assigns it once at append and nothing ever
// reassigns it, so a value read here cannot be invalidated by a later write —
// unlike the mutable post-state the UPDATE returns via RETURNING, this read
// observes a field that is stable for the row's life. It is scoped by message id
// ALONE — the same scope the UPDATE addresses — and performs NO membership or
// authorship check and returns NO distinct not-found (an unknown id or a message
// with no ask yields an empty slice), so it cannot be turned into an
// authz/session enumeration oracle: the sole authz gate remains the
// single-statement UpdateMessageBlocksAsAuthor that follows, and its result is
// never derived from what this read returned.
func (s *Store) MessageAskIDs(ctx context.Context, id MessageID) ([]string, error) {
	var blocksJSON []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT blocks FROM messages WHERE id = $1`, string(id),
	).Scan(&blocksJSON); err != nil {
		if noRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read message ask ids: %w", err)
	}
	blocks, err := unmarshalBlocks(blocksJSON)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, b := range blocks {
		if b.Ask != nil {
			ids = append(ids, b.Ask.AskID)
		}
	}
	return ids, nil
}

// ListMessages pages a channel's messages newest-first, clamped to the store's
// page bounds (comms.proto:446-461). Ordering keys on the monotonic seq (a
// stable total order even under equal timestamps); Page.BeforeMessageID pages
// strictly before a given message. An unknown BeforeMessageID is
// ErrInvalidArgument (a cursor that isn't a real message). An optional
// q.TopicID narrows the read to one topic; empty reads the whole channel across
// every topic.
//
// The channel is resolved THROUGH the topic join now that a message carries no
// channel_id: messages JOIN topics ON topic_id, filtered by topics.channel_id.
// Visibility is enforced in SQL — the channel must be one the actor is a member
// of (JOIN channel_members on the topic's channel), so a non-member — or a
// caller naming a channel it cannot see — reads nothing rather than leaking a
// private channel's history by id (the D9 not-found/forbidden merge, matching
// SearchMessages). The visibility gate is the store's, not the RPC edge's.
func (s *Store) ListMessages(ctx context.Context, q ListMessagesQuery) ([]Message, error) {
	if q.ChannelID == "" {
		return nil, fmt.Errorf("%w: list channel is required", ErrInvalidArgument)
	}
	limit := clampLimit(q.Page.Limit)

	var beforeSeq int64
	if q.Page.BeforeMessageID != "" {
		// Scope the cursor probe to the actor's membership too, so a non-member
		// naming a real message in a channel it cannot see gets the same
		// "not in channel" result as a fake id — no existence oracle across the
		// visibility boundary (the D9 not-found/forbidden merge the main query and
		// AnswerAsk also apply). The channel is the cursor message's topic's
		// channel.
		err := s.pool.QueryRow(ctx,
			`SELECT m.seq FROM messages m
			 JOIN topics t ON t.id = m.topic_id
			 JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
			 WHERE m.id = $2 AND t.channel_id = $3`,
			string(q.Actor), string(q.Page.BeforeMessageID), string(q.ChannelID),
		).Scan(&beforeSeq)
		if err != nil {
			if noRows(err) {
				return nil, fmt.Errorf("%w: before-cursor %q not in channel", ErrInvalidArgument, q.Page.BeforeMessageID)
			}
			return nil, fmt.Errorf("store: resolve page cursor: %w", err)
		}
	}

	// A zero beforeSeq (no cursor) reads the newest page; a positive one pages
	// strictly older. seq is BIGSERIAL starting at 1, so 0 is below every row.
	// The topic join resolves each message's channel, and the membership JOIN on
	// that channel scopes the read to the actor's visible set. An empty $6
	// TopicID reads the whole channel; a non-empty one narrows to that topic. A
	// non-zero SnapshotSeq bounds the read to the point-in-time snapshot the
	// client captured on subscribe (seq <= SnapshotSeq, comms.proto:353-368,
	// design.md:807-817); zero reads the latest, no boundary.
	// The boundary is point-in-time on set membership (which messages the page
	// returns, by insert seq), not on content: a blocks update mutates m.blocks in
	// place without bumping m.seq, so a row present at the boundary but edited
	// mid-catch-up returns its post-boundary blocks. This is sufficient, not a
	// lost update — the matching MessageUpdated also rides the live tail, so an
	// id-deduping client converges to current content (last-write-wins).
	// Freezing content too would need an update/change-seq and a larger schema
	// change; membership-only is the ratified scope (RIG-1333 OQ5).
	const query = `
		SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
		FROM messages m
		JOIN topics t ON t.id = m.topic_id
		JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
		WHERE t.channel_id = $2 AND ($3 = 0 OR m.seq < $3) AND ($5 = 0 OR m.seq <= $5)
		  AND ($6 = '' OR m.topic_id = $6)
		ORDER BY m.seq DESC
		LIMIT $4`
	// seq is BIGSERIAL (the int64 domain) and SnapshotSeq is a server-issued
	// boundary the client echoes back, so the value is in range by construction;
	// an out-of-range client value degrades to an empty page (m.seq <= a negative
	// bound matches nothing), never a fault.
	snap := int64(q.Page.SnapshotSeq) //nolint:gosec // G115: see the note above — server-issued seq, int64 domain
	rows, err := s.pool.Query(ctx, query, string(q.Actor), string(q.ChannelID), beforeSeq, int64(limit), snap, q.TopicID)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// SearchMessages runs a Postgres full-text search over message text, scoped to
// the actor's visible set server-side (comms.proto:489-504, design.md:1137-1139).
// scope optionally narrows to one channel; otherwise it searches every channel
// the actor is a member of. Results are best-match-first, clamped to the page
// bounds. Visibility is enforced in SQL — the actor sees a message only in a
// channel it belongs to — so a scope pointing at a channel the actor cannot see
// yields nothing rather than leaking.
func (s *Store) SearchMessages(ctx context.Context, actor AccountID, scope SearchScope, query string, page Page) ([]Message, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: search query is required", ErrInvalidArgument)
	}
	limit := clampLimit(page.Limit)

	// websearch_to_tsquery parses a human query string (quoted phrases, OR, -)
	// safely — no query-syntax injection, and an all-stopword query yields no
	// rows rather than erroring. Visibility: the message's channel (resolved
	// through the topic join) must be one the actor is a member of; the optional
	// scope narrows within that set.
	const q = `
		SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
		FROM messages m
		JOIN topics t ON t.id = m.topic_id
		JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
		WHERE m.search_tsv @@ websearch_to_tsquery('english', $2)
		  AND ($3 = '' OR t.channel_id = $3)
		  AND ($5 = 0 OR m.seq <= $5)
		ORDER BY ts_rank(m.search_tsv, websearch_to_tsquery('english', $2)) DESC, m.seq DESC
		LIMIT $4`
	// SnapshotSeq is int64-safe by construction; see ListMessages.
	snap := int64(page.SnapshotSeq) //nolint:gosec // G115: server-issued seq, int64 domain (see ListMessages)
	rows, err := s.pool.Query(ctx, q, string(actor), query, string(scope.ChannelID), int64(limit), snap)
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

// AnswerAsk records a participant's atomic answer to a pending structured ask
// (RespondToAsk; see docs/designs/product/compass-ask-typed-derivation.md). It
// locates the message whose blocks carry an ask with askID within the actor's
// visible set — the membership JOIN makes "the message exists" and "the actor
// participates" one gate — records the per-question answers on that ask block,
// and persists via the immutable-ask_id update path. It ALSO posts the answer
// as a new message — authored by actor, in the ask's channel/topic, carrying a
// single ask_answer block snapshotting the just-answered ask — inserted in the
// SAME tx so "ask answered" and "answer message exists" are one atomic fact.
// It returns both messages: the updated ask (the handler publishes
// MessageUpdated) and the answer (the handler publishes MessagePosted).
//
// Answering is atomic: answers must cover EXACTLY the ask's question_id set —
// every question answered once, no unknown or repeated question_id. An answer
// entry that is explicitly empty (no chosen ids AND empty custom_text) is an
// ACCEPTED deliberate skip, not a rejection (native forward-skip parity), and
// still satisfies coverage.
//
// Not-found and not-visible collapse to ErrNotFound deliberately: a message the
// actor cannot see is indistinguishable from a nonexistent askID, so a probe
// cannot learn an ask exists across a visibility boundary (the D9 not-found/
// forbidden merge). Validation failures — coverage gaps, an option not offered
// by the question, or multiple choices on a single-select question — are
// ErrInvalidArgument.
func (s *Store) AnswerAsk(ctx context.Context, actor AccountID, askID string, answers []AskAnswer) (askMsg Message, answerMsg Message, err error) {
	if askID == "" {
		return Message{}, Message{}, fmt.Errorf("%w: ask id is required", ErrInvalidArgument)
	}

	// Serialized find-and-answer: the whole read-modify-write runs in one
	// transaction with the matched message row locked FOR UPDATE, so two
	// concurrent answers to different asks on the SAME message can't lost-update
	// each other. Without the lock, each would read the pre-answer block set,
	// answer its own ask in that snapshot, and write the full set back — the
	// second commit clobbering the first's answer. The lock makes the second
	// answer block until the first commits, then re-read the updated blocks
	// (READ COMMITTED EvalPlanQual) and layer its own answer on top, so both
	// survive (RIG-1226).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, Message{}, fmt.Errorf("store: begin answer ask: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// Visibility + existence in one gate: the message's channel must be one the
	// actor is a member of, and its blocks JSONB must contain an ask with askID.
	// Zero rows -> ErrNotFound (never a distinct not-authorized), so ask
	// existence cannot leak across a membership boundary. FOR UPDATE OF m locks
	// the message row (not the membership row) for the transaction's duration.
	const q = `
		SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
		FROM messages m
		JOIN topics t ON t.id = m.topic_id
		JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
		WHERE m.blocks @> $2::jsonb
		FOR UPDATE OF m`
	filter, err := askIDContainmentFilter(askID)
	if err != nil {
		return Message{}, Message{}, fmt.Errorf("store: marshal ask filter: %w", err)
	}
	rows, err := tx.Query(ctx, q, string(actor), filter)
	if err != nil {
		return Message{}, Message{}, fmt.Errorf("store: find ask: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return Message{}, Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, Message{}, fmt.Errorf("%w: ask %q", ErrNotFound, askID)
	}
	msg := msgs[0]
	// rows must be closed before issuing further queries on the same tx (pgx
	// serializes a connection: an open rows cursor blocks the answer insert).
	rows.Close()

	// Locate the ask block and validate the answers cover its questions exactly,
	// each answer against its question's offered options and arity, then record
	// them in place. The answer-once guard inside applyAskAnswer is the sole
	// single-fire mechanism: a second answer is ErrConflict here, BEFORE the
	// answer message is built, so at most one answer message ever exists.
	if err := applyAskAnswer(&msg, askID, answers); err != nil {
		return Message{}, Message{}, err
	}
	if err := updateMessageBlocksExec(ctx, tx, msg.ID, msg.Blocks); err != nil {
		return Message{}, Message{}, err
	}

	// Build the answer message: authored by the answerer (actor), in the ask's
	// own topic, carrying a single ask_answer block snapshotting the
	// just-answered ask with the asking agent (the ask message's author)
	// denormalized as the target. Insert it in THIS tx via insertMessageTx, so
	// the Answered flip and the delivering message commit atomically. NO
	// idempotency key (clientRequestID="") — the answer-once guard above is the
	// sole single-fire mechanism, so the ON CONFLICT dedup is unreachable. The
	// membership/post-policy gates are NOT re-run: the actor's membership in the
	// ask's channel is already proven by the visibility JOIN above, in this same
	// tx. A zero-rows insert is an impossible-invariant violation (the answer
	// carries no dedup key), so any insert error rolls the whole answer back
	// rather than committing a flip with no message.
	answered := findAsk(msg.Blocks, askID)
	if answered == nil {
		return Message{}, Message{}, fmt.Errorf("store: answered ask %q vanished from blocks", askID)
	}
	answer := Message{
		AuthorAccountID: actor,
		Blocks: []MessageBlock{{AskAnswer: &AskAnswerBlock{
			Ask:            *answered,
			AskerAccountID: msg.AuthorAccountID,
		}}},
	}
	inserted, err := insertMessageTx(ctx, tx, answer, msg.TopicID, "")
	if err != nil {
		return Message{}, Message{}, fmt.Errorf("store: insert answer message: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, Message{}, fmt.Errorf("store: commit answer ask: %w", err)
	}
	return msg, inserted, nil
}

// AskAnswer is one participant answer to a single question within an ask, keyed
// by QuestionID. An answer with no chosen ids AND empty CustomText is a
// deliberate skip (accepted, not rejected).
type AskAnswer struct {
	QuestionID      string
	ChosenOptionIDs []string
	CustomText      string
}

// findAsk returns the ask carrying askID within blocks, or nil when no block
// holds it. Used to snapshot the just-answered ask into the answer message
// after applyAskAnswer has recorded the answers in place.
func findAsk(blocks []MessageBlock, askID string) *Ask {
	for i := range blocks {
		if a := blocks[i].Ask; a != nil && a.AskID == askID {
			return a
		}
	}
	return nil
}

// applyAskAnswer finds the ask block carrying askID in msg and records an atomic
// answer: the answers must cover EXACTLY the ask's question_id set (every
// question once, no unknown or repeated question_id), and each answer's chosen
// options must be offered by its question and respect its AllowMultiple arity.
// An explicitly empty answer entry (no chosen ids AND empty custom_text) is an
// accepted skip that still satisfies coverage. All rejections are
// ErrInvalidArgument. ask_id is immutable (preserved by the update write path),
// so recording an answer never re-mints it.
func applyAskAnswer(msg *Message, askID string, answers []AskAnswer) error {
	for i := range msg.Blocks {
		ask := msg.Blocks[i].Ask
		if ask == nil || ask.AskID != askID {
			continue
		}
		// Reject a second answer: an ask is answered exactly once. A re-answer
		// would silently overwrite the recorded answer (destroying the audit
		// value the answer fields carry), so it is a conflict, not a fresh
		// write. Answered flips only here, so this is the sole gate.
		if ask.Answered {
			return fmt.Errorf("%w: ask %q is already answered", ErrConflict, askID)
		}
		// Index the ask's questions by id; the containment/validate guards on the
		// write path guarantee these ids are unique and non-empty.
		byID := make(map[string]*AskQuestion, len(ask.Questions))
		for qi := range ask.Questions {
			byID[ask.Questions[qi].QuestionID] = &ask.Questions[qi]
		}
		// Coverage: exactly one answer per question, no unknown or repeated id.
		answered := make(map[string]struct{}, len(answers))
		for _, a := range answers {
			q, known := byID[a.QuestionID]
			if !known {
				return fmt.Errorf("%w: answer names unknown question_id %q in ask %q", ErrInvalidArgument, a.QuestionID, askID)
			}
			if _, dup := answered[a.QuestionID]; dup {
				return fmt.Errorf("%w: question_id %q answered more than once in ask %q", ErrInvalidArgument, a.QuestionID, askID)
			}
			answered[a.QuestionID] = struct{}{}
			if err := validateQuestionAnswer(q, a, askID); err != nil {
				return err
			}
		}
		if len(answered) != len(ask.Questions) {
			return fmt.Errorf("%w: ask %q has %d questions but %d were answered", ErrInvalidArgument, askID, len(ask.Questions), len(answered))
		}
		// Coverage holds → record every answer in place and mark the ask
		// answered so a subsequent AnswerAsk is rejected.
		for _, a := range answers {
			q := byID[a.QuestionID]
			q.ChosenOptionIDs = a.ChosenOptionIDs
			q.CustomText = a.CustomText
		}
		ask.Answered = true
		return nil
	}
	// The containment query matched the message but no block carries the ask:
	// only possible if the row changed under us; treat as not found.
	return fmt.Errorf("%w: ask %q", ErrNotFound, askID)
}

// validateQuestionAnswer checks one answer against its question's offered option
// set and AllowMultiple arity. An empty entry (no chosen ids, empty custom_text)
// is an accepted skip and validates trivially. On a single-select question the
// free-text answer is exclusive with an option pick (comms.proto: custom_text is
// provided "instead of (or, for allow_multiple, alongside) picking options"), so
// an option and custom_text together are two answers to a single-select question
// and rejected.
func validateQuestionAnswer(q *AskQuestion, a AskAnswer, askID string) error {
	if len(a.ChosenOptionIDs) > 1 && !q.AllowMultiple {
		return fmt.Errorf("%w: question %q in ask %q is single-select but %d options were chosen", ErrInvalidArgument, a.QuestionID, askID, len(a.ChosenOptionIDs))
	}
	if len(a.ChosenOptionIDs) == 1 && a.CustomText != "" && !q.AllowMultiple {
		return fmt.Errorf("%w: question %q in ask %q is single-select but was answered with both an option and custom text", ErrInvalidArgument, a.QuestionID, askID)
	}
	offered := make(map[string]struct{}, len(q.Options))
	for _, o := range q.Options {
		offered[o.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a.ChosenOptionIDs))
	for _, id := range a.ChosenOptionIDs {
		if _, ok := offered[id]; !ok {
			return fmt.Errorf("%w: option %q is not offered by question %q in ask %q", ErrInvalidArgument, id, a.QuestionID, askID)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: option %q chosen more than once for question %q in ask %q", ErrInvalidArgument, id, a.QuestionID, askID)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// getMessageByRequestID returns the message stored under an author's idempotency
// key — the dedup path for a retried AppendMessage.
func (s *Store) getMessageByRequestID(ctx context.Context, author AccountID, clientRequestID string) (Message, error) {
	const q = `
		SELECT id, topic_id, author_account_id, at_unix_ms, blocks
		FROM messages
		WHERE author_account_id = $1 AND client_request_id = $2`
	rows, err := s.pool.Query(ctx, q, string(author), clientRequestID)
	if err != nil {
		return Message{}, fmt.Errorf("store: read deduped message: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("%w: deduped message for key %q", ErrNotFound, clientRequestID)
	}
	return msgs[0], nil
}

// scanMessages reads message rows into Messages, decoding the JSONB block set on
// each. Shared by the list, search, and dedup reads.
func scanMessages(rows pgx.Rows) ([]Message, error) {
	var msgs []Message
	for rows.Next() {
		var (
			id, topicID, author string
			atMS                int64
			blocksJSON          []byte
		)
		if err := rows.Scan(&id, &topicID, &author, &atMS, &blocksJSON); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		blocks, err := unmarshalBlocks(blocksJSON)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, Message{
			ID:              MessageID(id),
			TopicID:         topicID,
			AuthorAccountID: AccountID(author),
			At:              time.UnixMilli(atMS).UTC(),
			Blocks:          blocks,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate messages: %w", err)
	}
	return msgs, nil
}
