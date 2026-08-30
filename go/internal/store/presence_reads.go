package store

import (
	"context"
	"fmt"
)

// The presence component's read side (RIG-1569 T8, design record D4). Two pure
// reads back the server-side presence projection: the unanswered-authored-ask
// overlay (AgentHasOpenAsk) and the shared-channel visibility predicate the
// SubscribeComms edge scopes an AgentPresenceChanged by (SharesVisibleChannel).
// They live beside the delivery reads (delivery_reads.go) so the whole
// notification-delivery store surface is proven against real Postgres in the
// pgtest-tagged files.

// AgentHasOpenAsk reports whether agent has authored a message carrying an ask
// block that is not yet answered — the WAITING overlay input (design.md:449-457,
// :864-867). WAITING is layered on the lifecycle mapping: a live agent with an
// unanswered authored ask projects WAITING, overriding IDLE.
//
// The open-ask predicate is a JSONB PATH-EXISTENCE probe, not a containment
// (`@>`) match, because of the storage shape. An ask block stores as
// {"kind":"ask","ask":{"ask_id":...,"answered":true}} and Answered is omitempty
// (blocks.go:41, toStoredAsk) — so an UNANSWERED ask OMITS the answered field
// entirely. A naive containment `@> '[{"kind":"ask","ask":{"answered":false}]'`
// requires the field to be literally present as false and therefore MISSES every
// omitted-field (i.e. genuinely open) ask — the exact case that matters. The
// path probe instead matches an ask block whose answered is absent OR false, so
// both the omitted-field open ask and an explicit answered=false are caught,
// while a fully-answered ask (answered=true) is not.
//
// An agent with no authored asks, or only answered asks, is false — never an
// error for the ordinary "no open ask" case.
func (s *Store) AgentHasOpenAsk(ctx context.Context, agent AccountID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE author_account_id = $1
			  AND blocks @? '$[*] ? (@.kind == "ask" && (!exists(@.ask.answered) || @.ask.answered == false))'
		)`
	var open bool
	if err := s.pool.QueryRow(ctx, q, string(agent)).Scan(&open); err != nil {
		return false, fmt.Errorf("store: check agent open ask: %w", err)
	}
	return open, nil
}

// SharesVisibleChannel reports whether actor shares at least one channel with
// agent — the shared-channel rule the SubscribeComms edge scopes an
// AgentPresenceChanged by (design.md:487-491). It is the single-id form of the
// per-actor fan-out scoping, mirroring the other eventVisibility predicates
// (subscribe.go:240-257): an EXISTS over a self-join of channel_members on
// channel_id, one side the actor and the other the agent.
//
// MVP scoping (parked nuance): "visible channel" is implemented as bare shared
// membership — actor and agent are both member rows of the same channel. The
// record's "visible channel" wording could also fold in SHARED-grouped channels
// the actor sees without a membership row; the conservative shared-membership
// reading never leaks (an actor learns presence only for an agent it already
// co-inhabits a channel with) and is the MVP the brief pins, with the broader
// visibility nuance parked for the driver.
func (s *Store) SharesVisibleChannel(ctx context.Context, actor AccountID, agent AccountID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1
			FROM channel_members cm1
			JOIN channel_members cm2 ON cm2.channel_id = cm1.channel_id
			WHERE cm1.account_id = $1
			  AND cm2.account_id = $2
		)`
	var shares bool
	if err := s.pool.QueryRow(ctx, q, string(actor), string(agent)).Scan(&shares); err != nil {
		return false, fmt.Errorf("store: check shared visible channel: %w", err)
	}
	return shares, nil
}
