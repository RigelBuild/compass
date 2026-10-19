package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec" //nolint:depguard // the Server mirror drives its pinned Git executable with argument vectors
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Leases maps each ref to the remote OID a fetch saw for it; "" means the branch was absent.
type Leases map[string]string

// Mirror applies agent bundles and pushes only refs approved by its caller.
type Mirror interface {
	// ApplyBundle fetches the forge, installs refs from bundle, and returns each ref's lease from that fetch.
	ApplyBundle(ctx context.Context, repo string, bundle []byte, refs []string) (Leases, error)
	// Push sends refs atomically, each leased on exactly expected[ref]; an empty commit deletes the ref.
	Push(ctx context.Context, repo string, refs map[string]string, expected Leases) error
	// Rebase replays a line of refs, bottom to top, onto the given commit in one
	// replay. It returns each moved ref's new head; none move when refs[0] already contains it.
	Rebase(ctx context.Context, repo string, refs []string, onto string) (heads map[string]string, conflict bool, err error)
	// RefHead returns a local branch ref's commit OID; an absent ref returns "".
	RefHead(ctx context.Context, repo, ref string) (string, error)
	// CommitIdentities returns the author and committer emails for a branch or commit.
	CommitIdentities(ctx context.Context, repo, commit string) (authorEmail, committerEmail string, err error)
	// SeedRef points ref at the fetched remote head of the same branch and returns it, which is also its lease.
	SeedRef(ctx context.Context, repo, ref string) (string, error)
}

type mirrorToken func(ctx context.Context, repo string) (string, error)

type gitMirror struct {
	dir             string
	host            string
	perRepoCapBytes int64
	totalCapBytes   int64
	token           mirrorToken
	locksMu         sync.Mutex
	repoLocks       map[string]*sync.Mutex
	capMu           sync.Mutex
}

var _ Mirror = (*gitMirror)(nil)

// NewGitMirror creates a GitHub mirror. An empty token source is valid for
// local or unauthenticated remotes; HTTPS writes require a token source.
func NewGitMirror(dir string, perRepoCapBytes, totalCapBytes int64, token func(context.Context, string) (string, error)) Mirror {
	return NewGitMirrorWithHost(dir, "github.com", perRepoCapBytes, totalCapBytes, token)
}

// NewGitMirrorWithHost creates a mirror against github.com or a GitHub Enterprise host.
func NewGitMirrorWithHost(dir, host string, perRepoCapBytes, totalCapBytes int64, token func(context.Context, string) (string, error)) Mirror {
	return &gitMirror{
		dir:             dir,
		host:            host,
		perRepoCapBytes: perRepoCapBytes,
		totalCapBytes:   totalCapBytes,
		token:           token,
	}
}

func (m *gitMirror) ApplyBundle(ctx context.Context, repo string, bundle []byte, refs []string) (Leases, error) {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	m.capMu.Lock()
	defer m.capMu.Unlock()
	return m.applyBundle(ctx, repo, bundle, refs)
}

// RefHead reads a local branch tip while excluding mirror mutations.
func (m *gitMirror) RefHead(ctx context.Context, repo, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", fmt.Errorf("ref %q is not a branch ref", ref)
	}
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	path, _, normalized, _, err := m.prepare(ctx, repo)
	if err != nil {
		return "", err
	}
	if _, err := m.run(ctx, path, "", "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("invalid branch ref %q: %w", ref, err)
	}
	out, err := m.run(ctx, path, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if isMissingRef(err) || exitCode(err) == 1 {
			return "", nil
		}
		return "", fmt.Errorf("read local head %q in %s: %w", ref, normalized, err)
	}
	return strings.TrimSpace(out), nil
}

// CommitIdentities reads a branch tip or object ID while excluding mirror mutations.
func (m *gitMirror) CommitIdentities(ctx context.Context, repo, commit string) (string, string, error) {
	if !strings.HasPrefix(commit, "refs/heads/") && !isHexObjectID(commit) {
		return "", "", fmt.Errorf("invalid commit reference %q", commit)
	}
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	path, _, normalized, _, err := m.prepare(ctx, repo)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(commit, "refs/heads/") {
		if _, err := m.run(ctx, path, "", "check-ref-format", commit); err != nil {
			return "", "", fmt.Errorf("invalid commit branch ref %q: %w", commit, err)
		}
	}
	out, err := m.run(ctx, path, "", "log", "-1", "--format=%ae%x00%ce", commit)
	if err != nil {
		return "", "", fmt.Errorf("read commit identities for %q in %s: %w", commit, normalized, err)
	}
	identities := strings.TrimSuffix(out, "\n")
	parts := strings.Split(identities, "\x00")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("read commit identities for %q in %s: unexpected git output", commit, normalized)
	}
	return parts[0], parts[1], nil
}

