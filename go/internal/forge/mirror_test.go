package forge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitMirrorApplyBundleRejectsMissingCommits(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	base := mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorCommit(t, fixture.work, "change.txt", "change\n")
	bundle := mirrorBundle(t, fixture.work, "refs/heads/agent", "^"+base)

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	_, err := m.ApplyBundle(t.Context(), "owner/repo", bundle, []string{"refs/heads/agent"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "prerequisite") {
		t.Fatalf("ApplyBundle with missing prerequisite = %v, want prerequisite error", err)
	}
}

func TestGitMirrorApplyBundleDoesNotInstallUnrequestedRefs(t *testing.T) {
	fixture := newMirrorFixture(t)
	mainHead := mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("initial ApplyBundle: %v", err)
	}
	path := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
	mirrorGit(t, fixture.root, "--git-dir", path, "update-ref", "refs/heads/extra", mainHead)
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	agentHead := mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	mirrorGit(t, fixture.work, "switch", "-c", "extra", mainHead)
	mirrorCommit(t, fixture.work, "extra.txt", "unrequested\n")
	bundle := mirrorBundle(t, fixture.work, "refs/heads/agent", "refs/heads/extra")

	if _, err := m.ApplyBundle(t.Context(), "owner/repo", bundle, []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if got := mirrorRef(t, path, "refs/heads/agent"); got != agentHead {
		t.Fatalf("requested agent head = %s, want %s", got, agentHead)
	}
	if got := mirrorRef(t, path, "refs/heads/extra"); got != mainHead {
		t.Fatalf("unrequested extra head = %s, want unchanged %s", got, mainHead)
	}
	if got := mirrorGit(t, fixture.root, "--git-dir", path, "for-each-ref", "--format=%(refname)", "refs/compass-incoming"); strings.TrimSpace(got) != "" {
		t.Fatalf("temporary incoming refs remain: %s", got)
	}
}

func TestGitMirrorApplyBundleRemovesStaleIncomingRefs(t *testing.T) {
	fixture := newMirrorFixture(t)
	mainHead := mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("initial ApplyBundle: %v", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	staleRef := "refs/compass-incoming/stale"
	mirrorGit(t, fixture.root, "--git-dir", path, "update-ref", staleRef, mainHead)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("ApplyBundle with empty refs: %v", err)
	}
	if got := mirrorGit(t, fixture.root, "--git-dir", path, "for-each-ref", "--format=%(refname)", "refs/compass-incoming"); strings.TrimSpace(got) != "" {
		t.Fatalf("stale incoming refs remain after empty ApplyBundle: %s", got)
	}
}

func TestGitMirrorApplyBundleRemovesStaleIncomingRefsOnCapRefusal(t *testing.T) {
	fixture := newMirrorFixture(t)
	mainHead := mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("initial ApplyBundle: %v", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	staleRef := "refs/compass-incoming/stale"
	mirrorGit(t, fixture.root, "--git-dir", path, "update-ref", staleRef, mainHead)
	m.perRepoCapBytes = 1
	m.totalCapBytes = 1
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "per-repository") {
		t.Fatalf("ApplyBundle over cap = %v, want per-repository cap refusal", err)
	}
	if got := mirrorGit(t, fixture.root, "--git-dir", path, "for-each-ref", "--format=%(refname)", "refs/compass-incoming"); strings.TrimSpace(got) != "" {
		t.Fatalf("stale incoming refs remain after refused ApplyBundle: %s", got)
	}
}

func TestGitMirrorFirstApplyRefusalRemovesRepository(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	repoBytes, err := directoryBytes(path)
	if err != nil {
		t.Fatalf("measure seeded repository: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove seeded repository: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(m.dir, "owner", "other"), 0o700); err != nil {
		t.Fatalf("create sibling mirror data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "owner", "other", "accounted.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write sibling mirror data: %v", err)
	}
	m.perRepoCapBytes = repoBytes
	m.totalCapBytes = repoBytes
	_, err = m.ApplyBundle(t.Context(), "owner/repo", nil, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "total") {
		t.Fatalf("first ApplyBundle over aggregate cap = %v, want total cap refusal", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first refused mirror repository stat error = %v, want not exist", err)
	}
	got, err := directoryBytes(m.dir)
	if err != nil {
		t.Fatalf("measure mirrors after first refusal: %v", err)
	}
	if got > m.totalCapBytes {
		t.Fatalf("total mirror bytes after first refusal = %d, cap %d", got, m.totalCapBytes)
	}
}

func TestGitMirrorRepositoryPathCanonicalizesCase(t *testing.T) {
	fixture := newMirrorFixture(t)
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	path, _, normalized, _, err := m.prepare(t.Context(), "Owner/Repo")
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
	leases, err := m.ApplyBundle(t.Context(), "owner/repo", bundle, []string{"refs/heads/agent"})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	seeded, err := m.SeedRef(t.Context(), "owner/repo", "refs/heads/main")
	if err != nil || seeded != oldHead || leases["refs/heads/agent"] != "" {
		t.Fatalf("leases = %v seeded = (%s, %v), want main at %s and agent absent", leases, seeded, err, oldHead)
	}
	leases["refs/heads/main"] = seeded
	competitor := filepath.Join(fixture.root, "competitor")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, competitor)
	mirrorGit(t, competitor, "config", "user.name", "Owner")
	mirrorGit(t, competitor, "config", "user.email", "owner@example.test")
	competitorHead := mirrorCommit(t, competitor, "main.txt", "competitor\n")
	mirrorGit(t, competitor, "push", "origin", "main")

	// A later fetch sees the competitor; the lease from the first fetch must still hold.
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	err = m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/main": newHead}, leases)
	if err == nil {
		t.Fatal("Push with a stale lease succeeded, want rejection")
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != competitorHead {
		t.Fatalf("remote main = %s, want unchanged newer head %s (old fetched head %s)", got, competitorHead, oldHead)
	}
}

func TestGitMirrorPushIsAtomicOnOneStaleLease(t *testing.T) {
	fixture := newMirrorFixture(t)
	oldMain := mirrorCommit(t, fixture.work, "main.txt", "old\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	agentHead := mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	leases, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent"), []string{"refs/heads/agent"})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if leases["refs/heads/main"], err = m.SeedRef(t.Context(), "owner/repo", "refs/heads/main"); err != nil {
		t.Fatalf("SeedRef: %v", err)
	}
	competitor := filepath.Join(fixture.root, "competitor")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, competitor)
	mirrorGit(t, competitor, "config", "user.name", "Owner")
	mirrorGit(t, competitor, "config", "user.email", "owner@example.test")
	competitorHead := mirrorCommit(t, competitor, "main.txt", "competitor\n")
	mirrorGit(t, competitor, "push", "origin", "main")

	err = m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/agent": agentHead, "refs/heads/main": agentHead}, leases)
	if err == nil {
		t.Fatal("Push with one stale lease succeeded, want rejection")
	}
	if mirrorRefExists(t, fixture.remote, "refs/heads/agent") {
		t.Fatal("remote agent was created, want no ref changed by a rejected atomic push")
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != competitorHead {
		t.Fatalf("remote main = %s, want competitor %s (old %s)", got, competitorHead, oldMain)
	}
}

func TestGitMirrorDeleteTwiceIsIdempotent(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	mirrorGit(t, fixture.work, "push", "origin", "agent")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	del := map[string]string{"refs/heads/agent": ""}
	for i := range 2 {
		leases, err := m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/agent"})
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if err := m.Push(t.Context(), "owner/repo", del, leases); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	if mirrorRefExists(t, fixture.remote, "refs/heads/agent") {
		t.Fatal("remote agent still exists after delete")
	}
}

func TestGitMirrorPushOnlyPushesRequestedRefs(t *testing.T) {
	fixture := newMirrorFixture(t)
	mainHead := mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	agentHead := mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	leases, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent"), []string{"refs/heads/agent"})
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if err := m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/agent": agentHead}, leases); err != nil {
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
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	gotHead, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, mainHead)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if !conflict {
		t.Fatalf("Rebase(%s onto %s) conflict = false, want true (base %s)", featureHead, mainHead, base)
	}
	if len(gotHead) != 0 {
		t.Fatalf("Rebase conflict heads = %v, want none", gotHead)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/agent"); got != featureHead {
		t.Fatalf("remote agent = %s, want unchanged %s", got, featureHead)
	}
	if got := mirrorRef(t, fixture.remote, "refs/heads/main"); got != mainHead {
		t.Fatalf("remote main = %s, want unchanged %s", got, mainHead)
	}
}

func TestGitMirrorRebaseRefusesOverCapAndRestoresRef(t *testing.T) {
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

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	mirrorGit(t, fixture.root, "--git-dir", path, "gc", "--prune=now")
	before, err := directoryBytes(path)
	if err != nil {
		t.Fatalf("measure mirror before rebase: %v", err)
	}
	m.perRepoCapBytes = before + 1
	if _, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, onto); err == nil || conflict || !strings.Contains(strings.ToLower(err.Error()), "per-repository") {
		t.Fatalf("Rebase over cap = (conflict %t, err %v), want per-repository cap refusal", conflict, err)
	}
	if got := mirrorRef(t, path, "refs/heads/agent"); got != oldHead {
		t.Fatalf("agent head after refused rebase = %s, want restored %s", got, oldHead)
	}
	after, err := directoryBytes(path)
	if err != nil {
		t.Fatalf("measure mirror after rebase refusal: %v", err)
	}
	if after > m.perRepoCapBytes {
		t.Fatalf("mirror bytes after refused rebase = %d, cap %d", after, m.perRepoCapBytes)
	}
}

func TestGitMirrorRebaseRestoresRefWhenReadingHeadIsCancelled(t *testing.T) {
	fixture := newMirrorFixture(t)
	_ = mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	feature := filepath.Join(fixture.root, "feature")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, feature)
	mirrorGit(t, feature, "switch", "-c", "agent")
	oldHead := mirrorCommit(t, feature, "agent.txt", "agent\n")
	mainline := filepath.Join(fixture.root, "mainline")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, mainline)
	mirrorGit(t, mainline, "config", "user.name", "Owner")
	mirrorGit(t, mainline, "config", "user.email", "owner@example.test")
	onto := mirrorCommit(t, mainline, "main.txt", "main\n")
	mirrorGit(t, mainline, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	firstPath := filepath.Join(m.dir, "owner", "repo.git")
	releaseFile, replayEventPath, readEventPath := gateGitReplay(t)
	readReleasePath := filepath.Join(t.TempDir(), "read-release")
	if err := os.WriteFile(readReleasePath, nil, 0o600); err != nil {
		t.Fatalf("create read release gate: %v", err)
	}
	t.Setenv("MIRROR_TEST_REPLAY_READ_RELEASE", readReleasePath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rebaseResult := make(chan error, 1)
	go func() {
		_, _, err := m.Rebase(ctx, "owner/repo", []string{"refs/heads/agent"}, onto)
		rebaseResult <- err
	}()
	waitForMirrorTestFile(t, replayEventPath, rebaseResult)
	if err := os.Remove(releaseFile.Name()); err != nil {
		t.Fatalf("release replay: %v", err)
	}
	waitForMirrorTestFile(t, readEventPath, rebaseResult)
	cancel()
	if err := <-rebaseResult; err == nil || !strings.Contains(strings.ToLower(err.Error()), "read rebased head") {
		t.Fatalf("Rebase after cancelled head read = %v, want read error", err)
	}
	if err := os.Remove(readReleasePath); err != nil {
		t.Fatalf("release failed head read: %v", err)
	}
	if got := mirrorRef(t, firstPath, "refs/heads/agent"); got != oldHead {
		t.Fatalf("agent head after failed read = %s, want restored %s", got, oldHead)
	}
}

func TestGitMirrorRebaseSerializesConcurrentApply(t *testing.T) {
	m, firstPath, secondPath, oldHead, onto, base, otherBundle := newConcurrentMirrorFixture(t)
	releaseFile, replayEventPath, _ := gateGitReplay(t)
	rebaseResult := make(chan error, 1)
	go func() {
		heads, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, onto)
		newHead := heads["refs/heads/agent"]
		if err != nil {
			rebaseResult <- err
			return
		}
		if conflict || newHead == "" || newHead == oldHead {
			rebaseResult <- errors.New("rebase did not install a new head")
			return
		}
		rebaseResult <- nil
	}()
	waitForMirrorTestFile(t, replayEventPath, rebaseResult)
	if m.capMu.TryLock() {
		m.capMu.Unlock()
		if err := os.Remove(releaseFile.Name()); err != nil {
			t.Fatalf("release replay after failed lock check: %v", err)
		}
		t.Fatal("rebase did not hold the mirror cap lock during replay")
	}
	applyResult := make(chan error, 1)
	go func() {
		applyResult <- func() error {
			_, err := m.ApplyBundle(t.Context(), "owner/other", otherBundle, []string{"refs/heads/agent"})
			return err
		}()
	}()
	if err := os.Remove(releaseFile.Name()); err != nil {
		t.Fatalf("release replay: %v", err)
	}
	if err := <-rebaseResult; err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if err := <-applyResult; err == nil || !strings.Contains(strings.ToLower(err.Error()), "total") {
		t.Fatalf("concurrent ApplyBundle error = %v, want total cap refusal", err)
	}
	if got := mirrorRef(t, firstPath, "refs/heads/agent"); got == oldHead {
		t.Fatalf("rebased head = %s, want a new commit (base %s)", got, base)
	}
	if _, err := m.run(t.Context(), secondPath, "", "rev-parse", "--verify", "refs/heads/agent"); err == nil {
		t.Fatal("refused concurrent bundle installed its agent branch")
	}
	after, err := directoryBytes(m.dir)
	if err != nil {
		t.Fatalf("measure mirror after concurrent operations: %v", err)
	}
	if after > m.totalCapBytes {
		t.Fatalf("mirror bytes after concurrent operations = %d, total cap %d", after, m.totalCapBytes)
	}
}

func newConcurrentMirrorFixture(t *testing.T) (*gitMirror, string, string, string, string, string, []byte) {
	t.Helper()
	fixture := newMirrorFixture(t)
	base := mirrorCommit(t, fixture.work, "base.txt", "base\n")
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

	otherRemote := filepath.Join(fixture.root, "forge", "owner", "other.git")
	mirrorGit(t, fixture.root, "init", "--bare", "--initial-branch=main", otherRemote)
	otherWork := filepath.Join(fixture.root, "other-work")
	mirrorGit(t, fixture.root, "init", "--initial-branch=main", otherWork)
	mirrorGit(t, otherWork, "config", "user.name", "Agent")
	mirrorGit(t, otherWork, "config", "user.email", "agent@example.test")
	mirrorGit(t, otherWork, "remote", "add", "origin", otherRemote)
	mirrorCommit(t, otherWork, "base.txt", "other base\n")
	mirrorGit(t, otherWork, "push", "origin", "main")
	mirrorGit(t, otherWork, "switch", "-c", "agent")
	largeContent := make([]byte, 8<<10)
	if _, err := rand.Read(largeContent); err != nil {
		t.Fatalf("create large bundle content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherWork, "large.bin"), largeContent, 0o600); err != nil {
		t.Fatalf("write large bundle content: %v", err)
	}
	mirrorGit(t, otherWork, "add", "large.bin")
	mirrorGit(t, otherWork, "commit", "-m", "large bundle")
	otherBundle := mirrorBundle(t, otherWork, "refs/heads/agent")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("seed first repository: %v", err)
	}
	if _, err := m.ApplyBundle(t.Context(), "owner/other", nil, nil); err != nil {
		t.Fatalf("seed second repository: %v", err)
	}
	firstPath := filepath.Join(m.dir, "owner", "repo.git")
	secondPath := filepath.Join(m.dir, "owner", "other.git")
	for _, path := range []string{firstPath, secondPath} {
		mirrorGit(t, fixture.root, "--git-dir", path, "gc", "--prune=now")
	}
	baseline, err := directoryBytes(m.dir)
	if err != nil {
		t.Fatalf("measure mirror baseline: %v", err)
	}
	m.totalCapBytes = baseline + 4096
	m.perRepoCapBytes = m.totalCapBytes
	return m, firstPath, secondPath, oldHead, onto, base, otherBundle
}

func gateGitReplay(t *testing.T) (*os.File, string, string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	binDir := t.TempDir()
	eventFile := filepath.Join(t.TempDir(), "replay-event")
	readEventFile := filepath.Join(t.TempDir(), "read-event")
	releaseFile, err := os.CreateTemp(t.TempDir(), "replay-release-*")
	if err != nil {
		t.Fatalf("create replay release gate: %v", err)
	}
	if err := releaseFile.Close(); err != nil {
		t.Fatalf("close replay release gate: %v", err)
	}
	wrapper := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = replay ]; then\n    printf x > \"$MIRROR_TEST_REPLAY_EVENT\"\n    while [ -f \"$MIRROR_TEST_REPLAY_RELEASE\" ]; do :; done\n  fi\ndone\nprevious=\nfor arg do\n  if [ \"$previous\" = --verify ] && [ \"$arg\" = refs/heads/agent ] && [ -n \"$MIRROR_TEST_REPLAY_READ_RELEASE\" ] && [ -f \"$MIRROR_TEST_REPLAY_READ_RELEASE\" ]; then\n    printf x > \"$MIRROR_TEST_REPLAY_READ_EVENT\"\n    while [ -f \"$MIRROR_TEST_REPLAY_READ_RELEASE\" ]; do :; done\n  fi\n  previous=\"$arg\"\ndone\nexec \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write Git wrapper: %v", err)
	}
	t.Setenv("MIRROR_TEST_REAL_GIT", realGit)
	t.Setenv("MIRROR_TEST_REPLAY_EVENT", eventFile)
	t.Setenv("MIRROR_TEST_REPLAY_RELEASE", releaseFile.Name())
	t.Setenv("MIRROR_TEST_REPLAY_READ_EVENT", readEventFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := exec.LookPath("git"); err != nil || got != filepath.Join(binDir, "git") {
		t.Fatalf("Git wrapper lookup = (%q, %v), want %q", got, err, filepath.Join(binDir, "git"))
	}
	return releaseFile, eventFile, readEventFile
}

