//go:build podman

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

func TestForgeThroughAgentLoop(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	ctx := context.Background()
	const handle = "forge-leg-agent"
	const repo = "owner/repo"
	args := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal tool arguments: %v", err)
		}
		return string(raw)
	}
	f := NewFixture(ctx, t, WithForgeStub(), WithCannedScript(
		CannedToolCall("forge_create_issue", args(map[string]any{"repo": repo, "title": "forge leg issue", "body": "authored by the forge leg", "labels": []string{"e2e"}})),
		CannedText("created"),
		CannedToolCall("forge_get_issue", args(map[string]any{"repo": repo, "issue_number": forgeStubIssueNumber})),
		CannedText("read"),
		CannedToolCall("forge_comment_on_issue", args(map[string]any{"repo": repo, "issue_number": forgeStubIssueNumber, "body": "forge leg comment"})),
		CannedText("commented"),
		CannedToolCall("forge_transition_issue_state", args(map[string]any{"repo": repo, "issue_number": forgeStubIssueNumber, "state": "closed", "close_reason": "completed"})),
		CannedText("closed"),
		CannedToolCall("forge_get_issue", args(map[string]any{"repo": repo, "issue_number": forgeStubIssueNumber})),
		CannedText("verified"),
		CannedToolCall("forge_create_pull_request", args(map[string]any{"repo": repo, "title": "forge leg pull request", "body": "authored by the forge leg", "head_ref": "forge-leg", "base_ref": "main", "draft": true})),
		CannedText("opened"),
	))
	agentID, err := f.CreateAgent(ctx, handle, "Forge Leg Agent")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container, err := f.Provision(ctx, agentID, "forge-leg-provision")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := f.RemoveWorkspace(ctx, container, "forge-leg-teardown"); err != nil {
			t.Logf("RemoveWorkspace: %v", err)
		}
	})
	sessionID, err := f.StartSession(ctx, container)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail: %v", err)
	}
	defer tail.Close()
	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	agent, err := adminAgentByHandle(ctx, st, handle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	for i, prompt := range []string{"create an issue", "read the issue", "comment on the issue", "close the issue", "verify the issue", "create a pull request"} {
		if _, err := f.PostMessage(ctx, string(agent.Agent.HomeChannelID), "general", prompt); err != nil {
			t.Fatalf("PostMessage trigger %d: %v", i+1, err)
		}
		if err := f.AwaitTurnSettled(ctx, tail); err != nil {
			t.Fatalf("AwaitTurnSettled turn %d: %v", i+1, err)
		}
	}

	requests := f.ForgeStub().Requests()
	if len(requests) != 7 {
		t.Fatalf("forge requests = %d, want 7 (one mint plus six API calls)", len(requests))
	}
	if requests[0].Path != "/api/v3/app/installations/1/access_tokens" || requests[0].Method != http.MethodPost {
		t.Fatalf("mint request = %#v", requests[0])
	}
	wantPaths := []string{
		"/api/v3/repos/owner/repo/issues",
		"/api/v3/repos/owner/repo/issues/4242",
		"/api/v3/repos/owner/repo/issues/4242/comments",
		"/api/v3/repos/owner/repo/issues/4242",
		"/api/v3/repos/owner/repo/issues/4242",
		"/api/v3/repos/owner/repo/pulls",
	}
	for i, want := range wantPaths {
		request := requests[i+1]
		if request.Path != want {
			t.Fatalf("forge request %d path = %q, want %q", i+1, request.Path, want)
		}
		if request.Authorization != "Bearer forge-stub-installation-1" {
			t.Fatalf("request %d authorization = %q", i+1, request.Authorization)
		}
	}

	var createBody map[string]any
	if err := json.Unmarshal(requests[1].Body, &createBody); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	createTitle, titleOK := createBody["title"].(string)
	createText, bodyOK := createBody["body"].(string)
	if !titleOK || createTitle != "forge leg issue" || !bodyOK || !strings.Contains(createText, "authored by the forge leg") || !strings.Contains(createText, "compass:owner") || !strings.Contains(createText, "agent="+handle) {
		t.Fatalf("create body = %#v", createBody)
	}
	if !reflect.DeepEqual(createBody["labels"], []any{"e2e"}) {
		t.Fatalf("create body labels = %#v, want [e2e]", createBody["labels"])
	}

	var commentBody map[string]any
	if err := json.Unmarshal(requests[3].Body, &commentBody); err != nil {
		t.Fatalf("decode comment body: %v", err)
	}
	if commentBody["body"] != "forge leg comment" {
		t.Fatalf("comment body = %#v", commentBody)
	}

	var pullRequestBody map[string]any
	if err := json.Unmarshal(requests[6].Body, &pullRequestBody); err != nil {
		t.Fatalf("decode pull request body: %v", err)
	}
	pullTitle, pullTitleOK := pullRequestBody["title"].(string)
	pullText, pullBodyOK := pullRequestBody["body"].(string)
	if !pullTitleOK || pullTitle != "forge leg pull request" || !pullBodyOK || !strings.Contains(pullText, "authored by the forge leg") || !strings.Contains(pullText, "compass:owner") || !strings.Contains(pullText, "agent="+handle) || pullRequestBody["head"] != "forge-leg" || pullRequestBody["base"] != "main" || pullRequestBody["draft"] != true {
		t.Fatalf("pull request body = %#v", pullRequestBody)
	}
	if _, err := f.awaitTranscriptPersisted(ctx, st, sessionID, "Created pull request #4243 in owner/repo: https://forge.stub/pulls/4243"); err != nil {
		t.Fatalf("awaitTranscriptPersisted (pull request response): %v", err)
	}
	authored, err := st.AuthoredArtifactByCoordinate(ctx, store.ForgeProviderGitHub, f.ForgeStub().Host(), repo, store.ForgeArtifactKindIssue, forgeStubIssueNumber)
	if err != nil {
		t.Fatalf("AuthoredArtifactByCoordinate: %v", err)
	}
	if authored.AgentAccountID != store.AccountID(agentID) || authored.Number != forgeStubIssueNumber {
		t.Fatalf("authored artifact = %#v", authored)
	}
	pullRequest, err := st.AuthoredArtifactByCoordinate(ctx, store.ForgeProviderGitHub, f.ForgeStub().Host(), repo, store.ForgeArtifactKindPullRequest, 4243)
	if err != nil {
		t.Fatalf("AuthoredArtifactByCoordinate pull request: %v", err)
	}
	if pullRequest.AgentAccountID != store.AccountID(agentID) || pullRequest.Number != 4243 {
		t.Fatalf("authored pull request = %#v", pullRequest)
	}
}
