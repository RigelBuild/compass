//go:build podman

package e2e

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Everything is t3- namespaced: this leg shares ONE stack with the other comms
// legs, so a bare name would collide with theirs.
const (
	t3PosterHandle   = "t3-poster"
	t3Sub1Handle     = "t3-sub-1"
	t3Sub2Handle     = "t3-sub-2"
	t3Sub3Handle     = "t3-sub-3"
	t3OutsiderHandle = "t3-outsider"

	t3Chan1     = "t3-chan-1"
	t3Chan2     = "t3-chan-2"
	t3Topic1    = "t3-topic-1"
	t3Topic2    = "t3-topic-2"
	t3TopicMiss = "t3-topic-miss"
)

// Routing by marker — never the positional script — is mandatory on the shared
// stack: two legs drawing unmarked turns would race the one positional counter.
// This substring appears in every trigger posted to the poster and in no other
// leg's marker.
const t3FanoutMarker = "t3-fanout-drive"

// Each body is unique so an AwaitDelivery match cannot collide with a trigger or
// another post, and none carries the marker: the poster is a bare member of its
// own channels, so its posts never fan back to re-route its turns.
const (
	t3FanBody        = "t3 fan-out post on channel one topic one"
	t3MissBody       = "t3 name-miss post without create_topic"
	t3MintCanaryBody = "t3 mint-negative canary on existing topic one"
	t3Topic2Body     = "t3 cross-topic post on channel one topic two"
	t3Chan2Body      = "t3 cross-channel canary on channel two"
	t3Settle         = "t3 poster standing by"
)

func init() {
	// The poster's ordered script: five comms_post_message tool calls, each
	// paired with a following text turn so the tool-call turn's second round-trip
	// terminates. create_topic mirrors the R2/R5 contract (legcomms_test.go, "As
	// of the peer-DM cutover"): channel is required, a name-miss topic mints only
	// with create_topic:true.
	fanArgs := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
		t3FanBody, t3Topic1, t3Chan1,
	)
	// The minting negative: a name-miss topic WITHOUT create_topic must not mint
	// or fan out.
	missArgs := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":false}`,
		t3MissBody, t3TopicMiss, t3Chan1,
	)
	// topic-1 exists now, so this posts without minting — the canary that proves
	// the miss above never fanned ahead of it.
	mintCanaryArgs := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":false}`,
		t3MintCanaryBody, t3Topic1, t3Chan1,
	)
	topic2Args := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
		t3Topic2Body, t3Topic2, t3Chan1,
	)
	// The chan-2 canary: topic-1's NAME is fresh in chan-2 (topics are per
	// channel), so it mints here.
	chan2Args := fmt.Sprintf(
		`{"text":%q,"topic":%q,"channel":%q,"create_topic":true}`,
		t3Chan2Body, t3Topic1, t3Chan2,
	)

	registerSharedFixtureOption(
		WithCannedMarkerScript(t3FanoutMarker,
			CannedToolCall("comms_post_message", fanArgs),
			CannedText(t3Settle),
			CannedToolCall("comms_post_message", missArgs),
			CannedText(t3Settle),
			CannedToolCall("comms_post_message", mintCanaryArgs),
			CannedText(t3Settle),
			CannedToolCall("comms_post_message", topic2Args),
			CannedText(t3Settle),
			CannedToolCall("comms_post_message", chan2Args),
			CannedText(t3Settle),
		),
	)
}

