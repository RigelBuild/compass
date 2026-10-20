//go:build podman && uitier

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	uiTierAgentHandle = "ui-tier-agent"
	uiTierTopicName   = "ui-tier-agent-reply"
	uiTierMarker      = "e2e-route-ui-tier-reply"
	uiTierReply       = "ui tier: the agent reply reached the live UI"
)

// TestUITierPostAndReply drives the real Vite UI in headless Chromium against
// this fixture's door: the browser starts a topic, and the canned turn replies
// into that same topic through comms_post_message, which the UI must render.
func TestUITierPostAndReply(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into the fixture and the browser process

	// The door allows exactly one CORS origin, so Vite must serve on the port named here.
	devPort := freePort(t, ctx)
	origin := "http://127.0.0.1:" + strconv.Itoa(devPort)
	f := NewFixture(ctx, t,
		WithCORSOrigin(origin),
		// Turns the canned backend on; the marker route below serves the real turn.
		WithCannedScript(CannedText("ui tier unrouted turn")),
		WithCannedMarkerScript(uiTierMarker,
			CannedToolCall("comms_post_message", fmt.Sprintf(
				`{"text":%q,"topic":%q,"channel":%q,"create_topic":false}`,
				uiTierReply, uiTierTopicName, uiTierAgentHandle)),
			CannedText("reply posted"),
		),
	)

	accountID, err := f.CreateAgent(ctx, uiTierAgentHandle, "UI Tier Agent")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	containerName, err := f.Provision(ctx, accountID, "ui-tier-agent-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Rootless conmon outlives stack Down, so reap the container by name.
	t.Cleanup(func() {
		_ = f.RemoveWorkspace(ctx, containerName, "ui-tier-agent-teardown")
	})
	if _, err := f.StartSession(ctx, containerName); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	agent, err := adminAgentByHandle(ctx, st, uiTierAgentHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}

	cmd := exec.CommandContext(ctx, "bunx", "playwright", "test", "--config", "playwright.live.config.ts", "post-and-reply.spec.ts")
	cmd.Dir = filepath.Join("..", "..", "apps", "ui")
	cmd.Env = append(os.Environ(),
		"PLAYWRIGHT_DEV_PORT="+strconv.Itoa(devPort),
		"VITE_COMPASS_BASE_URL="+f.ServerURL(),
		"VITE_COMPASS_TOKEN="+f.AdminToken(),
		"UI_TIER_HOME_CHANNEL_ID="+string(agent.Agent.HomeChannelID),
		"UI_TIER_TOPIC_NAME="+uiTierTopicName,
		"UI_TIER_TRIGGER_TEXT="+uiTierMarker+": reply in this topic",
		"UI_TIER_EXPECTED_REPLY="+uiTierReply,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("playwright output:\n%s", out)
	if err != nil {
		t.Fatalf("live UI scenario: %v", err)
	}
}

const (
	uiTierLiveChannel     = "ui-tier-live-channel"
	uiTierLiveTopic       = "ui-tier-earlier-work"
	uiTierLiveEarlierText = "ui tier: posted before the UI saw this channel"
	uiTierReadyMarker     = "UI_TIER_PAGE_READY"
)

// TestUITierLiveChannel opens the UI first, then adds the caller to a channel
// whose message predates the join. The channel is absent from the page's
// snapshot, so it and its history reach the UI only through the live stream.
func TestUITierLiveChannel(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into the fixture and the browser process
	devPort := freePort(t, ctx)
	f := NewFixture(ctx, t, WithCORSOrigin("http://127.0.0.1:"+strconv.Itoa(devPort)))

	// The channel exists before the page loads, but the admin is not yet a member.
	if _, err := f.CreateUser(ctx, "ui-tier-live-owner", "UI Tier Live Owner"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, ownerComms, err := f.AsObserver(ctx, "ui-tier-live-owner")
	if err != nil {
		t.Fatalf("AsObserver: %v", err)
	}
	// Created by the owner, so the admin the UI signs in as is not a founding member.
	created, err := ownerComms.CreateChannel(ctx, &compassv1.CreateChannelRequest{
		Name: uiTierLiveChannel,
		Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	})
	if err != nil {
		t.Fatalf("CreateChannel (owner): %v", err)
	}
	channelID := created.GetChannel().GetId()
	if _, err := f.PostMessageAsObserver(ctx, ownerComms, channelID, uiTierLiveTopic, uiTierLiveEarlierText); err != nil {
		t.Fatalf("PostMessageAsObserver: %v", err)
	}

	// Covers Vite's 120s start limit plus the 90s test, so a child that never
	// prints the marker is killed instead of blocking the read below.
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "bunx", "playwright", "test", "--config", "playwright.live.config.ts", "live-channel.spec.ts")
	cmd.Dir = filepath.Join("..", "..", "apps", "ui")
	cmd.Env = append(os.Environ(),
		"PLAYWRIGHT_DEV_PORT="+strconv.Itoa(devPort),
		"VITE_COMPASS_BASE_URL="+f.ServerURL(),
		"VITE_COMPASS_TOKEN="+f.AdminToken(),
		"UI_TIER_LIVE_CHANNEL="+uiTierLiveChannel,
		"UI_TIER_LIVE_TOPIC="+uiTierLiveTopic,
		"UI_TIER_LIVE_EARLIER_TEXT="+uiTierLiveEarlierText,
		"UI_TIER_READY_MARKER="+uiTierReadyMarker,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start playwright: %v", err)
	}
	var out strings.Builder
	reader := bufio.NewReader(io.TeeReader(stdout, &out))
	ready := false
	for !ready {
		line, readErr := reader.ReadString('\n')
		ready = strings.Contains(line, uiTierReadyMarker)
		if readErr != nil {
			break
		}
	}
	if ready {
		// The admin cannot see the private channel yet, so its owner adds it.
		rctx, rcancel := context.WithTimeout(runCtx, rpcTimeout)
		_, err := ownerComms.UpdateChannelMembers(rctx, &compassv1.UpdateChannelMembersRequest{
			ChannelId:        channelID,
			AddMemberHandles: []string{bootstrapAdminHandle},
			SubscribeHandles: []string{bootstrapAdminHandle},
		})
		rcancel()
		if err != nil {
			t.Errorf("UpdateChannelMembers: %v", err)
		}
	}
	_, _ = io.Copy(io.Discard, reader)
	waitErr := cmd.Wait()
	t.Logf("playwright output:\n%s", out.String())
	if !ready {
		t.Fatalf("playwright never printed %s", uiTierReadyMarker)
	}
	if waitErr != nil {
		t.Fatalf("live UI scenario: %v", waitErr)
	}
}
