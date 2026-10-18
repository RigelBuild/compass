package forge

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitMirrorApplyBundleRejectsMissingCommits(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	base := mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorCommit(t, fixture.work, "change.txt", "change\n")
	bundle := mirrorBundle(t, fixture.work, "refs/heads/agent", "^"+base)

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	err := m.ApplyBundle(t.Context(), "owner/repo", bundle)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "prerequisite") {
		t.Fatalf("ApplyBundle with missing prerequisite = %v, want prerequisite error", err)
	}
}

func TestGitMirrorRepositoryPathCanonicalizesCase(t *testing.T) {
	fixture := newMirrorFixture(t)
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	path, _, normalized, err := m.prepare(t.Context(), "Owner/Repo")
	if err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	wantPath := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
	if normalized != "owner/repo" || path != wantPath {
		t.Fatalf("prepared repository = (%q, %q), want (%q, %q)", normalized, path, "owner/repo", wantPath)
	}
}

func TestGitMirrorPushRejectsStaleLease(t *testing.T) {
	fixture := newMirrorFixture(t)
	oldHead := mirrorCommit(t, fixture.work, "main.txt", "old\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	newHead := mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	bundle := mirrorBundle(t, fixture.work, "refs/heads/agent")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	if err := m.ApplyBundle(t.Context(), "owner/repo", bundle); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	competitor := filepath.Join(fixture.root, "competitor")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, competitor)
	mirrorGit(t, competitor, "config", "user.name", "Owner")
	mirrorGit(t, competitor, "config", "user.email", "owner@example.test")
	competitorHead := mirrorCommit(t, competitor, "main.txt", "competitor\n")
	mirrorGit(t, competitor, "push", "origin", "main")

	err := m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/main": newHead})
	if err == nil {
		t.Fatal("Push with a stale lease succeeded, want rejection")
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != competitorHead {
		t.Fatalf("remote main = %s, want unchanged newer head %s (old fetched head %s)", got, competitorHead, oldHead)
	}
}
func TestGitMirrorPushOnlyPushesRequestedRefs(t *testing.T) {
	fixture := newMirrorFixture(t)
	mainHead := mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	agentHead := mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	if err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if err := m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/agent": agentHead}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/agent"); got != agentHead {
		t.Fatalf("remote agent = %s, want requested head %s", got, agentHead)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != mainHead {
		t.Fatalf("remote main = %s, want unchanged head %s", got, mainHead)
	}
}

func TestGitMirrorRebaseConflictDoesNotPush(t *testing.T) {
	fixture := newMirrorFixture(t)
	base := mirrorCommit(t, fixture.work, "shared.txt", "base\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")

	feature := filepath.Join(fixture.root, "feature")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, feature)
	mirrorGit(t, feature, "switch", "-c", "agent")
	featureHead := mirrorCommit(t, feature, "shared.txt", "agent change\n")
	mirrorGit(t, feature, "push", "origin", "agent")

	mainline := filepath.Join(fixture.root, "mainline")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, mainline)
	mirrorGit(t, mainline, "config", "user.name", "Owner")
	mirrorGit(t, mainline, "config", "user.email", "owner@example.test")
	mainHead := mirrorCommit(t, mainline, "shared.txt", "owner change\n")
	mirrorGit(t, mainline, "push", "origin", "main")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	if err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	gotHead, conflict, err := m.Rebase(t.Context(), "owner/repo", "refs/heads/agent", mainHead)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if !conflict {
		t.Fatalf("Rebase(%s onto %s) conflict = false, want true (base %s)", featureHead, mainHead, base)
	}
	if gotHead != "" {
		t.Fatalf("Rebase conflict head = %q, want empty", gotHead)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/agent"); got != featureHead {
		t.Fatalf("remote agent = %s, want unchanged %s", got, featureHead)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != mainHead {
		t.Fatalf("remote main = %s, want unchanged %s", got, mainHead)
	}
}

func TestGitMirrorRebaseUpdatesRef(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")

	feature := filepath.Join(fixture.root, "feature")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, feature)
	mirrorGit(t, feature, "switch", "-c", "agent")
	oldHead := mirrorCommit(t, feature, "agent.txt", "agent\n")
	mirrorGit(t, feature, "push", "origin", "agent")

	mainline := filepath.Join(fixture.root, "mainline")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, mainline)
	mirrorGit(t, mainline, "config", "user.name", "Owner")
	mirrorGit(t, mainline, "config", "user.email", "owner@example.test")
	onto := mirrorCommit(t, mainline, "main.txt", "main\n")
	mirrorGit(t, mainline, "push", "origin", "main")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	if err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	newHead, conflict, err := m.Rebase(t.Context(), "owner/repo", "refs/heads/agent", onto)
	if err != nil || conflict {
		t.Fatalf("Rebase = (%q, %t, %v), want successful rebase", newHead, conflict, err)
	}
	if newHead == oldHead {
		t.Fatalf("rebased head = %s, want a new commit", newHead)
	}
	if got := mirrorRef(t, filepath.Join(fixture.root, "mirrors", "owner", "repo.git"), "refs/heads/agent"); got != newHead {
		t.Fatalf("mirror agent = %s, want updated head %s", got, newHead)
	}
}

