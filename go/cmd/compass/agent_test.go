//go:build unix

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// The agent subcommand drives GetAgentStatus against the shared fakeCompass /
// startFakeServer harness in cli_test.go (its statuses/gotStatus fields record
// the request and canned response), so the wiring is tested without a live
// Server or Postgres.

// TestRunAgentStatusAll asserts an empty --session sends an empty session id
// (every live session) and renders each returned session's id and short state
// label, stamping the bearer token.
func TestRunAgentStatusAll(t *testing.T) {
	fake := &fakeCompass{statuses: []*compassv1.AgentSessionStatus{
		{SessionId: "s1", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
		{SessionId: "s2", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED},
	}}
	client := startFakeServer(t, fake)

	var out strings.Builder
	if err := runAgentStatus(context.Background(), client, "", &out); err != nil {
		t.Fatalf("runAgentStatus: %v", err)
	}
	if fake.gotStatus.GetSessionId() != "" {
		t.Errorf("GetAgentStatus session_id = %q, want empty (all sessions)", fake.gotStatus.GetSessionId())
	}
	got := out.String()
	for _, want := range []string{"s1", "working", "s2", "disconnected", "SESSION", "STATE"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output %q missing %q", got, want)
		}
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
}

// TestRunAgentStatusByID asserts a set --session maps straight onto the request
// session_id (the by-id filter) and renders that one session.
func TestRunAgentStatusByID(t *testing.T) {
	fake := &fakeCompass{statuses: []*compassv1.AgentSessionStatus{
		{SessionId: "s1", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED},
	}}
	client := startFakeServer(t, fake)

	var out strings.Builder
	if err := runAgentStatus(context.Background(), client, "s1", &out); err != nil {
		t.Fatalf("runAgentStatus: %v", err)
	}
	if fake.gotStatus.GetSessionId() != "s1" {
		t.Errorf("GetAgentStatus session_id = %q, want s1", fake.gotStatus.GetSessionId())
	}
	if got := out.String(); !strings.Contains(got, "s1") || !strings.Contains(got, "stopped") {
		t.Errorf("status output %q missing s1/stopped", got)
	}
}

// TestRunAgentStatusEmpty asserts an empty result renders a clear message, not
// an error — no live sessions (or an unknown id) is a valid answer.
func TestRunAgentStatusEmpty(t *testing.T) {
	fake := &fakeCompass{}
	client := startFakeServer(t, fake)

	var out strings.Builder
	if err := runAgentStatus(context.Background(), client, "", &out); err != nil {
		t.Fatalf("runAgentStatus: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "no live agent sessions") {
		t.Errorf("empty output %q, want the no-sessions message", got)
	}
}

// TestAgentStatusFlagParsing asserts the status verb parses --session off argv
// into the request (flag wiring, not just the run function).
func TestAgentStatusFlagParsing(t *testing.T) {
	fake := &fakeCompass{}
	client := startFakeServer(t, fake)

	cmd := newAgentStatusCmd()
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runAgentStatus(cmd.Context(), client, cmd.Flag("session").Value.String(), cmd.OutOrStdout())
	}
	cmd.SetArgs([]string{"--session", "abc"})
	cmd.SetOut(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.gotStatus.GetSessionId() != "abc" {
		t.Errorf("parsed session_id = %q, want abc", fake.gotStatus.GetSessionId())
	}
}

// TestStateLabel asserts the enum-name → short-token rendering, including the
// unspecified fallback for an out-of-range value.
func TestStateLabel(t *testing.T) {
	cases := map[compassv1.AgentSessionState]string{
		compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING:      "working",
		compassv1.AgentSessionState_AGENT_SESSION_STATE_READY:        "ready",
		compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED: "disconnected",
		compassv1.AgentSessionState(9999):                            "unspecified",
	}
	for state, want := range cases {
		if got := stateLabel(state); got != want {
			t.Errorf("stateLabel(%v) = %q, want %q", state, got, want)
		}
	}
}

// TestRenderAgentStatusesTierEgress asserts the tier and egress columns render
// the short operator-facing token a user reads. The zero-value case is
// load-bearing: an unset posture must read as unknown, never "armed", because a
// client that renders an unknown posture as armed would show an uncontained
// session as contained.
func TestRenderAgentStatusesTierEgress(t *testing.T) {
	cases := []struct {
		name     string
		status   *compassv1.AgentSessionStatus
		wantTier string
		wantEg   string
	}{
		{
			name: "host unenforced",
			status: &compassv1.AgentSessionStatus{
				SessionId:     "s-host",
				State:         compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
				RuntimeTier:   compassv1.RuntimeTier_RUNTIME_TIER_HOST,
				EgressPosture: compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED,
			},
			wantTier: "host",
			wantEg:   "unenforced",
		},
		{
			name: "podman armed",
			status: &compassv1.AgentSessionStatus{
				SessionId:     "s-podman",
				State:         compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
				RuntimeTier:   compassv1.RuntimeTier_RUNTIME_TIER_PODMAN,
				EgressPosture: compassv1.EgressPosture_EGRESS_POSTURE_ARMED,
			},
			wantTier: "podman",
			wantEg:   "armed",
		},
		{
			// The rendered tier must be a token --backend accepts; a derived
			// label would print "apple_container", which the flag rejects.
			name: "apple-container renders the hyphenated flag token",
			status: &compassv1.AgentSessionStatus{
				SessionId:     "s-apple",
				State:         compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
				RuntimeTier:   compassv1.RuntimeTier_RUNTIME_TIER_APPLE_CONTAINER,
				EgressPosture: compassv1.EgressPosture_EGRESS_POSTURE_ARMED,
			},
			wantTier: "apple-container",
			wantEg:   "armed",
		},
		{
			name: "zero value renders unknown",
			status: &compassv1.AgentSessionStatus{
				SessionId: "s-zero",
				State:     compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
			},
			wantTier: unspecifiedLabel,
			wantEg:   unspecifiedLabel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := renderAgentStatuses(&out, []*compassv1.AgentSessionStatus{tc.status}); err != nil {
				t.Fatalf("renderAgentStatuses: %v", err)
			}
			got := out.String()
			for _, want := range []string{"TIER", "EGRESS", tc.status.GetSessionId(), tc.wantTier, tc.wantEg} {
				if !strings.Contains(got, want) {
					t.Errorf("rendered output %q missing %q", got, want)
				}
			}
			if tc.name == "zero value renders unknown" {
				for _, forbidden := range []string{"armed", "contained"} {
					if strings.Contains(got, forbidden) {
						t.Errorf("zero-value output %q must not contain %q", got, forbidden)
					}
				}
			}
		})
	}
}
