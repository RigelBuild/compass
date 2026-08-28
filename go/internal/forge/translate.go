package forge

// Pure forge→canonical mappers (#1018 ingestion translation). Each Translate*
// takes a raw forge value type and returns the forge-SUBSET of the canonical
// compass.v1 message: only forge-derived fields plus the caller-supplied agent
// attribution. Compass-owned machinery (id, lifecycle state, priority,
// assignee, summary, branch, prs, tracker, forge, repo) is left ZERO — the
// projection/store layer fills it.
//
// These mappers are PURE: they do not strip owner headers (the Service strips
// on read, DL-050, so a Service caller already passes the stripped body) and
// they do not parse attribution (the Service parses the owner header via
// StripOwner and hands the result in as attr).

import (
	"math"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// narrowNumber narrows a forge's uint64 issue/PR number to the canonical
// uint32. A real forge number fits uint32 in practice; the clamp prevents a
// silent high-bit-truncation collision if that ever breaks.
func narrowNumber(n uint64) uint32 {
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

// nilIfEmpty returns nil for an empty (or nil) slice so an empty-but-non-nil
// forge slice maps to a nil canonical slice, matching the per-element helpers
// below and the module's "empty inputs yield nil slices" contract.
func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// updatedAtProto maps a forge last-updated time to the canonical proto
// timestamp, returning nil for the zero time so a not-yet-populated
// UpdatedAt leaves the proto field unset (the store-side recency guard's
// NULL arm then keeps the write additive).
func updatedAtProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// TranslateIssue maps a raw forge Issue to the forge-subset canonical Issue,
// filling only forge-derived fields plus the passed-in agent attribution
// (nil ⇒ non-Compass author, left unset).
func TranslateIssue(in Issue, attr *compassv1.AgentAttribution) *compassv1.Issue {
	return &compassv1.Issue{
		Number:       narrowNumber(in.Number),
		Title:        in.Title,
		Body:         in.Body,
		ForgeState:   in.State,
		Url:          in.URL,
		ForgeAccount: in.ForgeAccount,
		Labels:       nilIfEmpty(in.Labels),
		Agent:        attr,
		UpdatedAt:    updatedAtProto(in.UpdatedAt),
	}
}

// TranslatePullRequest maps a raw forge PullRequest to the forge-subset
// canonical PullRequest. The canonical PR carries no body field; attribution
// comes solely from the passed-in attr. Forge/Repo are left zero (the
// caller/projection supplies them).
func TranslatePullRequest(in PullRequest, attr *compassv1.AgentAttribution) *compassv1.PullRequest {
	return &compassv1.PullRequest{
		Number:       narrowNumber(in.Number),
		Title:        in.Title,
		ForgeState:   in.State,
		Url:          in.URL,
		HeadRef:      in.HeadRef,
		BaseRef:      in.BaseRef,
		ForgeAccount: in.ForgeAccount,
		Draft:        in.Draft,
		Agent:        attr,
		Changed: &compassv1.ChangedStats{
			Files:     in.Changed.Files,
			Additions: in.Changed.Additions,
			Deletions: in.Changed.Deletions,
		},
		Checks:  TranslateChecks(in.Checks),
		Reviews: translateReviews(in.Reviews),
		Threads: translateThreads(in.Threads),
	}
}

// TranslateChecks maps a raw forge Checks roll-up to the canonical
// ChecksSummary.
func TranslateChecks(in Checks) *compassv1.ChecksSummary {
	return &compassv1.ChecksSummary{
		HeadSha: in.HeadSHA,
		State:   in.State,
		Checks:  translateChecks(in.Checks),
	}
}

func translateChecks(in []Check) []*compassv1.Check {
	if len(in) == 0 {
		return nil
	}
	out := make([]*compassv1.Check, len(in))
	for i, c := range in {
		out[i] = &compassv1.Check{
			Name:     c.Name,
			State:    c.State,
			Url:      c.URL,
			Required: c.Required,
		}
	}
	return out
}

func translateReviews(in []Review) []*compassv1.Review {
	if len(in) == 0 {
		return nil
	}
	out := make([]*compassv1.Review, len(in))
	for i, r := range in {
		out[i] = &compassv1.Review{
			Author:  r.Author,
			IsBot:   r.IsBot,
			Verdict: r.Verdict,
			Body:    r.Body,
		}
	}
	return out
}

func translateThreads(in []ReviewThread) []*compassv1.ReviewThread {
	if len(in) == 0 {
		return nil
	}
	out := make([]*compassv1.ReviewThread, len(in))
	for i, t := range in {
		out[i] = &compassv1.ReviewThread{
			Path:     t.Path,
			Resolved: t.Resolved,
			Comments: translateThreadComments(t.Comments),
		}
	}
	return out
}

func translateThreadComments(in []ThreadComment) []*compassv1.Comment {
	if len(in) == 0 {
		return nil
	}
	out := make([]*compassv1.Comment, len(in))
	for i, c := range in {
		out[i] = &compassv1.Comment{
			Author: c.Author,
			IsBot:  c.IsBot,
			Body:   c.Body,
		}
	}
	return out
}
