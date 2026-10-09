//go:build pgtest && unix

package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newForgeSubscribeNotifyEndpoint mounts the GitHub notify lane on the forge wire's hub.
func newForgeSubscribeNotifyEndpoint(t *testing.T, w *forgeE2EWire) (string, []byte) {
	t.Helper()
	w.hub.SetDeliveryStore(w.store)
	secret := []byte("forge-subscribe-e2e-webhook-secret")
	log := slog.New(slog.DiscardHandler)
	secretFn := func(context.Context) ([]byte, error) { return secret, nil }
	router := ingest.NewNotifyRouter(
		&forgeNotifyStore{st: w.store, provider: store.ForgeProviderGitHub, host: forgeE2EHost},
		&forgeNotifyDispatcher{hub: w.hub},
		&matrixChecksRoller{res: forge.ConditionalResult[forge.Checks]{V: forge.Checks{State: "success"}}},
		nil,
		&forgeIdentityResolver{st: w.store, provider: store.ForgeProviderGitHub, host: forgeE2EHost},
		mxRef(),
		log,
	)
	arm := ingest.NewNotifyWebhookArm(router, ingest.NotifyArmConfig{Log: log})
	runCtx, cancel := context.WithCancel(w.ctx)
	runResult := make(chan error, 1)
	go func() { runResult <- arm.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runResult:
			if err != nil {
				t.Errorf("notify arm Run: %v", err)
			}
		case <-time.After(e2eTimeout):
			t.Error("notify arm did not stop after cancellation")
		}
	})

	path, handler := NewGitHubWebhookHandler(secretFn, arm, log)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, secret
}

// openForgeSubscribeControl reads through replay_complete before observing live notifications.
func openForgeSubscribeControl(t *testing.T, w *forgeE2EWire) (context.Context, *connect.ServerStreamForClient[compassv1internal.AgentControl]) {
	t.Helper()
	ctx, cancel := context.WithTimeout(w.ctx, e2eTimeout)
	t.Cleanup(cancel)
	stream, err := w.supervisorClient.Control(ctx, connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	t.Cleanup(func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close Control stream: %v", err)
		}
	})
	for {
		if !stream.Receive() {
			t.Fatalf("Control ended before replay_complete: %v", stream.Err())
		}
		if stream.Msg().GetReplayComplete() != nil {
			return ctx, stream
		}
	}
}

// receiveForgeSubscribeNotification skips unrelated ops until a forge notification arrives.
func receiveForgeSubscribeNotification(t *testing.T, stream *connect.ServerStreamForClient[compassv1internal.AgentControl]) *compassv1internal.ForgeNotification {
	t.Helper()
	for {
		if !stream.Receive() {
			t.Fatalf("Control ended before forge notification: %v", stream.Err())
		}
		if notification := stream.Msg().GetForgeNotification(); notification != nil {
			return notification
		}
	}
}

// postForgeSubscribeGitHub posts a signed webhook through the mounted ingress.
func postForgeSubscribeGitHub(t *testing.T, parent context.Context, endpoint string, webhook signedWebhook) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, e2eTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint+githubWebhookPath,
		bytes.NewReader(webhook.body),
	)
	if err != nil {
		t.Fatalf("build GitHub webhook request: %v", err)
	}
	request.Header.Set(githubEventHeader, webhook.event)
	request.Header.Set(githubDeliveryHeader, webhook.delivery)
	request.Header.Set(githubSignatureHeader, webhook.sig)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST GitHub webhook: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Errorf("close GitHub webhook response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GitHub webhook status = %d, want 200", response.StatusCode)
	}
}

// ackForgeNotificationAndAssertCursor acks over the socket and checks the delivery cursor moved.
func ackForgeNotificationAndAssertCursor(t *testing.T, ctx context.Context, w *forgeE2EWire, subscriptionID, revision string) {
	t.Helper()
	publish := w.supervisorClient.Publish(ctx)
	if err := publish.Send(&compassv1internal.PublishFrameRequest{Frame: &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ForgeNotificationAck{ForgeNotificationAck: &compassv1internal.ForgeNotificationAck{
			SubscriptionId: subscriptionID,
			Revision:       revision,
		}},
	}}); err != nil {
		t.Fatalf("send forge notification ack: %v", err)
	}
	if _, err := publish.CloseAndReceive(); err != nil {
		t.Fatalf("close Publish after forge notification ack: %v", err)
	}
	subscribers, err := w.store.SubscribersForArtifact(ctx, store.ForgeProviderGitHub, forgeE2EHost, forgeE2ERepo, store.ForgeArtifactKindIssue, 11, "", false)
	if err != nil {
		t.Fatalf("SubscribersForArtifact after ack: %v", err)
	}
	for _, subscriber := range subscribers {
		if subscriber.SubscriptionID == subscriptionID {
			if subscriber.DeliveredRevision != revision {
				t.Fatalf("delivered_revision = %q, want notified revision %q", subscriber.DeliveredRevision, revision)
			}
			return
		}
	}
	t.Fatalf("subscription %q not found after ack", subscriptionID)
}

