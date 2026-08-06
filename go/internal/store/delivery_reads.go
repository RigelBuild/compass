package store

import (
	"context"
	"fmt"
)

// The delivery consumer's read side (SEA-1569 T3, design record D1). These live
// beside the T2 cursor methods (delivery_cursors.go) so the fan-out consumer's
// store surface is proven against real Postgres in one pgtest-tagged file, the
// same discipline the cursor reads follow. All four are pure reads — the
// consumer never mutates through them (the only delivery mutation is
// AckDelivery, T2).

// SubscribedAgents resolves the agent accounts that should receive a message
// posted to channel, EXCLUDING the author (an agent never receives its own post
// back as a deliver). It is D1's one subscriber-resolution query: a member
// delivers when it
// is flagged subscribed, OR the channel is that agent's home channel, OR the
// channel is mandatory_subscription (T4, design.md:521-522) — every member of a
// mandatory channel is a delivery target regardless of its stored subscribed
// flag. The home-channel and mandatory disjuncts are frozen-model-fidelity
// repairs, not optimizations — a member row flipped subscribed=false MUST still
// deliver on a home or mandatory channel, so the query enforces the guarantee
// read-side, independent of the stored flag. The JOIN to agent_accounts is what
// scopes the result to AGENT members: a human member has no agent_accounts row
// and is excluded, so a deliver is only ever dispatched to an agent session. $1
// is the channel, $2 the author account excluded from the result.
func (s *Store) SubscribedAgents(ctx context.Context, channel ChannelID, author AccountID) ([]AccountID, error) {
	const q = `
		SELECT aa.account_id
		FROM channel_members cm
		JOIN agent_accounts aa ON aa.account_id = cm.account_id
		JOIN channels ch ON ch.id = cm.channel_id
		WHERE cm.channel_id = $1
		  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
		  AND cm.account_id <> $2
		ORDER BY aa.account_id`
	rows, err := s.pool.Query(ctx, q, string(channel), string(author))
	if err != nil {
		return nil, fmt.Errorf("store: resolve subscribed agents: %w", err)
	}
	defer rows.Close()

	var agents []AccountID
	for rows.Next() {
		var acct string
		if err := rows.Scan(&acct); err != nil {
			return nil, fmt.Errorf("store: scan subscribed agent: %w", err)
		}
		agents = append(agents, AccountID(acct))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscribed agents: %w", err)
	}
	return agents, nil
}

// ChannelAgentMembers resolves every AGENT member of a channel, author excluded,
// regardless of subscribe state — the mention→steer routing set (design.md:526-527:
// membership, not subscription). Distinct from SubscribedAgents, which is the plain
// deliver set (subscribed-or-home): this query is the same JOIN shape MINUS the
// `(cm.subscribed OR home_channel)` disjunct, so an unsubscribed non-home agent
// member is STILL returned. The JOIN to agent_accounts scopes the result to agent
// members (a human member has no agent_accounts row); $1 is the channel, $2 the
// author excluded (an agent's own `@agents` / self-mention never steers itself).
func (s *Store) ChannelAgentMembers(ctx context.Context, channel ChannelID, author AccountID) ([]AccountID, error) {
	const q = `
		SELECT aa.account_id
		FROM channel_members cm
		JOIN agent_accounts aa ON aa.account_id = cm.account_id
		WHERE cm.channel_id = $1
		  AND cm.account_id <> $2
		ORDER BY aa.account_id`
	rows, err := s.pool.Query(ctx, q, string(channel), string(author))
	if err != nil {
		return nil, fmt.Errorf("store: resolve channel agent members: %w", err)
	}
	defer rows.Close()

	var agents []AccountID
	for rows.Next() {
		var acct string
		if err := rows.Scan(&acct); err != nil {
			return nil, fmt.Errorf("store: scan channel agent member: %w", err)
		}
		agents = append(agents, AccountID(acct))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel agent members: %w", err)
	}
	return agents, nil
}

// IsAgentAccount reports whether account is an owned agent (has an agent_accounts
// row). The delivery settle gate (design.md:139-168) splits on it: a
// human-authored message is settled at post and delivers immediately, while an
// agent-authored message streams and is held until the author's turn-settle
// edge. A non-existent account is simply not an agent (false, no error) — the
// caller (the consumer, resolving a message's author) treats "not an agent" and
// "unknown" identically: deliver at post.
func (s *Store) IsAgentAccount(ctx context.Context, account AccountID) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = $1)`,
		string(account),
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: check agent account: %w", err)
	}
	return exists, nil
}

// MessageByID reads one message by its id, blocks included. The delivery
// consumer re-reads through it at the author's settle edge to dispatch the
// message's CURRENT (settled) block set (design.md:158-161) rather than a
// stale in-memory copy — the commit-lag-safe read the settle gate rides. An
// unknown id is ErrNotFound.
func (s *Store) MessageByID(ctx context.Context, messageID string) (Message, error) {
	const q = `
		SELECT id, topic_id, author_account_id, at_unix_ms, blocks
		FROM messages
		WHERE id = $1`
	rows, err := s.pool.Query(ctx, q, messageID)
	if err != nil {
		return Message{}, fmt.Errorf("store: read message by id: %w", err)
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("%w: message %q", ErrNotFound, messageID)
	}
	return msgs[0], nil
}

// MessageChannel resolves a message id to its channel — the ack arm's channel
// resolution (design.md, T3 §6). A DeliveryAck frame carries only the
// message_id, but AckDelivery is keyed (agent, channel, message_id); resolving
// the channel by message id (rather than threading it through in-memory
// dispatch state) survives a Server restart that lost the pending-dispatch
// bookkeeping — the ack can arrive after such a restart. An unknown id is
// ErrNotFound, which the ack arm treats as a fail-closed no-op (a foreign or
// fabricated ack never advances a cursor).
func (s *Store) MessageChannel(ctx context.Context, messageID string) (ChannelID, error) {
	var channel string
	if err := s.pool.QueryRow(ctx,
		`SELECT t.channel_id FROM messages m JOIN topics t ON t.id = m.topic_id WHERE m.id = $1`, messageID,
	).Scan(&channel); err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: message %q", ErrNotFound, messageID)
		}
		return "", fmt.Errorf("store: resolve message channel: %w", err)
	}
	return ChannelID(channel), nil
}