func waitForMirrorTestFile(t *testing.T, path string, result <-chan error) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect mirror test event: %v", err)
		}
		select {
		case err := <-result:
			t.Fatalf("mirror operation returned before event %s: %v", path, err)
		case <-deadline.C:
			t.Fatalf("timed out waiting for mirror test event %s", path)
		case <-tick.C:
		}
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
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	heads, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, onto)
	newHead := heads["refs/heads/agent"]
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

func TestGitMirrorRebaseMovesSeededForeignLineInOneReplay(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")

	feature := filepath.Join(fixture.root, "feature")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, feature)
	mirrorGit(t, feature, "switch", "-c", "foreign")
	foreignOld := mirrorCommit(t, feature, "foreign.txt", "foreign\n")
	mirrorGit(t, feature, "push", "origin", "foreign")
	mirrorGit(t, feature, "switch", "-c", "own1")
	own1Old := mirrorCommit(t, feature, "own1.txt", "own1\n")
	mirrorGit(t, feature, "switch", "-c", "own2")
	own2Old := mirrorCommit(t, feature, "own2.txt", "own2\n")

	mainline := filepath.Join(fixture.root, "mainline")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, mainline)
	mirrorGit(t, mainline, "config", "user.name", "Owner")
	mirrorGit(t, mainline, "config", "user.email", "owner@example.test")
	onto := mirrorCommit(t, mainline, "main.txt", "main\n")
	mirrorGit(t, mainline, "push", "origin", "main")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	own := []string{"refs/heads/own1", "refs/heads/own2"}
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, own...), own); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	seeded, err := m.SeedRef(t.Context(), "owner/repo", "refs/heads/foreign")
	if err != nil || seeded != foreignOld {
		t.Fatalf("SeedRef = (%q, %v), want fetched head %s", seeded, err, foreignOld)
	}
	line := []string{"refs/heads/foreign", "refs/heads/own1", "refs/heads/own2"}
	heads, conflict, err := m.Rebase(t.Context(), "owner/repo", line, onto)
	if err != nil || conflict {
		t.Fatalf("Rebase = (%v, %t, %v), want a moved line", heads, conflict, err)
	}
	mirror := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
	for _, ref := range line {
		if got := mirrorRef(t, mirror, ref); got != heads[ref] {
			t.Fatalf("mirror %s = %s, want rebased %s", ref, got, heads[ref])
		}
	}
	for ref, old := range map[string]string{line[0]: foreignOld, line[1]: own1Old, line[2]: own2Old} {
		if heads[ref] == old {
			t.Fatalf("%s did not move from %s", ref, old)
		}
	}
	// The line stays chained: each rebased ref's parent is the one below it.
	if got := mirrorRef(t, mirror, line[0]+"^"); got != onto {
		t.Fatalf("foreign parent = %s, want onto %s", got, onto)
	}
	if got := mirrorRef(t, mirror, line[1]+"^"); got != heads[line[0]] {
		t.Fatalf("own1 parent = %s, want rebased foreign %s", got, heads[line[0]])
	}
	if got := mirrorRef(t, mirror, line[2]+"^"); got != heads[line[1]] {
		t.Fatalf("own2 parent = %s, want rebased own1 %s", got, heads[line[1]])
	}
	again, conflict, err := m.Rebase(t.Context(), "owner/repo", line, onto)
	if err != nil || conflict || len(again) != 0 {
		t.Fatalf("second Rebase = (%v, %t, %v), want no move", again, conflict, err)
	}
}