func isHexObjectID(value string) bool {
	if len(value) < 4 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func isReplayConflict(err error) bool {
	return exitCode(err) == 1
}

func (m *gitMirror) Push(ctx context.Context, repo string, refs map[string]string, expected Leases) error {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	path, remote, normalized, _, err := m.prepare(ctx, repo)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	refNames := make([]string, 0, len(refs))
	for ref := range refs {
		refNames = append(refNames, ref)
	}
	sort.Strings(refNames)
	args := []string{"push", "--porcelain", "--atomic"}
	refspecs := make([]string, 0, len(refs))
	for _, ref := range refNames {
		if !strings.HasPrefix(ref, "refs/heads/") {
			return fmt.Errorf("push ref %q is not a branch ref", ref)
		}
		if _, err := m.run(ctx, path, "", "check-ref-format", ref); err != nil {
			return fmt.Errorf("invalid push ref %q: %w", ref, err)
		}
		old, ok := expected[ref]
		if !ok {
			return fmt.Errorf("push ref %s has no lease", ref)
		}
		commit := refs[ref]
		if commit == "" && old == "" {
			// The leased fetch saw no such branch, so the delete already happened.
			continue
		}
		args = append(args, "--force-with-lease="+ref+":"+old)
		if commit == "" {
			refspecs = append(refspecs, ":"+ref)
			continue
		}
		resolved, err := m.run(ctx, path, "", "rev-parse", "--verify", commit+"^{commit}")
		if err != nil {
			return fmt.Errorf("push %s commit %q is unavailable: %w", ref, commit, err)
		}
		refspecs = append(refspecs, strings.TrimSpace(resolved)+":"+ref)
	}
	if len(refspecs) == 0 {
		return nil
	}
	args = append(args, "origin")
	args = append(args, refspecs...)
	if _, err := m.run(ctx, path, remote, args...); err != nil {
		return fmt.Errorf("push refs for %s: %w", normalized, err)
	}
	return nil
}

func (m *gitMirror) Rebase(ctx context.Context, repo string, refs []string, onto string) (map[string]string, bool, error) {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	m.capMu.Lock()
	defer m.capMu.Unlock()
	path, _, normalized, _, err := m.prepare(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	if len(refs) == 0 {
		return nil, false, errors.New("rebase needs at least one ref")
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, false, fmt.Errorf("rebase ref %q is not a branch ref", ref)
		}
		if _, err := m.run(ctx, path, "", "check-ref-format", ref); err != nil {
			return nil, false, fmt.Errorf("invalid rebase ref %q: %w", ref, err)
		}
		if _, err := m.run(ctx, path, "", "cat-file", "-e", ref+"^{commit}"); err != nil {
			return nil, false, fmt.Errorf("rebase ref %q is unavailable: %w", ref, err)
		}
	}
	if _, err := m.run(ctx, path, "", "cat-file", "-e", onto+"^{commit}"); err != nil {
		return nil, false, fmt.Errorf("rebase target %q is unavailable: %w", onto, err)
	}
	bottom, top := refs[0], refs[len(refs)-1]
	if _, err := m.run(ctx, path, "", "merge-base", "--is-ancestor", onto, bottom); err == nil {
		return map[string]string{}, false, nil
	} else if exitCode(err) != 1 {
		return nil, false, fmt.Errorf("check %s against %s in %s: %w", bottom, onto, normalized, err)
	}
	refsBefore, err := m.rollbackSnapshotRefs(ctx, path)
	if err != nil {
		return nil, false, err
	}
	baseOut, err := m.run(ctx, path, "", "merge-base", bottom, onto)
	if err != nil {
		return nil, false, fmt.Errorf("find rebase base for %s: %w", normalized, err)
	}
	base := strings.TrimSpace(baseOut)
	if base == "" {
		return nil, false, fmt.Errorf("find rebase base for %s: git returned an empty merge base", normalized)
	}
	// Print mode moves no ref, so a conflict or a stray branch in the range changes nothing.
	out, err := m.run(ctx, path, "", "replay", "--contained", "--onto="+onto, "--ref-action=print", base+".."+top)
	if err != nil {
		if isReplayConflict(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("replay %s onto %s in %s: %w", top, onto, normalized, err)
	}
	updates, err := replayUpdates(out, refs)
	if err != nil {
		return nil, false, fmt.Errorf("replay %s onto %s in %s: %w", top, onto, normalized, err)
	}
	if _, err := m.runInput(ctx, path, updates, "", "update-ref", "--stdin"); err != nil {
		return nil, false, errors.Join(fmt.Errorf("install rebased refs for %s: %w", normalized, err), m.rollbackRefs(context.WithoutCancel(ctx), path, refsBefore))
	}
	if err := m.checkCaps(path); err != nil {
		return nil, false, errors.Join(err, m.rollbackRefs(ctx, path, refsBefore))
	}
	heads := make(map[string]string, len(refs))
	for _, ref := range refs {
		out, err := m.run(ctx, path, "", "rev-parse", "--verify", ref)
		if err != nil {
			return nil, false, errors.Join(
				fmt.Errorf("read rebased head for %s: %w", normalized, err),
				m.rollbackRefs(context.WithoutCancel(ctx), path, refsBefore),
			)
		}
		heads[ref] = strings.TrimSpace(out)
	}
	return heads, false, nil
}

// replayUpdates keeps only the requested refs from replay's printed updates,
// as one update-ref transaction, and requires each requested ref to be in the line.
func replayUpdates(out string, refs []string) (string, error) {
	want := make(map[string]bool, len(refs))
	for _, ref := range refs {
		want[ref] = true
	}
	var b strings.Builder
	b.WriteString("start\n")
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != "update" || !want[fields[1]] {
			continue
		}
		fmt.Fprintf(&b, "update %s %s %s\n", fields[1], fields[2], fields[3])
		delete(want, fields[1])
	}
	for _, ref := range refs {
		if want[ref] {
			return "", fmt.Errorf("ref %s is not in the replayed line", ref)
		}
	}
	b.WriteString("commit\n")
	return b.String(), nil
}

func (m *gitMirror) SeedRef(ctx context.Context, repo, ref string) (string, error) {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	m.capMu.Lock()
	defer m.capMu.Unlock()
	path, remote, normalized, _, err := m.prepare(ctx, repo)
	if err != nil {
		return "", err
	}
	refsBefore, err := m.rollbackSnapshotRefs(ctx, path)
	if err != nil {
		return "", err
	}
	if err := m.fetch(ctx, path, remote); err != nil {
		return "", fmt.Errorf("fetch forge refs for %s: %w", normalized, err)
	}
	if err := m.checkCaps(path); err != nil {
		return "", errors.Join(err, m.rollbackRefs(ctx, path, refsBefore))
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", fmt.Errorf("seed ref %q is not a branch ref", ref)
	}
	if _, err := m.run(ctx, path, "", "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("invalid seed ref %q: %w", ref, err)
	}
	tracking := "refs/remotes/origin/" + strings.TrimPrefix(ref, "refs/heads/")
	out, err := m.run(ctx, path, "", "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read fetched %s in %s: %w", tracking, normalized, err)
	}
	head := strings.TrimSpace(out)
	if _, err := m.run(ctx, path, "", "update-ref", ref, head); err != nil {
		return "", errors.Join(fmt.Errorf("seed %s in %s: %w", ref, normalized, err), m.rollbackRefs(ctx, path, refsBefore))
	}
	if err := m.checkCaps(path); err != nil {
		return "", errors.Join(err, m.rollbackRefs(ctx, path, refsBefore))
	}
	return head, nil
}