// TestCommsFanOutAndIsolation proves comms fan-out and isolation over the real
// stack. The subscribers observe over their OWN bearers, not the admin one, so
// the per-event D9 filter runs against each stream's account — an all-seeing
// admin stream could not carry a negative visibility claim at all.
func TestCommsFanOutAndIsolation(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into sharedFixture + every primitive

	f := sharedFixture(t)

	// The one container agent. Its reap is registered BEFORE StartSession: the
	// reparented rootless conmon outlives stack Down, so RemoveWorkspace is the
	// only reliable reap, and registering early survives a later t.Fatal.
	posterID, err := f.CreateAgent(ctx, t3PosterHandle, "T3 Poster")
	if err != nil {
		t.Fatalf("CreateAgent(poster): %v", err)
	}
	container, err := f.Provision(ctx, posterID, "t3-poster-provision")
	if err != nil {
		t.Fatalf("Provision(poster): %v", err)
	}
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, container, "t3-poster-teardown") // best-effort reap
	})
	session, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession(poster): %v", err)
	}

	// The three subscribers and the outsider are accounts only — membership plus
	// an observer stream needs no container, which is what keeps this leg cheap.
	sub1ID := t3CreateAgent(ctx, t, f, t3Sub1Handle, "T3 Subscriber One")
	sub2ID := t3CreateAgent(ctx, t, f, t3Sub2Handle, "T3 Subscriber Two")
	sub3ID := t3CreateAgent(ctx, t, f, t3Sub3Handle, "T3 Subscriber Three")
	outsiderID := t3CreateAgent(ctx, t, f, t3OutsiderHandle, "T3 Outsider")

	// Both channels are PRIVATE (ungrouped): membership-only visibility, so a
	// non-member's observer stream is genuinely denied and the isolation
	// assertions bite the filter rather than a grouping accident.
	chan1ID, err := f.CreateChannel(ctx, posterID, t3Chan1, true)
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", t3Chan1, err)
	}
	chan2ID, err := f.CreateChannel(ctx, posterID, t3Chan2, true)
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", t3Chan2, err)
	}
	for _, h := range []string{t3Sub1Handle, t3Sub2Handle, t3Sub3Handle} {
		if err := f.SubscribeMember(ctx, chan1ID, h); err != nil {
			t.Fatalf("SubscribeMember(%q -> chan-1): %v", h, err)
		}
	}
	if err := f.SubscribeMember(ctx, chan2ID, t3OutsiderHandle); err != nil {
		t.Fatalf("SubscribeMember(outsider -> chan-2): %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// The poster's home channel: the trigger posts land there to drive its turns.
	poster, err := adminAgentByHandle(ctx, st, t3PosterHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(poster): %v", err)
	}

	// Opened before any trigger: OpenSessionTail returns on the registration-ack,
	// a server-guaranteed happens-before, so a fast turn's edges cannot fan into
	// a subscribe gap.
	tail, err := f.OpenSessionTail(ctx, session)
	if err != nil {
		t.Fatalf("OpenSessionTail(poster): %v", err)
	}
	defer tail.Close()

	// One observer stream per subscriber, each on the subscriber's OWN bearer and
	// gated live before any post, so a post cannot be missed and every delivery
	// below is the per-stream filter admitting it.
	sub1Stream := t3ObserverStream(ctx, t, f, sub1ID)
	defer sub1Stream.Close()
	sub2Stream := t3ObserverStream(ctx, t, f, sub2ID)
	defer sub2Stream.Close()
	sub3Stream := t3ObserverStream(ctx, t, f, sub3ID)
	defer sub3Stream.Close()
	outsiderStream := t3ObserverStream(ctx, t, f, outsiderID)
	defer outsiderStream.Close()

	// Drive the poster's five posts, one trigger per marker round-trip.
	for i, desc := range []string{
		"fan-out post",
		"name-miss post without create_topic",
		"mint-negative canary",
		"cross-topic post",
		"cross-channel canary",
	} {
		trigger := t3FanoutMarker + ": " + desc
		if _, err := f.PostMessage(ctx, string(poster.Agent.HomeChannelID), "general", trigger); err != nil {
			t.Fatalf("PostMessage(trigger %d %q): %v", i, desc, err)
		}
		if err := f.AwaitTurnSettled(ctx, tail); err != nil {
			t.Fatalf("AwaitTurnSettled(trigger %d %q): %v", i, desc, err)
		}
	}

	// ── Assertion 1: fan-out reaches all three member streams ────────────────
	// The three sub1Stream assertions (fan → mint canary → topic-2) MUST stay in
	// stream-seq order: AwaitDelivery consumes forward, so reordering them would
	// discard a later assertion's frame and time it out.
	fan1 := t3AwaitBody(ctx, t, f, sub1Stream, t3FanBody, "sub-1")
	if got := fan1.GetAuthorAccountId(); got != posterID {
		t.Fatalf("fan-out post author = %q, want the poster's account id %q", got, posterID)
	}
	fan2 := t3AwaitBody(ctx, t, f, sub2Stream, t3FanBody, "sub-2")
	if got := fan2.GetAuthorAccountId(); got != posterID {
		t.Fatalf("sub-2 fan-out post author = %q, want the poster's account id %q", got, posterID)
	}
	fan3 := t3AwaitBody(ctx, t, f, sub3Stream, t3FanBody, "sub-3")
	if got := fan3.GetAuthorAccountId(); got != posterID {
		t.Fatalf("sub-3 fan-out post author = %q, want the poster's account id %q", got, posterID)
	}

	// ── Assertion 3: name-miss without create_topic never minted/fanned ──────
	// The match admits either body, so AwaitDelivery returns whichever fanned
	// FIRST. In-order per-subscriber delivery means a wrongly-minted miss (posted
	// before the canary) would arrive AHEAD of it. So "the canary came first" is
	// a positive ordering fact — no sleep, no timeout proves this absence.
	firstMint, err := f.AwaitDelivery(ctx, sub1Stream, func(m *compassv1.Message) bool {
		return firstBlockText(m) == t3MissBody || firstBlockText(m) == t3MintCanaryBody
	})
	if err != nil {
		t.Fatalf("sub-1 stream carried neither the miss nor the mint canary: %v — the canary never arrived, so the negative is unproven", err)
	}
	if got := firstBlockText(firstMint); got == t3MissBody {
		t.Fatalf("MINT LEAK: a name-miss topic posted WITHOUT create_topic minted and fanned out on chan-1 — it arrived ahead of the canary %q", t3MintCanaryBody)
	} else if got != t3MintCanaryBody {
		t.Fatalf("sub-1 first mint-test event body = %q, want the mint canary %q", got, t3MintCanaryBody)
	}

	// ── Assertion 4: topic-2 traffic is isolated from topic-1 ────────────────
	topic2Msg := t3AwaitBody(ctx, t, f, sub1Stream, t3Topic2Body, "sub-1")
	if topic2Msg.GetTopicId() == fan1.GetTopicId() {
		t.Fatalf("cross-topic leak: topic-2 post shares topic id %q with the topic-1 fan-out post; the two topics are not distinct", topic2Msg.GetTopicId())
	}

	// ── Assertion 2: chan-1 post never reaches the chan-2-only outsider ──────
	// Same canary discipline: the outsider is not a chan-1 member, so the chan-1
	// fan-out post must never precede the chan-2 canary on its stream.
	firstOut, err := f.AwaitDelivery(ctx, outsiderStream, func(m *compassv1.Message) bool {
		return firstBlockText(m) == t3FanBody || firstBlockText(m) == t3Chan2Body
	})
	if err != nil {
		t.Fatalf("outsider stream carried neither the chan-1 post nor the chan-2 canary: %v — the high-water mark never arrived, so the negative is unproven", err)
	}
	if got := firstBlockText(firstOut); got == t3FanBody {
		t.Fatalf("CROSS-CHANNEL LEAK: the outsider (a chan-2-only member) received the chan-1 post %q ahead of the chan-2 canary %q; membership-gated visibility is not holding", t3FanBody, t3Chan2Body)
	} else if got != t3Chan2Body {
		t.Fatalf("outsider first event body = %q, want the chan-2 canary %q", got, t3Chan2Body)
	}
}

// t3CreateAgent creates a non-author subscriber account (no container) and fatals
// on failure — a setup miss makes every assertion below meaningless.
func t3CreateAgent(ctx context.Context, t *testing.T, f *Fixture, handle, displayName string) string {
	t.Helper()
	id, err := f.CreateAgent(ctx, handle, displayName)
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	return id
}

// t3ObserverStream opens accountID's own comms subscription and drains the
// snapshot-boundary frame, so the stream is provably live server-side before any
// post — the per-account bearer is what lets a NEGATIVE visibility claim stand.
func t3ObserverStream(ctx context.Context, t *testing.T, f *Fixture, accountID string) *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse] {
	t.Helper()
	_, comms, err := f.AsObserver(ctx, accountID)
	if err != nil {
		t.Fatalf("AsObserver(%s): %v", accountID, err)
	}
	stream, err := f.SubscribeCommsAsObserver(ctx, comms, 0)
	if err != nil {
		t.Fatalf("SubscribeCommsAsObserver(%s): %v", accountID, err)
	}
	awaitSubscriptionLive(ctx, t, stream)
	return stream
}

// t3AwaitBody waits for the message whose first block is body and fatals if it
// never fans, naming the stream so a failure points at the right subscriber.
func t3AwaitBody(ctx context.Context, t *testing.T, f *Fixture, stream *connect.ServerStreamForClient[compassv1.SubscribeCommsResponse], body, who string) *compassv1.Message {
	t.Helper()
	m, err := f.AwaitDelivery(ctx, stream, func(msg *compassv1.Message) bool {
		return firstBlockText(msg) == body
	})
	if err != nil {
		t.Fatalf("%s stream never carried %q: %v", who, body, err)
	}
	return m
}
