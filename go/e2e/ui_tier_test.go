//go:build podman && uitier

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

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

	cmd := exec.CommandContext(ctx, "bunx", "playwright", "test", "--config", "playwright.live.config.ts")
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