// trackingHead reads the fetched remote head of ref's branch; "" means the forge has no such branch.
func (m *gitMirror) trackingHead(ctx context.Context, path, ref string) (string, error) {
	tracking := "refs/remotes/origin/" + strings.TrimPrefix(ref, "refs/heads/")
	out, err := m.run(ctx, path, "", "rev-parse", "--verify", tracking)
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	if isMissingRef(err) {
		return "", nil
	}
	return "", fmt.Errorf("read fetched lease for %s: %w", ref, err)
}

// applyBundle fetches, reads the leases, and installs the bundle's refs; the
// caller holds the repo and cap locks.
func (m *gitMirror) applyBundle(ctx context.Context, repo string, bundle []byte, refs []string) (_ Leases, err error) {
	path, remote, normalized, created, err := m.prepare(ctx, repo)
	defer func() {
		if created && err != nil && path != "" {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove newly created git mirror for %s: %w", normalized, removeErr))
			}
		}
	}()
	if err != nil {
		return nil, err
	}
	if err := m.removeIncomingRefs(ctx, path); err != nil {
		return nil, err
	}
	refsBeforeFetch, err := m.rollbackSnapshotRefs(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := m.fetch(ctx, path, remote); err != nil {
		return nil, fmt.Errorf("fetch forge refs for %s: %w", normalized, err)
	}
	if err := m.checkCaps(path); err != nil {
		return nil, errors.Join(err, m.rollbackRefs(ctx, path, refsBeforeFetch))
	}
	// The leases come from this call's fetch, so a later fetch cannot widen what the push overwrites.
	leases := make(Leases, len(refs))
	for _, ref := range refs {
		lease, err := m.trackingHead(ctx, path, ref)
		if err != nil {
			return nil, errors.Join(err, m.rollbackRefs(context.WithoutCancel(ctx), path, refsBeforeFetch))
		}
		leases[ref] = lease
	}
	if len(bundle) == 0 {
		return leases, nil
	}
	bundlePath, err := writeMirrorBundle(bundle)
	if err != nil {
		return nil, err
	}
	defer func() {
		if removeErr := os.Remove(bundlePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary bundle: %w", removeErr))
		}
	}()
	if len(refs) == 0 {
		return leases, nil
	}
	refsBeforeBundle, err := m.rollbackSnapshotRefs(ctx, path)
	if err != nil {
		return nil, err
	}
	orderedRefs := append([]string(nil), refs...)
	sort.Strings(orderedRefs)
	args := []string{"fetch", "--no-tags", "--force", bundlePath}
	for _, ref := range orderedRefs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, fmt.Errorf("bundle ref %q is not a branch ref", ref)
		}
		if _, err := m.run(ctx, path, "", "check-ref-format", ref); err != nil {
			return nil, fmt.Errorf("invalid bundle ref %q: %w", ref, err)
		}
		stageRef := "refs/compass-incoming/" + strings.TrimPrefix(ref, "refs/heads/")
		args = append(args, "+"+ref+":"+stageRef)
	}
	if _, err := m.run(ctx, path, "", args...); err != nil {
		return nil, errors.Join(
			fmt.Errorf("apply git bundle for %s (bundle prerequisite may be missing): %w", normalized, err),
			m.rollbackRefs(ctx, path, refsBeforeBundle),
		)
	}
	for _, ref := range orderedRefs {
		stageRef := "refs/compass-incoming/" + strings.TrimPrefix(ref, "refs/heads/")
		out, err := m.run(ctx, path, "", "rev-parse", "--verify", stageRef+"^{commit}")
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read staged bundle ref %s: %w", ref, err), m.rollbackRefs(ctx, path, refsBeforeBundle))
		}
		if _, err := m.runInput(ctx, path, "", "", "update-ref", ref, strings.TrimSpace(out)); err != nil {
			return nil, errors.Join(fmt.Errorf("install bundle ref %s: %w", ref, err), m.rollbackRefs(ctx, path, refsBeforeBundle))
		}
	}
	if err := m.removeIncomingRefs(ctx, path); err != nil {
		return nil, errors.Join(err, m.rollbackRefs(ctx, path, refsBeforeBundle))
	}
	if err := m.checkCaps(path); err != nil {
		return nil, errors.Join(err, m.rollbackRefs(ctx, path, refsBeforeBundle))
	}
	return leases, nil
}