// One stale expected-old value fails the whole multi-ref transaction, so no ref in the line moves.
func TestGitMirrorRebaseExpectedOldMismatchMovesNoRef(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "base.txt", "base\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	feature := filepath.Join(fixture.root, "feature")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, feature)
	mirrorGit(t, feature, "switch", "-c", "foreign")
	mirrorCommit(t, feature, "foreign.txt", "foreign\n")
	mirrorGit(t, feature, "push", "origin", "foreign")
	mirrorGit(t, feature, "switch", "-c", "own1")
	mirrorCommit(t, feature, "own1.txt", "own1\n")
	mirrorGit(t, feature, "switch", "-c", "own2")
	mirrorCommit(t, feature, "own2.txt", "own2\n")
	mainline := filepath.Join(fixture.root, "mainline")
	mirrorGit(t, fixture.root, "clone", "--branch", "main", fixture.remote, mainline)
	mirrorGit(t, mainline, "config", "user.name", "Owner")
	mirrorGit(t, mainline, "config", "user.email", "owner@example.test")
	onto := mirrorCommit(t, mainline, "main.txt", "main\n")
	mirrorGit(t, mainline, "push", "origin", "main")

	m := newTestGitMirror(t, fixture, 1<<30, 1<<30)
	own := []string{"refs/heads/own1", "refs/heads/own2"}
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, feature, own...), own); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if _, err := m.SeedRef(t.Context(), "owner/repo", "refs/heads/foreign"); err != nil {
		t.Fatalf("SeedRef: %v", err)
	}
	mirror := filepath.Join(fixture.root, "mirrors", "owner", "repo.git")
	line := []string{"refs/heads/foreign", "refs/heads/own1", "refs/heads/own2"}
	before := make(map[string]string, len(line))
	for _, ref := range line {
		before[ref] = mirrorRef(t, mirror, ref)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "mangled")
	// Only the first update-ref --stdin (the rebase install) is altered; the rollback runs unchanged.
	wrapper := "#!/bin/sh\ncase \" $* \" in\n  *' update-ref --stdin '*)\n    if [ ! -f \"$MIRROR_TEST_MARKER\" ]; then\n      : > \"$MIRROR_TEST_MARKER\"\n      sed 's#^update refs/heads/own2 \\([0-9a-f]*\\) [0-9a-f]*$#update refs/heads/own2 \\1 1111111111111111111111111111111111111111#' | \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n      exit $?\n    fi\n    ;;\nesac\nexec \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write Git wrapper: %v", err)
	}
	t.Setenv("MIRROR_TEST_REAL_GIT", realGit)
	t.Setenv("MIRROR_TEST_MARKER", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	heads, conflict, err := m.Rebase(t.Context(), "owner/repo", line, onto)
	if err == nil || conflict || !strings.Contains(err.Error(), "install rebased refs") {
		t.Fatalf("Rebase with stale expected-old = (%v, %t, %v), want install error", heads, conflict, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("update-ref --stdin was never intercepted: %v", err)
	}
	for _, ref := range line {
		if got := mirrorRef(t, mirror, ref); got != before[ref] {
			t.Fatalf("%s = %s after failed install, want unmoved %s", ref, got, before[ref])
		}
	}
}

func TestGitMirrorReplayFailureIsNotConflict(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	mirrorGit(t, fixture.work, "switch", "-c", "agent")
	mirrorCommit(t, fixture.work, "agent.txt", "agent\n")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	_, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, "refs/heads/unknown")
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
	// Advance main before the fetch so the line is behind and replay actually runs.
	mirrorGit(t, fixture.work, "switch", "main")
	mirrorCommit(t, fixture.work, "main2.txt", "main2\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", mirrorBundle(t, fixture.work, "refs/heads/agent"), []string{"refs/heads/agent"}); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	path, _, normalized, _, err := m.prepare(t.Context(), "owner/repo")
	if err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	if normalized == "" {
		t.Fatal("prepared mirror returned empty repository name")
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
	_, conflict, err := m.Rebase(t.Context(), "owner/repo", []string{"refs/heads/agent"}, "refs/remotes/origin/main")
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
			if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/agent"}); err != nil {
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
			_, err = m.ApplyBundle(t.Context(), "owner/repo", bundle, []string{"refs/heads/agent"})
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
			if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/agent"}); err != nil {
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

			_, err = m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/agent"})
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
	path, _, normalized, _, err := m.prepare(t.Context(), "owner/repo")
	if err != nil {
		t.Fatalf("prepare mirror: %v", err)
	}
	if normalized == "" {
		t.Fatal("prepared mirror returned empty repository name")
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
	err = m.Push(t.Context(), "owner/repo", map[string]string{"refs/heads/main": strings.TrimSpace(mirrorGit(t, fixture.work, "rev-parse", "HEAD"))}, Leases{"refs/heads/main": ""})
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
			_, err := m.ApplyBundle(t.Context(), "owner/repo", bundle, []string{"refs/heads/agent"})
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
	// CI runners have no global Git identity; fixture clones need one to commit.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mirrorRefExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	return exec.CommandContext(t.Context(), "git", "--git-dir", repo, "rev-parse", "--verify", "--quiet", ref).Run() == nil
}

// The fetch fits under the cap but the seeded branch ref crosses it: SeedRef
// refuses and the mirror's refs are back to their state before the call.
func TestGitMirrorSeedRefRefusesPostUpdateCapAndRestoresRefs(t *testing.T) {
	fixture := newMirrorFixture(t)
	mirrorCommit(t, fixture.work, "main.txt", "main\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	mirrorGit(t, fixture.root, "--git-dir", path, "gc", "--prune=now")
	before, err := m.refHeads(t.Context(), path)
	if err != nil {
		t.Fatalf("refs before: %v", err)
	}
	size, err := directoryBytes(path)
	if err != nil {
		t.Fatalf("measure mirror: %v", err)
	}
	// The refetch brings nothing new; the new loose branch ref is what crosses the cap.
	m.perRepoCapBytes = size + 8

	if _, err := m.SeedRef(t.Context(), "owner/repo", "refs/heads/main"); err == nil || !strings.Contains(err.Error(), "per-repository") {
		t.Fatalf("SeedRef over cap = %v, want per-repository cap refusal", err)
	}
	after, err := m.refHeads(t.Context(), path)
	if err != nil {
		t.Fatalf("refs after: %v", err)
	}
	if !maps.Equal(before, after) {
		t.Fatalf("refs after refused seed = %v, want restored %v", after, before)
	}
}

// A failed lease read after the fetch rolls the fetched tracking refs back.
func TestGitMirrorApplyBundleRestoresRefsWhenLeaseReadFails(t *testing.T) {
	fixture := newMirrorFixture(t)
	oldHead := mirrorCommit(t, fixture.work, "main.txt", "old\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")
	m := newTestGitMirror(t, fixture, 1<<30, 1<<30).(*gitMirror)
	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, nil); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	mirrorCommit(t, fixture.work, "main.txt", "new\n")
	mirrorGit(t, fixture.work, "push", "origin", "main")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find Git executable: %v", err)
	}
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\nprevious=\nfor arg do\n  if [ \"$previous\" = --verify ] && [ \"$arg\" = refs/remotes/origin/main ]; then\n    echo 'fatal: lease read refused' >&2\n    exit 128\n  fi\n  previous=\"$arg\"\ndone\nexec \"$MIRROR_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write Git wrapper: %v", err)
	}
	t.Setenv("MIRROR_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := m.ApplyBundle(t.Context(), "owner/repo", nil, []string{"refs/heads/main"}); err == nil || !strings.Contains(err.Error(), "read fetched lease") {
		t.Fatalf("ApplyBundle with failing lease read = %v, want the lease error", err)
	}
	path := filepath.Join(m.dir, "owner", "repo.git")
	out, err := exec.CommandContext(t.Context(), realGit, "--git-dir", path, "rev-parse", "refs/remotes/origin/main").Output()
	if err != nil {
		t.Fatalf("read tracking main: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != oldHead {
		t.Fatalf("tracking main after failed lease read = %s, want restored %s", got, oldHead)
	}
}
