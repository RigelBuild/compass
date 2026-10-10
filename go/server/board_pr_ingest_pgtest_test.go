//go:build pgtest && unix

package server

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakePRReads serves one PR closing an issue and lists it as open.
type fakePRReads struct{ pr forge.PullRequest }

func (f *fakePRReads) GetPullRequest(context.Context, string, uint64) (forge.PullRequest, error) {
	return f.pr, nil
}

func (f *fakePRReads) ListOpenPullRequests(context.Context, string) ([]forge.UpdatedPull, error) {
	return []forge.UpdatedPull{{Number: f.pr.Number, State: "open", UpdatedAt: f.pr.UpdatedAt}}, nil
}

type notModifiedLister struct{}

func (notModifiedLister) ListUpdatedIssues(context.Context, string, time.Time, string) (forge.ConditionalResult[forge.UpdatedRows], error) {
	return forge.ConditionalResult[forge.UpdatedRows]{NotModified: true}, nil
}

// TestBoardReconcileBackfillAttachesOpenPR drives the backfill through the real
// store adapter: an enabled repo with a watermark but no backfill mark hydrates
// its open PR onto the closing-ref issue, then is marked once.
func TestBoardReconcileBackfillAttachesOpenPR(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	const repo = "owner/backfill"
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: forgeTestHost, Repo: repo, Enabled: true,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	adapter := &boardReconcileStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	if err := adapter.StoreRepoWatermark(ctx, repo, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), `"e"`); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	ref := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost}
	if err := brd.PublishIssueUpdate(ctx, &compassv1.Issue{Forge: ref, Repo: repo, Number: 2, Title: "t", ForgeState: "open"}); err != nil {
		t.Fatalf("PublishIssueUpdate: %v", err)
	}
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	reads := &fakePRReads{pr: forge.PullRequest{Number: 40, State: "open", CreatedAt: at, UpdatedAt: at,
		ClosingRefs: []forge.IssueRef{{Repo: repo, Number: 2}}}}
	marked := &markSignal{boardReconcileStore: adapter, done: make(chan struct{})}
	rc := ingest.NewBoardReconciler(notModifiedLister{}, ingest.NewIngester(nil, brd, ref), marked, ingest.BoardReconcileConfig{
		Pace: -1, Pulls: ingest.NewPullRequestHydrator(reads, brd, ref),
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = rc.Run(runCtx) }()
	select {
	case <-marked.done:
	case <-time.After(10 * time.Second):
		t.Fatal("backfill never marked the repo")
	}

	var prs []*compassv1.PullRequest
	for _, iss := range brd.Snapshot() {
		if iss.GetNumber() == 2 {
			prs = iss.GetPrs()
		}
	}
	if len(prs) != 1 || prs[0].GetNumber() != 40 {
		t.Fatalf("issue #2 prs = %v, want PR #40", prs)
	}
	if _, ok, err := adapter.PRsBackfilledAt(ctx, repo); err != nil || !ok {
		t.Fatalf("PRsBackfilledAt ok=%v err=%v, want marked", ok, err)
	}
}

// markSignal closes done once the backfill mark is written.
type markSignal struct {
	*boardReconcileStore
	done chan struct{}
}

func (m *markSignal) MarkPRsBackfilled(ctx context.Context, repo string, at time.Time) error {
	err := m.boardReconcileStore.MarkPRsBackfilled(ctx, repo, at)
	close(m.done)
	return err
}