func (m *gitMirror) repoLock(repo string) *sync.Mutex {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	if m.repoLocks == nil {
		m.repoLocks = make(map[string]*sync.Mutex)
	}
	key := strings.ToLower(repo)
	if lock := m.repoLocks[key]; lock != nil {
		return lock
	}
	lock := new(sync.Mutex)
	m.repoLocks[key] = lock
	return lock
}

func (m *gitMirror) prepare(ctx context.Context, repo string) (path, remote, normalized string, created bool, err error) {
	if m.dir == "" {
		return "", "", "", false, errors.New("git mirror directory is not configured")
	}
	if m.perRepoCapBytes <= 0 || m.totalCapBytes <= 0 || m.perRepoCapBytes > m.totalCapBytes {
		return "", "", "", false, errors.New("git mirror size caps must be positive and the per-repository cap must not exceed the total cap")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return "", "", "", false, fmt.Errorf("invalid github repository %q", repo)
	}
	normalized = strings.ToLower(repo)
	parts[0] = strings.ToLower(parts[0])
	parts[1] = strings.ToLower(parts[1])
	host := strings.TrimSpace(m.host)
	if host == "" {
		host = "github.com"
	}
	base, err := mirrorBaseURL(host)
	if err != nil {
		return "", "", "", false, err
	}
	remote = strings.TrimSuffix(base, "/") + "/" + parts[0] + "/" + parts[1] + ".git"
	path = filepath.Join(m.dir, parts[0], parts[1]+".git")
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return "", "", "", false, fmt.Errorf("create git mirror directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); errors.Is(err, os.ErrNotExist) {
		created = true
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return path, remote, normalized, created, fmt.Errorf("create git mirror parent: %w", err)
		}
		if _, err := m.run(ctx, path, "", "init", "--bare", "--initial-branch=main", path); err != nil {
			return path, remote, normalized, created, fmt.Errorf("initialize git mirror for %s: %w", normalized, err)
		}
		if _, err := m.run(ctx, path, "", "remote", "add", "origin", remote); err != nil {
			return path, remote, normalized, created, fmt.Errorf("configure git mirror remote for %s: %w", normalized, err)
		}
	} else if err != nil {
		return path, remote, normalized, false, fmt.Errorf("inspect git mirror for %s: %w", normalized, err)
	}
	if _, err := m.run(ctx, path, "", "config", "remote.origin.url", remote); err != nil {
		return path, remote, normalized, created, fmt.Errorf("update git mirror remote for %s: %w", normalized, err)
	}
	return path, remote, normalized, created, nil
}