func TestGitMirrorReplayFailureIsNotConflict(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	_, conflict, err := m.Rebase(t.Context(), "owner/repo", "refs/heads/agent", "refs/heads/unknown")
	if err == nil || conflict {
		t.Fatalf("Rebase with unknown target = (%t, %v), want non-conflict error", conflict, err)
	}
}

func TestGitMirrorReplayCommandFailureIsNotConflict(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	path, _, _, err := m.prepare(t.Context(), "owner/repo")
	if err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	oldHead := mirrorRef(t, path, "refs/heads/agent")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = replay ]; then\n    echo 'injected replay failure' >&2\n    exit 2\n  fi\ndone\nexec \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write Git wrapper: %v", err)
	}
	t.Setenv("MIRROR_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, conflict, err := m.Rebase(t.Context(), "owner/repo", "refs/heads/agent", "refs/remotes/origin/main")
	if err == nil || conflict || !strings.Contains(err.Error(), "injected replay failure") {
		t.Fatalf("Rebase with replay command failure = (%t, %v), want non-conflict command error", conflict, err)
	}
	if got := mirrorRef(t, path, "refs/heads/agent"); got != oldHead {
		t.Fatalf("agent head after replay command failure = %s, want unchanged %s", got, oldHead)
	}
}

func TestGitMirrorRejectedBundleDoesNotLeaveObjects(t *testing.T) {
	for _, limit := range []string{"per-repository", "total"} {
		t.Run(limit, func(t *testing.T) {
			fixture := newMirrorFixture(t)
			mirrorCommit(t, fixture.work, "main.txt", "main\n")
			mirrorGit(t, fixture.work, "push", "origin", "main")
			mirrorGit(t, fixture.work, "switch", "-c", "agent")
			mirrorCommit(t, fixture.work, "agent.txt", strings.Repeat("change", 200))
			bundle := mirrorBundle(t, fixture.work, "refs/heads/agent")

			m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
			if err := m.ApplyBundle(t.Context(), "owner/repo", nil); err != nil {
				t.Fatalf("initial ApplyBundle: %v", err)
			}
			path := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
			mirrorGit(t, fixture.root, "--git-dir", path, "gc", "--prune=now")
			before, err := directoryBytes(path)
			if err != nil {
				t.Fatalf("measure initial mirror: %v", err)
			}
			if limit == "per-repository" {
				m.perRepoCapBytes = before + 1
			} else {
				m.totalCapBytes = before + 1
			}
			err = m.ApplyBundle(t.Context(), "owner/repo", bundle)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), limit) {
				t.Fatalf("ApplyBundle over %s cap = %v, want refusal", limit, err)
			}
			afterRepo, err := directoryBytes(path)
			if err != nil {
				t.Fatalf("measure mirror after refusal: %v", err)
			}
			afterTotal, err := directoryBytes(m.dir)
			if err != nil {
				t.Fatalf("measure total mirrors after refusal: %v", err)
			}
			if afterRepo > m.perRepoCapBytes || afterTotal > m.totalCapBytes {
				t.Fatalf("mirror sizes after refusal = repo %d/%d total %d/%d", afterRepo, m.perRepoCapBytes, afterTotal, m.totalCapBytes)
			}
		})
	}
}

func TestGitMirrorFetchRefusalRollsBackRefsAndObjects(t *testing.T) {
	for _, limit := range []string{"per-repository", "total"} {
		t.Run(limit, func(t *testing.T) {
			fixture := newMirrorFixture(t)
			initialHead := mirrorCommit(t, fixture.work, "main.txt", "initial\n")
			mirrorGit(t, fixture.work, "push", "origin", "main")
			m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
			if err := m.ApplyBundle(t.Context(), "owner/repo", nil); err != nil {
				t.Fatalf("initial ApplyBundle: %v", err)
			}
			path := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
			mirrorGit(t, fixture.root, "--git-dir", path, "gc", "--prune=now")
			before, err := directoryBytes(path)
			if err != nil {
				t.Fatalf("measure initial mirror: %v", err)
			}
			if limit == "per-repository" {
				m.perRepoCapBytes = before + 1
			} else {
				m.totalCapBytes = before + 1
			}
			remoteHead := mirrorCommit(t, fixture.work, "large.txt", strings.Repeat("remote change ", 1000))
			mirrorGit(t, fixture.work, "push", "origin", "main")

			err = m.ApplyBundle(t.Context(), "owner/repo", nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), limit) {
				t.Fatalf("ApplyBundle over %s cap = %v, want refusal", limit, err)
			}
			if got := mirrorRef(t, path, "refs/remotes/origin/main"); got != initialHead {
				t.Fatalf("fetched main = %s, want restored head %s (refused head %s)", got, initialHead, remoteHead)
			}
			afterRepo, err := directoryBytes(path)
			if err != nil {
				t.Fatalf("measure mirror after fetch refusal: %v", err)
			}
			afterTotal, err := directoryBytes(m.dir)
			if err != nil {
				t.Fatalf("measure total mirrors after fetch refusal: %v", err)
			}
			if afterRepo > before || afterTotal > before {
				t.Fatalf("mirror sizes after fetch refusal = repo %d/%d total %d/%d", afterRepo, before, afterTotal, before)
			}
		})
	}
}

