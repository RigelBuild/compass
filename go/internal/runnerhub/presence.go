//go:build unix

package runnerhub

import (
	"context"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// PresenceSnapshot is the enum-only presence read the roster join consumes for
// one agent (DL-074: presence stays an in-memory ENUM; the durable activity
// string lives in the store's agent_activity table, never here). A struct rather
// than a bare enum so a later live-only presence attribute (e.g. a last-seen ms)
// can join here without changing every PresenceFor caller.
type PresenceSnapshot struct {
	Presence compassv1.AgentPresence
}

// presenceSource reads the in-memory presence projection and publishes activity events.
// It is separate from PresenceSink, which receives lifecycle updates; missing agents are OFFLINE.
type presenceSource interface {
	PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence
	PublishActivity(ctx context.Context, agentAccountID store.AccountID, activity string)
}

// SetPresenceSource wires the T8 presence projection as the hub's presence READ
// source, AFTER both exist — the post-construction setter that breaks the
// component<->hub construction cycle, exactly as SetPresenceSink does for the
// write edge. Called once at server assembly; safe to leave unset (a hub with no
// presence source reports every agent OFFLINE and drops the activity publish —
// today's un-wired behavior, and what a hub-less unit test sees).
func (h *Hub) SetPresenceSource(src presenceSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.presenceSource = src
}

// PresenceFor returns the live presence enum for each requested agent, defaulting
// an agent absent from the in-memory projection to OFFLINE (DL-074's restart
// posture: presence is ephemeral, so an unknown agent is offline until it
// re-enrolls). It is the hub-side surface the roster read (comms.GetRoster)
// joins the tree + durable-activity sources against. A hub with no presence
// source wired reports every agent OFFLINE.
func (h *Hub) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]PresenceSnapshot {
	h.mu.Lock()
	src := h.presenceSource
	h.mu.Unlock()

	out := make(map[store.AccountID]PresenceSnapshot, len(accountIDs))
	var known map[store.AccountID]compassv1.AgentPresence
	if src != nil {
		known = src.PresenceFor(accountIDs)
	}
	for _, id := range accountIDs {
		presence := compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
		if p, ok := known[id]; ok {
			presence = p
		}
		out[id] = PresenceSnapshot{Presence: presence}
	}
	return out
}

// PublishActivity publishes an AgentPresenceChanged{presence, activity} onto the
// comms bus for a live UI — the publish-only hook the set_status write-through
// fires AFTER the durable Store.SetActivity commits (design.md T2:409-411). It is
// NOT storage; a lost publish self-heals on the next set_status. A hub with no
// presence source wired drops it (best-effort, matching the durability contract:
// the table is the source of record).
func (h *Hub) PublishActivity(ctx context.Context, agentAccountID store.AccountID, activity string) {
	h.mu.Lock()
	src := h.presenceSource
	h.mu.Unlock()
	if src != nil {
		src.PublishActivity(ctx, agentAccountID, activity)
	}
}
