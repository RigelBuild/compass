//go:build unix

package presence

import (
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// PresenceFor returns the last-published presence for each requested agent that
// has one — the enum-only snapshot the roster read joins against (DL-074: the
// hub/projection presence map is ENUM ONLY; the activity string is durable in
// the store, never here). An agent with no published presence is OMITTED from
// the map; the caller (runnerhub.Hub.PresenceFor / the roster handler) defaults
// an absent agent to OFFLINE. A copy read under the lock, so the caller never
// touches publisher state. It is the read half the T2 roster join consumes; the
// existing PresenceSnapshot() returns the WHOLE map, this projects the requested
// subset.
func (p *Publisher) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[store.AccountID]compassv1.AgentPresence, len(accountIDs))
	for _, id := range accountIDs {
		if pr, ok := p.last[id]; ok {
			out[id] = pr
		}
	}
	return out
}

// PublishActivity publishes an AgentPresenceChanged carrying the agent's CURRENT
// presence enum plus the activity string — the publish-only live-UI hook the
// set_status write-through fires AFTER the durable Store.SetActivity commits
// (design.md T2:409-411, T3:473-486). It is NOT storage: the durable half is the
// store write, and a lost publish self-heals on the next set_status / reattach
// re-publish. Unlike publishIfChanged (deduped on presence) this ALWAYS
// publishes, because the activity string can change with the presence unchanged.
// An agent with no published presence publishes OFFLINE, matching the roster's
// absent-from-map posture.
func (p *Publisher) PublishActivity(agentAccountID store.AccountID, activity string) {
	p.mu.Lock()
	presence, ok := p.last[agentAccountID]
	p.mu.Unlock()
	if !ok {
		presence = compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
	}

	p.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AgentPresenceChanged{
			AgentPresenceChanged: &compassv1.AgentPresenceChanged{
				AgentAccountId: string(agentAccountID),
				Presence:       presence,
				Activity:       activity,
			},
		},
	})
}