func TestGitMirrorPushErrorHidesToken(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	token := "mirror-secret-redaction-test"
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	m := NewGitMirrorWithHost(filepath.Join(fixture.root, "mirrors"), "https://127.0.0.1:1", 1<<30, 1<<30,
		func(context.Context, string) (string, error) { return token, nil }).(*gitMirror)
	path, _, _, err := m.prepare(t.Context(), "owner/repo")
	if err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	if _, err := m.run(t.Context(), path, "", "fetch", fixture.work, "refs/heads/main:refs/heads/main"); err != nil {
		t.Fatalf("seed mirror commit: %v", err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = push ]; then\n    printf 'injected failed push: %s %s %s\\n' \"$MIRROR_TEST_TOKEN\" \"$MIRROR_TEST_BASIC\" \"$GIT_CONFIG_VALUE_0\" >&2\n    exit 1\n  fi\ndone\nexec \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write Git wrapper: %v", err)
	}
	t.Setenv("MIRROR_TEST_REAL_GIT", realGit)
	t.Setenv("MIRROR_TEST_TOKEN", token)
	t.Setenv("MIRROR_TEST_BASIC", encoded)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/main": strings.TrimSpace(mirrorGit(t, fixture.work, "rev-parse", "HEAD"))})
	if err == nil || !strings.Contains(err.Error(), "injected failed push") {
		t.Fatalf("push with injected failure = %v, want the injected error", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), encoded) {
		t.Fatalf("push error exposed the App token: %v", err)
	}
}

func TestGitMirrorApplyBundleEnforcesCaps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		perRepoCap  int64
		totalCap    int64
		wantMessage string
	}{
		{name: "per repository", perRepoCap: 1, totalCap: 1 << 30, wantMessage: "per-repository"},
		{name: "total", perRepoCap: 1 << 30, totalCap: 1, wantMessage: "total"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMirrorFixture(t)
			mirrorCommit(t, fixture.work, "main.txt", "main\n")
			mirrorGit(t, fixture.work, "push", "origin", "main")
			mirrorGit(t, fixture.work, "switch", "-c", "agent")
			mirrorCommit(t, fixture.work, "agent.txt", "change\n")
			bundle := mirrorBundle(t, fixture.work, "refs/heads/agent")

			m := newTestGitMirror(t, fixture, tc.perRepoCap, tc.totalCap)
			err := m.ApplyBundle(t.Context(), "owner/repo", bundle)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantMessage) {
				t.Fatalf("ApplyBundle over %s cap = %v, want refusal naming %q", tc.name, err, tc.wantMessage)
			}
		})
	}
}

type mirrorFixture struct {
	root   string
	remote string
	work   string
	host   string
}

func newMirrorFixture(t *testing.T) mirrorFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "forge", "owner", "repo.git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}
	mirrorGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(root, "work")
	mirrorGit(t, root, "init", "--initial-branch=main", work)
	mirrorGit(t, work, "config", "user.name", "Agent")
	mirrorGit(t, work, "config", "user.email", "agent@example.test")
	mirrorGit(t, work, "remote", "add", "origin", remote)
	return mirrorFixture{root: root, remote: remote, work: work, host: "file://" + filepath.Join(root, "forge")}
}

func newTestGitMirror(t *testing.T, fixture mirrorFixture, perRepoCap, totalCap int64) Mirror {
	t.Helper()
	return NewGitMirrorWithHost(filepath.Join(fixture.root, "mirrors"), fixture.host, perRepoCap, totalCap,
		func(context.Context, string) (string, error) { return "", nil })
}
func mirrorCommit(t *testing.T, repo, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrorGit(t, repo, "add", name)
	mirrorGit(t, repo, "commit", "-m", "update "+name)
	return strings.TrimSpace(mirrorGit(t, repo, "rev-parse", "HEAD"))
}

func mirrorBundle(t *testing.T, repo string, refs ...string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "change.bundle")
	args := append([]string{"bundle", "create", path}, refs...)
	mirrorGit(t, repo, args...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mirrorRef(t *testing.T, repo, ref string) string {
	t.Helper()
	return strings.TrimSpace(mirrorGit(t, repo, "--git-dir", repo, "rev-parse", "--verify", ref))
}

func mirrorGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
