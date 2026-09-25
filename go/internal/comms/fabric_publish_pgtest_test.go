//go:build pgtest

package comms

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/store"
)

// publishedRef is one Publish call the fake fabric recorded, plus whether the
// named row was already committed when it arrived.
type publishedRef struct {
	subject   string
	ref       fabric.EventRef
	committed bool
}

// fakeEventFabric records publishes and, for each one, re-reads the row so a
// test can prove the publish happened after the commit, as the consumer needs.
type fakeEventFabric struct {
	st  *store.Store
	err error

	mu  sync.Mutex
	got []publishedRef
}

func (f *fakeEventFabric) Publish(ctx context.Context, subject string, ref fabric.EventRef) error {
	_, readErr := f.st.MessageByID(store.WithSystemRole(ctx), ref.RowID)
	f.mu.Lock()
	f.got = append(f.got, publishedRef{subject: subject, ref: ref, committed: readErr == nil})
	f.mu.Unlock()
	return f.err
}

func (f *fakeEventFabric) Subscribe(context.Context, string, func(fabric.EventRef)) (fabric.Unsubscribe, error) {
	return nil, errors.New("fakeEventFabric: Subscribe not used by comms")
}

func (f *fakeEventFabric) SubscribeKind(context.Context, fabric.EventKind, func(fabric.EventRef)) (fabric.Unsubscribe, error) {
	return nil, errors.New("fakeEventFabric: SubscribeKind not used by comms")
}

func (f *fakeEventFabric) published() []publishedRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishedRef(nil), f.got...)
}

// newFabricHandler builds a handler over a real store with fab wired in.
func newFabricHandler(t *testing.T, fabErr error) (*Comms, *store.Store, *fakeEventFabric) {
	t.Helper()
	st := newTestStore(t)
	admin, err := st.BootstrapAdmin(context.Background(), store.NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	fab := &fakeEventFabric{st: st, err: fabErr}
	return NewComms(st, newBus(t), fab, admin.ID), st, fab
}

func postText(ctx context.Context, t *testing.T, svc *Comms, actor store.AccountID, ch store.ChannelID, body, requestID string) string {
	t.Helper()
	resp, err := svc.PostMessage(WithActor(ctx, actor), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch)},
		Topic:           &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic:     true,
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
		ClientRequestId: requestID,
	}))
	if err != nil {
		t.Fatalf("PostMessage(%q): %v", body, err)
	}
	return resp.Msg.GetMessage().GetId()
}

func assertOnePostedRef(t *testing.T, st *store.Store, fab *fakeEventFabric, wantRowID string) {
	t.Helper()
	got := fab.published()
	if len(got) != 1 {
		t.Fatalf("fabric received %d publishes, want exactly 1: %+v", len(got), got)
	}
	tenant := string(st.EffectiveTenant(context.Background()))
	wantSubject, err := fabric.CommsSubject(tenant, fabric.KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	want := fabric.EventRef{Tenant: tenant, Kind: fabric.KindMessagePosted, RowID: wantRowID}
	if got[0].ref != want || got[0].subject != wantSubject {
		t.Fatalf("published %q %+v, want %q %+v", got[0].subject, got[0].ref, wantSubject, want)
	}
	if !got[0].committed {
		t.Fatal("fabric publish arrived before the message row was committed")
	}
}

func TestPostMessagePublishesPostedRefOnFabric(t *testing.T) {
	svc, st, fab := newFabricHandler(t, nil)
	ctx := context.Background()
	poster := mustUser(t, st, "poster")
	ch, err := st.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	id := postText(ctx, t, svc, poster.ID, ch.ID, "hello", "")
	assertOnePostedRef(t, st, fab, id)
}

func TestPostMessageIdempotentRetryDoesNotRepublishOnFabric(t *testing.T) {
	svc, st, fab := newFabricHandler(t, nil)
	ctx := context.Background()
	poster := mustUser(t, st, "poster")
	ch, err := st.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	id := postText(ctx, t, svc, poster.ID, ch.ID, "once", "req-dup")
	if retry := postText(ctx, t, svc, poster.ID, ch.ID, "again", "req-dup"); retry != id {
		t.Fatalf("retry returned %q, want the stored %q", retry, id)
	}
	assertOnePostedRef(t, st, fab, id)
}

func TestPostMessageSurvivesFabricPublishFailureAndCountsIt(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	otel.SetMeterProvider(mp)

	svc, st, fab := newFabricHandler(t, errors.New("nats unavailable"))
	ctx := context.Background()
	poster := mustUser(t, st, "poster")
	ch, err := st.CreateChannel(ctx, poster.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// postText fails the test if the RPC returns an error, which is the point.
	id := postText(ctx, t, svc, poster.ID, ch.ID, "committed anyway", "")
	assertOnePostedRef(t, st, fab, id)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "compass.delivery.fabric_publish_failures" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("fabric_publish_failures data = %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	if total != 1 {
		t.Fatalf("compass.delivery.fabric_publish_failures = %d, want 1", total)
	}
}

func TestRespondToAskPublishesAnswerRefOnFabric(t *testing.T) {
	svc, st, fab := newFabricHandler(t, nil)
	ctx := context.Background()
	agent := mustUser(t, st, "agent")
	ch, err := st.CreateChannel(ctx, agent.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	askMsg, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: agent.ID, Blocks: []store.MessageBlock{pendingAskStore("ask-1")}}, string(ch.ID), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if got := fab.published(); len(got) != 0 {
		t.Fatalf("a store-level append published %d refs, want 0", len(got))
	}

	if _, err := svc.RespondToAsk(WithActor(ctx, agent.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-1",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}
	got := fab.published()
	if len(got) != 1 {
		t.Fatalf("fabric received %d publishes, want exactly 1 for the answer", len(got))
	}
	if got[0].ref.RowID == string(askMsg.ID) {
		t.Fatal("published the ask message, want the new answer message")
	}
	answer, err := st.MessageByID(store.WithSystemRole(ctx), got[0].ref.RowID)
	if err != nil {
		t.Fatalf("published row %q is not a stored message: %v", got[0].ref.RowID, err)
	}
	if len(answer.Blocks) != 1 || answer.Blocks[0].AskAnswer == nil {
		t.Fatalf("published row %q is not an ask_answer message: %+v", answer.ID, answer.Blocks)
	}
	assertOnePostedRef(t, st, fab, string(answer.ID))
}
