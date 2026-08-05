// Package ingest drives the forge read-side owner-strip + canonical translation
// for the Compass Server poll path (#1018): for each raw forge issue it strips
// the owner header, parses the display attribution, translates to the canonical
// compass.v1 Issue, stamps the forge coordinate the translator leaves zero, and
// hands the coordinate-complete Issue to a projection sink.
//
// Ingestion produces the CANONICAL proto and sinks it; it imports NO store. The
// proto->store mapping edge is part 4 (the sink's real implementation). The raw
// forge shape never escapes this package — the UI receives finished truth.
package ingest

import (
	"context"
	"fmt"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/forge"
)

// forgeReader is the read surface ingestion needs — a narrow view of
// forge.Provider (satisfied by *forge.FakeProvider and the real provider). Kept
// narrow so a test drives it without the write half of Provider.
type forgeReader interface {
	ListIssues(ctx context.Context, repo string, f forge.IssueFilter) ([]forge.Issue, error)
}

// issueSink receives each translated, coordinate-complete canonical Issue.
// Part 4's IssueProjection satisfies this (its PublishIssueUpdate maps proto ->
// store and persists+publishes). In this slice it is faked. It takes a
// *compassv1.Issue because the ingestion output is the CANONICAL type.
type issueSink interface {
	PublishIssueUpdate(ctx context.Context, issue *compassv1.Issue) error
}

// Ingester drives forge reads -> strip -> translate -> sink for one forge.
type Ingester struct {
	provider forgeReader
	sink     issueSink
	forgeRef *compassv1.ForgeRef // the provider's ForgeRef (provider+host); stamped onto each Issue
}

// NewIngester returns an Ingester that reads from p, stamps ref as the forge
// coordinate on each translated Issue, and sinks the result to sink.
func NewIngester(p forgeReader, sink issueSink, ref *compassv1.ForgeRef) *Ingester {
	return &Ingester{provider: p, sink: sink, forgeRef: ref}
}

// Ingest fetches every issue for repo, translates each (owner header stripped +
// parsed into attribution), stamps the forge coordinate, and sinks the canonical
// Issue. It stops and returns the first provider/sink error (wrapped, so callers
// can errors.Is the cause); partial progress is fine — a re-poll is idempotent
// on the coordinate.
func (in *Ingester) Ingest(ctx context.Context, repo string) error {
	raws, err := in.provider.ListIssues(ctx, repo, forge.IssueFilter{})
	if err != nil {
		return fmt.Errorf("ingest: list issues for %q: %w", repo, err)
	}
	for _, raw := range raws {
		issue := in.translateOne(raw, repo)
		if err := in.sink.PublishIssueUpdate(ctx, issue); err != nil {
			return fmt.Errorf("ingest: publish issue #%d for %q: %w", raw.Number, repo, err)
		}
	}
	return nil
}

// translateOne strips the owner header off a raw forge issue's body BEFORE
// translation (TranslateIssue is pure and copies the body verbatim — it does not
// strip, so the strip MUST happen here or the header leaks onto the canonical
// Body), parses the display attribution, translates, and stamps the forge
// coordinate the translator deliberately leaves zero.
func (in *Ingester) translateOne(raw forge.Issue, repo string) *compassv1.Issue {
	clean, author, ok := forge.StripOwner(raw.Body)
	raw.Body = clean

	var attr *compassv1.AgentAttribution
	if ok && author.AgentHandle != "" {
		attr = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
	}

	issue := forge.TranslateIssue(raw, attr)
	// Clone (don't alias) the shared ForgeRef message onto each Issue.
	issue.Forge = &compassv1.ForgeRef{
		Provider: in.forgeRef.GetProvider(),
		Host:     in.forgeRef.GetHost(),
	}
	issue.Repo = repo
	return issue
}