// TestForgeSubscribeOverTheWire covers the socket subscription and notification lifecycle.
func TestForgeSubscribeOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	endpoint, secret := newForgeSubscribeNotifyEndpoint(t, w)
	ctx, control := openForgeSubscribeControl(t, w)
	github := newFakeGitHubForge(secret, forgeE2ERepo)

	subscribe := func(callID string, number uint64) *compassv1internal.SubscribeForgeResponse {
		t.Helper()
		response, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
			CallId: callID,
			Call: &compassv1internal.ForgeCallRequest_Subscribe{Subscribe: &compassv1internal.SubscribeForgeRequest{
				Repo: forgeE2ERepo, Kind: mxIssue, Number: number,
			}},
		}))
		if err != nil {
			t.Fatalf("Forge subscribe %s: %v", callID, err)
		}
		result := response.Msg.GetSubscribed()
		if result == nil || result.GetSubscriptionId() == "" {
			t.Fatalf("Forge subscribe %s result = %v, want subscription id", callID, response.Msg.GetResult())
		}
		return result
	}

	issue11 := subscribe("subscribe-issue-11", 11).GetSubscriptionId()
	postForgeSubscribeGitHub(t, w.ctx, endpoint, github.commentOnIssue(
		t,
		11,
		"https://github.com/owner/repo/issues/11#comment-1",
		"hello",
		"octocat",
	))
	notification := receiveForgeSubscribeNotification(t, control)
	if notification.GetSubscriptionId() != issue11 {
		t.Errorf("subscription id = %q, want %q", notification.GetSubscriptionId(), issue11)
	}
	if notification.GetChange() != mxComment {
		t.Errorf("change = %v, want COMMENT", notification.GetChange())
	}
	if notification.GetRepo() != forgeE2ERepo || notification.GetNumber() != 11 {
		t.Errorf("coordinate = %s#%d, want %s#11", notification.GetRepo(), notification.GetNumber(), forgeE2ERepo)
	}
	if notification.GetComment().GetForgeAccount() != "octocat" {
		t.Errorf("comment author = %q, want octocat", notification.GetComment().GetForgeAccount())
	}
	if notification.GetRevision() == "" {
		t.Fatal("notification revision is empty")
	}

	ackForgeNotificationAndAssertCursor(t, ctx, w, issue11, notification.GetRevision())

	unsubscribed, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
		CallId: "unsubscribe-issue-11",
		Call:   &compassv1internal.ForgeCallRequest_Unsubscribe{Unsubscribe: &compassv1internal.UnsubscribeForgeRequest{SubscriptionId: issue11}},
	}))
	if err != nil {
		t.Fatalf("Forge unsubscribe: %v", err)
	}
	if unsubscribed.Msg.GetUnsubscribed() == nil {
		t.Fatalf("Forge unsubscribe result = %v, want unsubscribed", unsubscribed.Msg.GetResult())
	}
	issue12 := subscribe("subscribe-issue-12", 12).GetSubscriptionId()

	// The later issue-12 delivery is an ordered-drain barrier for the preceding issue-11 webhook.
	postForgeSubscribeGitHub(t, w.ctx, endpoint, github.commentOnIssue(
		t,
		11,
		"https://github.com/owner/repo/issues/11#comment-2",
		"after unsubscribe",
		"octocat",
	))
	postForgeSubscribeGitHub(t, w.ctx, endpoint, github.commentOnIssue(
		t,
		12,
		"https://github.com/owner/repo/issues/12#comment-3",
		"barrier",
		"octocat",
	))
	barrier := receiveForgeSubscribeNotification(t, control)
	if barrier.GetSubscriptionId() == issue11 {
		t.Fatal("unsubscribed issue-11 notification arrived before the issue-12 barrier")
	}
	if barrier.GetSubscriptionId() != issue12 || barrier.GetNumber() != 12 {
		t.Fatalf(
			"notification after unsubscribe = %s#%d (%q), want issue-12 barrier %s",
			barrier.GetRepo(),
			barrier.GetNumber(),
			barrier.GetSubscriptionId(),
			issue12,
		)
	}
}

// TestForgeContainerSubscribeOverTheWire proves container subscriptions receive new issues.
func TestForgeContainerSubscribeOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	endpoint, secret := newForgeSubscribeNotifyEndpoint(t, w)
	ctx, control := openForgeSubscribeControl(t, w)
	github := newFakeGitHubForge(secret, forgeE2ERepo)

	response, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
		CallId: "subscribe-repo-container",
		Call: &compassv1internal.ForgeCallRequest_Subscribe{Subscribe: &compassv1internal.SubscribeForgeRequest{
			Repo:   forgeE2ERepo,
			Kind:   mxIssue,
			Number: 0,
			Scope:  compassv1internal.ForgeSubscriptionScope_FORGE_SUBSCRIPTION_SCOPE_CONTAINER,
		}},
	}))
	if err != nil {
		t.Fatalf("Forge container subscribe: %v", err)
	}
	subscription := response.Msg.GetSubscribed()
	if subscription == nil || subscription.GetSubscriptionId() == "" {
		t.Fatalf("Forge container subscribe result = %v, want subscription id", response.Msg.GetResult())
	}

	postForgeSubscribeGitHub(t, w.ctx, endpoint, github.openIssue(
		t,
		42,
		"https://github.com/owner/repo/issues/42",
	))
	notification := receiveForgeSubscribeNotification(t, control)
	if notification.GetSubscriptionId() != subscription.GetSubscriptionId() {
		t.Errorf("subscription id = %q, want %q", notification.GetSubscriptionId(), subscription.GetSubscriptionId())
	}
	if notification.GetChange() != mxOpened {
		t.Errorf("change = %v, want OPENED", notification.GetChange())
	}
	if notification.GetNumber() != 42 {
		t.Errorf("number = %d, want 42", notification.GetNumber())
	}
}