func (m *gitMirror) fetch(ctx context.Context, path, remote string) error {
	_, err := m.run(ctx, path, remote, "fetch", "--prune", "--no-tags", "origin",
		"+refs/heads/*:refs/remotes/origin/*")
	return err
}

func (m *gitMirror) checkCaps(repoPath string) error {
	repoBytes, err := directoryBytes(repoPath)
	if err != nil {
		return fmt.Errorf("measure per-repository git mirror: %w", err)
	}
	if repoBytes > m.perRepoCapBytes {
		return fmt.Errorf("git mirror per-repository size cap exceeded: %d > %d bytes", repoBytes, m.perRepoCapBytes)
	}
	totalBytes, err := directoryBytes(m.dir)
	if err != nil {
		return fmt.Errorf("measure total git mirror size: %w", err)
	}
	if totalBytes > m.totalCapBytes {
		return fmt.Errorf("git mirror total size cap exceeded: %d > %d bytes", totalBytes, m.totalCapBytes)
	}
	return nil
}

func (m *gitMirror) run(ctx context.Context, repoPath, remote string, args ...string) (string, error) {
	return m.runInput(ctx, repoPath, "", remote, args...)
}

func (m *gitMirror) runInput(ctx context.Context, repoPath, input, remote string, args ...string) (string, error) {
	gitArgs := make([]string, 0, len(args)+1)
	if repoPath != "" {
		gitArgs = append(gitArgs, "--git-dir", repoPath)
	}
	gitArgs = append(gitArgs, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...) //nolint:gosec // fixed executable; all Git input is passed as arguments, never through a shell
	// The Server has no Git config; replay keeps authors but needs a committer.
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_NAME=Compass", "GIT_COMMITTER_EMAIL=noreply@compass.invalid")
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var token string
	if remote != "" {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("parse git remote: %w", err)
		}
		if u.Scheme == "https" && m.token != nil {
			name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
			token, err = m.token(ctx, name)
			if err != nil {
				return "", fmt.Errorf("resolve git mirror token: %w", err)
			}
			if token == "" {
				return "", errors.New("git mirror token is empty")
			}
			header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
			cmd.Env = append(cmd.Env,
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http.extraHeader",
				"GIT_CONFIG_VALUE_0="+header,
			)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := string(out)
		if token != "" {
			for _, secret := range []string{token, "x-access-token:" + token, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))} {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(message))
	}
	return string(out), nil
}

