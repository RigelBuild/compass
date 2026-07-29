package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppendMessage stores a new message in its channel, assigning the row id and
// timestamp (comms.proto:463-479). The blocks are serialized to JSONB and their
// text content extracted for the full-text index. When m carries a non-empty
// client_request_id, a retry with the same key returns the already-stored
// message rather than duplicating (idempotency, comms.proto:470-474). The
// returned bool reports whether a row was genuinely inserted: it is false on the
// idempotent-retry return, so the caller suppresses a duplicate MessagePosted
// fan-out for a row that did not change. A message with no blocks is
// ErrInvalidArgument, as is an unknown channel or a parent_message_id that
// names a message outside this channel. The membership check and the
// insert run in one transaction, so a membership revoked between them cannot
// slip a message into a channel the author can no longer read.
func (s *Store) AppendMessage(ctx context.Context, m Message, clientRequestID string) (Message, bool, error) {
	if m.Container.ChannelID == "" {
		return Message{}, false, fmt.Errorf("%w: message channel is required", ErrInvalidArgument)
	}
	if len(m.Blocks) == 0 {
		return Message{}, false, fmt.Errorf("%w: message has no blocks", ErrInvalidArgument)
	}
	mintAskIDs(m.Blocks)
	blocksJSON, err := marshalBlocks(m.Blocks)
	if err != nil {
		return Message{}, false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, false, fmt.Errorf("store: begin append message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// D9 write-authz: the author must be a member of the target channel, so a
	// non-member cannot persist (and fan out) into a private channel it can't
	// see — the write-side mirror of the ListMessages/AnswerAsk read gate. A
	// non-member gets ErrNotFound (the not-found/forbidden merge), never a hint
	// that the channel exists. Checked in the same tx as the insert so a
	// concurrent removal cannot race between the gate and the write.
	if err := requireChannelMember(ctx, tx, m.AuthorAccountID, m.Container.ChannelID); err != nil {
		return Message{}, false, err
	}

	// Same-channel threading: a reply's parent must live in the same channel as
	// the reply (spec: a message threads under an existing message in the
	// channel). Resolve the parent's channel in-tx and reject a cross-channel
	// OR unknown parent with one indistinguishable ErrInvalidArgument — the
	// author is already a verified member here, so a valid parent is necessarily
	// one they can see, and collapsing the two cases closes both a dangling
	// cross-channel thread reference and the existence oracle a bare FK check
	// would leave (whether the id exists in some other channel never leaks).
	if m.ParentMessageID != "" {
		var parentChannelID string
		switch err := tx.QueryRow(ctx,
			`SELECT channel_id FROM messages WHERE id = $1`,
			string(m.ParentMessageID),
		).Scan(&parentChannelID); {
		case noRows(err):
			return Message{}, false, fmt.Errorf("%w: parent message %q is not in this channel", ErrInvalidArgument, m.ParentMessageID)
		case err != nil:
			return Message{}, false, fmt.Errorf("store: resolve parent message: %w", err)
		}
		if parentChannelID != string(m.Container.ChannelID) {
			return Message{}, false, fmt.Errorf("%w: parent message %q is not in this channel", ErrInvalidArgument, m.ParentMessageID)
		}
	}

	id := newID()
	at := time.Now().UTC()
	const q = `
		INSERT INTO messages (id, channel_id, author_account_id, at_unix_ms, blocks, text_content, client_request_id, parent_message_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		ON CONFLICT (author_account_id, client_request_id) WHERE client_request_id <> ''
		DO NOTHING
		RETURNING id, at_unix_ms`
	var (
		storedID string
		atMS     int64
	)
	err = tx.QueryRow(ctx, q,
		id, string(m.Container.ChannelID), string(m.AuthorAccountID),
		at.UnixMilli(), blocksJSON, textContent(m.Blocks), clientRequestID, string(m.ParentMessageID),
	).Scan(&storedID, &atMS)
	switch {
	case noRows(err):
		// ON CONFLICT DO NOTHING suppressed the insert: a message with this
		// idempotency key already exists (a retry). Nothing was written, so the
		// tx rolls back; return the already-committed row with inserted=false so
		// the handler suppresses a duplicate MessagePosted.
		stored, err := s.getMessageByRequestID(ctx, m.AuthorAccountID, clientRequestID)
		return stored, false, err
	case pgErrIs(err, pgForeignKeyViolation):
		return Message{}, false, fmt.Errorf("%w: unknown channel %q or author %q", ErrInvalidArgument, m.Container.ChannelID, m.AuthorAccountID)
	case err != nil:
		return Message{}, false, fmt.Errorf("store: insert message: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Message{}, false, fmt.Errorf("store: commit append message: %w", err)
	}
	m.ID = MessageID(storedID)
	m.At = time.UnixMilli(atMS).UTC()
	return m, true, nil
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
// actor- or channel-scoped: a single instance-wide token is the frozen
// contract's ratified shape (design.md:809-816), store-space so it survives
// restarts and covers the empty-ring bootstrap. The subscribe path sends it
// before visibility filtering, so a subscriber learns the instance-wide durable
// message count (one integer, no content) even for channels it cannot see; this
// count-metadata exposure is accepted as within the threat model, not a leak to
// close by scoping the boundary — that would be a different token (SEA-1333 OQ4).
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

// ListMessages pages a channel's messages newest-first, clamped to the store's
// page bounds (comms.proto:446-461). Ordering keys on the monotonic seq (a
// stable total order even under equal timestamps); BeforeMessageID pages
// strictly before a given message. An unknown BeforeMessageID is
// ErrInvalidArgument (a cursor that isn't a real message).
//
// Visibility is enforced in SQL: the channel must be one the actor is a member
// of (JOIN channel_members), so a non-member — or a caller naming a channel it
// cannot see — reads nothing rather than leaking a private channel's history by
// id (the D9 not-found/forbidden merge, matching SearchMessages). The visibility
// gate is the store's, not the RPC edge's.
func (s *Store) ListMessages(ctx context.Context, actor AccountID, container ContainerRef, page Page) ([]Message, error) {
	if container.ChannelID == "" {
		return nil, fmt.Errorf("%w: list container channel is required", ErrInvalidArgument)
	}
	limit := clampLimit(page.Limit)

	var beforeSeq int64
	if page.BeforeMessageID != "" {
		// Scope the cursor probe to the actor's membership too, so a non-member
		// naming a real message in a channel it cannot see gets the same
		// "not in channel" result as a fake id — no existence oracle across the
		// visibility boundary (the D9 not-found/forbidden merge the main query and
		// AnswerAsk also apply).
		err := s.pool.QueryRow(ctx,
			`SELECT m.seq FROM messages m
			 JOIN channel_members cm ON cm.channel_id = m.channel_id AND cm.account_id = $1
			 WHERE m.id = $2 AND m.channel_id = $3`,
			string(actor), string(page.BeforeMessageID), string(container.ChannelID),
		).Scan(&beforeSeq)
		if err != nil {
			if noRows(err) {
				return nil, fmt.Errorf("%w: before-cursor %q not in channel", ErrInvalidArgument, page.BeforeMessageID)
			}
			return nil, fmt.Errorf("store: resolve page cursor: %w", err)
		}
	}

	// A zero beforeSeq (no cursor) reads the newest page; a positive one pages
	// strictly older. seq is BIGSERIAL starting at 1, so 0 is below every row.
	// The membership JOIN scopes the read to the actor's visible set. A non-zero
	// page.SnapshotSeq bounds the read to the point-in-time snapshot the client
	// captured on subscribe (seq <= SnapshotSeq, comms.proto:353-368,
	// design.md:807-817); zero reads the latest, no boundary.
	// The boundary is point-in-time on set membership (which messages the page
	// returns, by insert seq), not on content: a blocks update mutates m.blocks in
	// place without bumping m.seq, so a row present at the boundary but edited
	// mid-catch-up returns its post-boundary blocks. This is sufficient, not a
	// lost update — the matching MessageUpdated also rides the live tail, so an
	// id-deduping client converges to current content (last-write-wins).
	// Freezing content too would need an update/change-seq and a larger schema
	// change; membership-only is the ratified scope (SEA-1333 OQ5).
	const q = `
		SELECT m.id, m.channel_id, m.author_account_id, m.at_unix_ms, m.blocks, COALESCE(m.parent_message_id, '')
		FROM messages m
		JOIN channel_members cm ON cm.channel_id = m.channel_id AND cm.account_id = $1
		WHERE m.channel_id = $2 AND ($3 = 0 OR m.seq < $3) AND ($5 = 0 OR m.seq <= $5)
		ORDER BY m.seq DESC
		LIMIT $4`
	// seq is BIGSERIAL (the int64 domain) and SnapshotSeq is a server-issued
	// boundary the client echoes back, so the value is in range by construction;
	// an out-of-range client value degrades to an empty page (m.seq <= a negative
	// bound matches nothing), never a fault.
	snap := int64(page.SnapshotSeq) //nolint:gosec // G115: see the note above — server-issued seq, int64 domain
	rows, err := s.pool.Query(ctx, q, string(actor), string(container.ChannelID), beforeSeq, int64(limit), snap)
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
	// rows rather than erroring. Visibility: the message's channel must be one
	// the actor is a member of; the optional scope narrows within that set.
	const q = `
		SELECT m.id, m.channel_id, m.author_account_id, m.at_unix_ms, m.blocks, COALESCE(m.parent_message_id, '')
		FROM messages m
		JOIN channel_members cm ON cm.channel_id = m.channel_id AND cm.account_id = $1
		WHERE m.search_tsv @@ websearch_to_tsquery('english', $2)
		  AND ($3 = '' OR m.channel_id = $3)
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
// and persists via the immutable-ask_id update path, returning the updated
// message. The handler publishes MessageUpdated.
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
func (s *Store) AnswerAsk(ctx context.Context, actor AccountID, askID string, answers []AskAnswer) (Message, error) {
	if askID == "" {
		return Message{}, fmt.Errorf("%w: ask id is required", ErrInvalidArgument)
	}

	// Serialized find-and-answer: the whole read-modify-write runs in one
	// transaction with the matched message row locked FOR UPDATE, so two
	// concurrent answers to different asks on the SAME message can't lost-update
	// each other. Without the lock, each would read the pre-answer block set,
	// answer its own ask in that snapshot, and write the full set back — the
	// second commit clobbering the first's answer. The lock makes the second
	// answer block until the first commits, then re-read the updated blocks
	// (READ COMMITTED EvalPlanQual) and layer its own answer on top, so both
	// survive (SEA-1226).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("store: begin answer ask: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Visibility + existence in one gate: the message's channel must be one the
	// actor is a member of, and its blocks JSONB must contain an ask with askID.
	// Zero rows -> ErrNotFound (never a distinct not-authorized), so ask
	// existence cannot leak across a membership boundary. FOR UPDATE OF m locks
	// the message row (not the membership row) for the transaction's duration.
	const q = `
		SELECT m.id, m.channel_id, m.author_account_id, m.at_unix_ms, m.blocks, COALESCE(m.parent_message_id, '')
		FROM messages m
		JOIN channel_members cm ON cm.channel_id = m.channel_id AND cm.account_id = $1
		WHERE m.blocks @> $2::jsonb
		FOR UPDATE OF m`
	filter, err := askIDContainmentFilter(askID)
	if err != nil {
		return Message{}, fmt.Errorf("store: marshal ask filter: %w", err)
	}
	rows, err := tx.Query(ctx, q, string(actor), filter)
	if err != nil {
		return Message{}, fmt.Errorf("store: find ask: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("%w: ask %q", ErrNotFound, askID)
	}
	msg := msgs[0]

	// Locate the ask block and validate the answers cover its questions exactly,
	// each answer against its question's offered options and arity, then record
	// them in place.
	if err := applyAskAnswer(&msg, askID, answers); err != nil {
		return Message{}, err
	}
	if err := updateMessageBlocksExec(ctx, tx, msg.ID, msg.Blocks); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, fmt.Errorf("store: commit answer ask: %w", err)
	}
	return msg, nil
}

// AskAnswer is one participant answer to a single question within an ask, keyed
// by QuestionID. An answer with no chosen ids AND empty CustomText is a
// deliberate skip (accepted, not rejected).
type AskAnswer struct {
	QuestionID      string
	ChosenOptionIDs []string
	CustomText      string
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
		SELECT id, channel_id, author_account_id, at_unix_ms, blocks, COALESCE(parent_message_id, '')
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
			id, channelID, author string
			atMS                  int64
			blocksJSON            []byte
			parentID              string
		)
		if err := rows.Scan(&id, &channelID, &author, &atMS, &blocksJSON, &parentID); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		blocks, err := unmarshalBlocks(blocksJSON)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, Message{
			ID:              MessageID(id),
			Container:       ContainerRef{ChannelID: ChannelID(channelID)},
			AuthorAccountID: AccountID(author),
			At:              time.UnixMilli(atMS).UTC(),
			Blocks:          blocks,
			ParentMessageID: MessageID(parentID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate messages: %w", err)
	}
	return msgs, nil
}
