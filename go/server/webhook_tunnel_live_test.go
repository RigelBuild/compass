//go:build livegithub && unix

// Live webhook round-trip: the real testbed GitHub App and the Linear test-team
// webhook deliver to a public relay channel; the test replays each delivery's
// raw body and signature headers through the production handlers.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// liveTunnelSkipMessage is the STABLE skip string; the CI oracle guard greps it
// from this file, so a missing tunnel secret turns the required check red.
const liveTunnelSkipMessage = "live webhook tunnel: relay channel or testbed webhook secrets unset; skipping the round-trip leg"

const (
	envTunnelChannel  = "WEBHOOK_TUNNEL_CHANNEL"      // relay channel both testbed webhooks post to
	envGitHubWHSecret = "LIVEGITHUB_WEBHOOK_SECRET"   // testbed author App webhook secret
	envLinearWHSecret = "LINEAR_FORGE_WEBHOOK_SECRET" // TEST-team Linear webhook secret
)

// tunnelRelayURL is the public gosmee relay. It forwards the raw body byte-exact
// as base64 `bodyB`; smee.io re-serializes the JSON and breaks the HMAC.
const tunnelRelayURL = "https://hook.pipelinesascode.com"

// tunnelDeliveryWait bounds how long a leg waits for the forge to deliver.
const tunnelDeliveryWait = 2 * time.Minute

// linearTunnelWait stays inside the Linear handler's 60s timestamp window: a
// later delivery is acked and dropped, so waiting longer cannot succeed.
const linearTunnelWait = 45 * time.Second

func requireTunnelEnv(t *testing.T, names ...string) []string {
	t.Helper()
	vals := make([]string, len(names))
	for i, n := range names {
		if vals[i] = os.Getenv(n); vals[i] == "" {
			t.Skip(liveTunnelSkipMessage)
		}
	}
	return vals
}

// liveTunnel subscribes to the relay channel before the caller triggers an event.
func liveTunnel(t *testing.T, channel string, wait time.Duration) (*tunnelSub, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(cancel)
	sub, err := tunnelSubscribe(ctx, tunnelRelayURL, channel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.close)
	return sub, ctx
}