func writeMirrorBundle(bundle []byte) (string, error) {
	file, err := os.CreateTemp("", "compass-git-bundle-*.bundle")
	if err != nil {
		return "", fmt.Errorf("create temporary git bundle: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(bundle); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("write temporary git bundle: %w", err), closeErr, removeErr)
	}
	if err := file.Close(); err != nil {
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("close temporary git bundle: %w", err), removeErr)
	}
	return path, nil
}

func directoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > math.MaxInt64-total {
			return errors.New("git mirror size overflow")
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func mirrorBaseURL(host string) (string, error) {
	if !strings.Contains(host, "://") {
		return "https://" + strings.TrimSuffix(host, "/"), nil
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme == "" || u.Scheme != "file" && u.Host == "" {
		return "", fmt.Errorf("invalid git mirror host %q", host)
	}
	return strings.TrimSuffix(host, "/"), nil
}

func validRepoPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, r := range part {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_.", r) {
			return false
		}
	}
	return true
}

func isMissingRef(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unknown revision") || strings.Contains(strings.ToLower(err.Error()), "needed a single revision") || strings.Contains(strings.ToLower(err.Error()), "not a valid object name")
}

func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func (m *gitMirror) refHeads(ctx context.Context, repoPath string) (map[string]string, error) {
	out, err := m.run(ctx, repoPath, "", "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, fmt.Errorf("read git mirror refs: %w", err)
	}
	refs := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[1]] = fields[0]
		}
	}
	return refs, nil
}

func (m *gitMirror) rollbackSnapshotRefs(ctx context.Context, repoPath string) (map[string]string, error) {
	refs, err := m.refHeads(ctx, repoPath)
	if err != nil {
		return nil, err
	}
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/compass-incoming/") {
			delete(refs, ref)
		}
	}
	return refs, nil
}

func (m *gitMirror) removeIncomingRefs(ctx context.Context, repoPath string) error {
	refs, err := m.run(ctx, repoPath, "", "for-each-ref", "--format=%(refname)", "refs/compass-incoming")
	if err != nil {
		return fmt.Errorf("list temporary incoming bundle refs: %w", err)
	}
	for ref := range strings.FieldsSeq(refs) {
		if _, err := m.run(ctx, repoPath, "", "update-ref", "-d", ref); err != nil {
			return fmt.Errorf("remove temporary incoming bundle ref %s: %w", ref, err)
		}
	}
	return nil
}

func (m *gitMirror) rollbackRefs(ctx context.Context, repoPath string, refsBefore map[string]string) error {
	refsAfter, err := m.refHeads(ctx, repoPath)
	if err != nil {
		return err
	}
	var updates strings.Builder
	for ref, head := range refsAfter {
		if old, exists := refsBefore[ref]; exists {
			if old != head {
				fmt.Fprintf(&updates, "update %s %s %s\n", ref, old, head)
			}
		} else {
			fmt.Fprintf(&updates, "delete %s %s\n", ref, head)
		}
	}
	for ref, old := range refsBefore {
		if _, exists := refsAfter[ref]; !exists {
			fmt.Fprintf(&updates, "create %s %s\n", ref, old)
		}
	}
	if updates.Len() > 0 {
		if _, err := m.runInput(ctx, repoPath, updates.String(), "", "update-ref", "--stdin"); err != nil {
			return fmt.Errorf("restore git mirror refs after cap refusal: %w", err)
		}
	}
	if _, err := m.run(ctx, repoPath, "", "reflog", "expire", "--expire=now", "--all"); err != nil {
		return fmt.Errorf("expire rejected git mirror reflogs: %w", err)
	}
	if _, err := m.run(ctx, repoPath, "", "gc", "--prune=now"); err != nil {
		return fmt.Errorf("prune rejected git mirror objects: %w", err)
	}
	return nil
}
