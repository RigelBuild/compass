//go:build pgtest && unix

package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// claimCounter counts, per row id, the refs one instance's delivery consumer
// was handed by the fabric; the real callback still runs after the count.
type claimCounter struct {
	fabric.EventFabric
	mu         sync.Mutex
	claims     map[string]int
	claimed    chan struct{}
	subscribed chan struct{}
	once       sync.Once
}

func newClaimCounter(fab fabric.EventFabric) *claimCounter {
	return &claimCounter{
		EventFabric: fab,
		claims:      make(map[string]int),
		claimed:     make(chan struct{}, 1024),
		subscribed:  make(chan struct{}),
	}
}

func (c *claimCounter) SubscribeKind(ctx context.Context, kind fabric.EventKind, fn func(context.Context, fabric.EventRef)) (fabric.Unsubscribe, error) {
	unsub, err := c.EventFabric.SubscribeKind(ctx, kind, func(ctx context.Context, ref fabric.EventRef) {
		c.mu.Lock()
		c.claims[ref.RowID]++
		c.mu.Unlock()
		c.claimed <- struct{}{}
		fn(ctx, ref)
	})
	if err == nil {
		c.once.Do(func() { close(c.subscribed) })
	}
	return unsub, err
}

func (c *claimCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.claims)
}

// deliveryInstance is one Server's delivery half: its own store pool, fabric
// connection, hub, and comms, with the consumer started by startDeliveryConsumer.
type deliveryInstance struct {
	comms  *comms.Comms
	claims *claimCounter
}

func startDeliveryInstance(t *testing.T, dsn, natsURL string, admin store.AccountID) *deliveryInstance {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	fab, err := fabric.New(fabric.Config{URL: natsURL})
	if err != nil {
		t.Fatalf("fabric.New: %v", err)
	}
	t.Cleanup(func() {
		if err := fab.Close(); err != nil {
			t.Errorf("fabric Close: %v", err)
		}
	})

	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, fab, admin)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	hub := newRunnerHub(st, board.NewProjection(bus), newSessionTail(), commsSvc, slog.New(slog.DiscardHandler))

	counter := newClaimCounter(fab)
	gctx, cancel := context.WithCancel(ctx)
	g, gctx := errgroup.WithContext(gctx)
	startDeliveryConsumer(gctx, g, counter, st, hub, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		cancel()
		if err := g.Wait(); err != nil {
			t.Errorf("delivery consumer Run: %v", err)
		}
	})
	return &deliveryInstance{comms: commsSvc, claims: counter}
}

// TestTwoInstancesClaimEachPostExactlyOnce proves the delivery trigger's
// transport claim semantics: two Servers sharing one NATS and one Postgres
// compete on one durable consumer, so every post is claimed exactly once in
// total. It does not assert which instance claims, nor delivery correctness.
func TestTwoInstancesClaimEachPostExactlyOnce(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	natsURL := startTestNats(t)

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	ch, err := st.CreateChannel(ctx, admin.ID, store.NewChannel{
		Name: "two-instance", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{admin.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	a := startDeliveryInstance(t, dsn, natsURL, admin.ID)
	b := startDeliveryInstance(t, dsn, natsURL, admin.ID)
	for name, inst := range map[string]*deliveryInstance{"a": a, "b": b} {
		select {
		case <-inst.claims.subscribed:
		case <-timeAfter():
			t.Fatalf("instance %s never subscribed to message_posted", name)
		}
	}

	const n = 20
	posted := make([]string, 0, n)
	for i := range n {
		resp, err := a.comms.PostAsAccount(ctx, admin.ID, &compassv1.PostMessageRequest{
			Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
			Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
			CreateTopic: true,
			Blocks:      textBlock(fmt.Sprintf("post %d", i)),
		})
		if err != nil {
			t.Fatalf("PostAsAccount %d: %v", i, err)
		}
		posted = append(posted, resp.GetMessage().GetId())
	}

	deadline := timeAfter()
	for got := range n {
		select {
		case <-a.claims.claimed:
		case <-b.claims.claimed:
		case <-deadline:
			t.Fatalf("saw %d claims, want at least %d", got, n)
		}
	}
	// Every consumer on the stream settled means no claim is still in flight.
	waitCommsConsumersSettled(t, natsURL)

	claimsA, claimsB := a.claims.snapshot(), b.claims.snapshot()
	for _, id := range posted {
		if total := claimsA[id] + claimsB[id]; total != 1 {
			t.Errorf("message %s claimed %d time(s) (a=%d, b=%d), want exactly 1", id, total, claimsA[id], claimsB[id])
		}
	}
	want := make(map[string]bool, n)
	for _, id := range posted {
		want[id] = true
	}
	for name, claims := range map[string]map[string]int{"a": claimsA, "b": claimsB} {
		for id, count := range claims {
			if !want[id] {
				t.Errorf("instance %s claimed %s %d time(s), which was never posted", name, id, count)
			}
		}
	}
	t.Logf("claims: a=%d, b=%d", len(claimsA), len(claimsB))
}

// waitCommsConsumersSettled polls until every consumer on the comms stream has
// nothing pending and nothing awaiting ack. There is no event for that state.
func waitCommsConsumersSettled(t *testing.T, natsURL string) {
	t.Helper()
	ctx := t.Context()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	stream, err := js.Stream(ctx, fabric.DefaultStreamName)
	if err != nil {
		t.Fatalf("Stream(%q): %v", fabric.DefaultStreamName, err)
	}
	deadline := timeAfter()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		settled, seen := true, 0
		lister := stream.ListConsumers(ctx)
		for info := range lister.Info() {
			seen++
			if info.NumPending > 0 || info.NumAckPending > 0 {
				settled = false
			}
		}
		if err := lister.Err(); err != nil {
			t.Fatalf("ListConsumers: %v", err)
		}
		if settled && seen > 0 {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("comms consumers did not settle within %s", testTimeout)
		}
	}
}