func liveRunTitle(kind string) string {
	return "compass-live-webhook-" + kind + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func TestLiveWebhookTunnelGitHub(t *testing.T) {
	env := requireTunnelEnv(t, envTunnelChannel, envGitHubWHSecret, "LIVEGITHUB_REPO",
		"LIVEGITHUB_AUTHOR_APP_ID", "LIVEGITHUB_AUTHOR_APP_INSTALLATION_ID", "LIVEGITHUB_AUTHOR_APP_KEY")
	channel, secret, repo := env[0], []byte(env[1]), env[2]
	appID, err := strconv.ParseInt(env[3], 10, 64)
	if err != nil {
		t.Fatalf("LIVEGITHUB_AUTHOR_APP_ID: %v", err)
	}
	installID, err := strconv.ParseInt(env[4], 10, 64)
	if err != nil {
		t.Fatalf("LIVEGITHUB_AUTHOR_APP_INSTALLATION_ID: %v", err)
	}
	key := []byte(env[5])
	ts, err := forge.NewAppTokenSource(forge.GitHubAppConfig{
		AppID: appID, InstallationID: installID, Host: "github.com",
		PrivateKey: func(context.Context) ([]byte, error) { return key, nil },
	})
	if err != nil {
		t.Fatalf("NewAppTokenSource: %v", err)
	}
	gh := forge.NewGitHub(forge.GitHubConfig{Host: "github.com", Token: ts})

	sub, ctx := liveTunnel(t, channel, tunnelDeliveryWait)
	title := liveRunTitle("gh")
	iss, err := gh.CreateIssue(ctx, repo, forge.CreateIssue{Title: title})
	if err != nil {
		t.Fatalf("create testbed issue: %v", err)
	}
	t.Cleanup(func() {
		if _, err := gh.TransitionIssueState(context.Background(), repo, iss.Number, forge.TransitionState{State: "closed"}); err != nil {
			t.Logf("teardown: close %s#%d: %v", repo, iss.Number, err)
		}
	})

	d, err := sub.next(ctx, func(d tunnelDelivery) bool {
		return d.header.Get(githubEventHeader) == "issues" && bytes.Contains(d.body, []byte(title))
	})
	if err != nil {
		t.Fatalf("GitHub delivery for %s#%d: %v", repo, iss.Number, err)
	}
	h, sink := newTestHandler(t, secret)
	if rec := replayTunnel(t, h, githubWebhookPath, d); rec.Code != http.StatusOK {
		t.Fatalf("GitHub handler code = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if sink.count() != 1 || sink.events[0].Number != iss.Number || sink.events[0].Repo != repo {
		t.Fatalf("sink = %+v, want one event for %s#%d", sink.events, repo, iss.Number)
	}
}

func TestLiveWebhookTunnelLinear(t *testing.T) {
	env := requireTunnelEnv(t, envTunnelChannel, envLinearWHSecret, "LINEAR_FORGE", "LINEAR_FORGE_TEAM")
	channel, secret, team := env[0], []byte(env[1]), env[3]
	tok := &staticLiveToken{token: env[2]}
	ln := forge.NewLinear(forge.LinearConfig{Token: tok})

	sub, ctx := liveTunnel(t, channel, linearTunnelWait)
	title := liveRunTitle("linear")
	iss, err := ln.CreateIssue(ctx, team, forge.CreateIssue{Title: title})
	if err != nil {
		t.Fatalf("create TEST issue: %v", err)
	}
	ident := fmt.Sprintf("%s-%d", team, iss.Number)
	t.Cleanup(func() { archiveLiveLinear(t, tok.token, ident) })

	d, err := sub.next(ctx, func(d tunnelDelivery) bool {
		return d.header.Get(linearSignatureHeader) != "" && bytes.Contains(d.body, []byte(title))
	})
	if err != nil {
		t.Fatalf("Linear delivery for %s: %v", ident, err)
	}
	data := &recordingSink{}
	_, h := NewLinearWebhookHandler(func(context.Context) ([]byte, error) { return secret, nil }, data, nil, nil)
	if rec := replayTunnel(t, h, linearWebhookPath, d); rec.Code != http.StatusOK {
		t.Fatalf("Linear handler code = %d, want 200", rec.Code)
	}
	// A wrong secret or a stale timestamp still acks 200 but never reaches the sink.
	if data.count() != 1 || data.events[0].Number != iss.Number || data.events[0].Repo != team {
		t.Fatalf("data sink = %+v, want one event for %s", data.events, ident)
	}
}

type staticLiveToken struct{ token string }

func (s *staticLiveToken) Token(context.Context) (string, error) { return s.token, nil }
func (s *staticLiveToken) Invalidate()                           {}

// archiveLiveLinear archives the leg's issue; issueArchive accepts an identifier.
func archiveLiveLinear(t *testing.T, token, ident string) {
	t.Helper()
	q, err := json.Marshal(map[string]any{
		"query":     "mutation($id: String!) { issueArchive(id: $id) { success } }",
		"variables": map[string]string{"id": ident},
	})
	if err != nil {
		t.Errorf("teardown: archive %s: %v", ident, err)
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(q))
	if err != nil {
		t.Errorf("teardown: archive %s: %v", ident, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("teardown: archive %s: %v", ident, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data struct {
			IssueArchive struct {
				Success bool `json:"success"`
			} `json:"issueArchive"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK ||
		len(out.Errors) > 0 || !out.Data.IssueArchive.Success {
		t.Errorf("teardown: archive %s: HTTP %d, errors %v, decode %v", ident, resp.StatusCode, out.Errors, err)
	}
}
