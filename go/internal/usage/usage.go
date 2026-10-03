// Package usage is the Plane-A token-usage contract: the event the LLM gateway
// emits, the Store seam every backend satisfies, and the in-memory reference.
package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ErrInvalidArgument marks a malformed event batch or query. It is returned
// before any write, so a rejected batch leaves no trace.
var ErrInvalidArgument = errors.New("usage: invalid argument")

// The Outcome values. Failed and aborted calls still carry the partial usage
// the provider billed, so they are recorded and rolled up like "ok".
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeAborted = "aborted"
)

// TokenUsageEvent is one completed upstream model call. Money is integer
// micro-USD so that no float reaches a store.
type TokenUsageEvent struct {
	ID               string // server-assigned UUID
	OccurredAtUnixMs int64
	AgentAccountID   string // the calling agent (attribution spine)
	OwnerUserID      string // rollup key only; the store resolves tenancy, never from this field
	SessionID        string // runner session id, when known
	RequestID        string // gateway request UUID
	Provider         string // e.g. "anthropic"
	Model            string // resolved model id
	CredentialID     string // which pool account served it
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	CostMicroUSD     int64  // total; computed gateway-side from the pricing table
	RateVersion      string // pricing-table version applied at write time
	Outcome          string // "ok" | "error" | "aborted" (partial usage still recorded)
}

// Granularity is a rollup bucket width. Backends persist these values, so
// never renumber them.
type Granularity int32

// The rollup granularities. Unspecified is the zero value a query must not use.
const (
	GranularityUnspecified Granularity = iota
	GranularityHour
	GranularityDay
)

const (
	hourMs = int64(time.Hour / time.Millisecond)
	dayMs  = 24 * hourMs
)

// The unix milliseconds pgx can send as a timestamptz. Postgres starts at
// 4714-11-24 BC, and pgx overflows int64 microseconds past MaxInt64/1000.
const (
	minTimestamptzMs = -210_866_803_200_000
	maxTimestamptzMs = math.MaxInt64 / 1000
)

// SeriesQuery selects the rollup buckets whose start lies in
// [StartUnixMs, EndUnixMs).
type SeriesQuery struct {
	Granularity Granularity
	StartUnixMs int64
	EndUnixMs   int64
	// AgentAccountIDs narrows the read to these agents; empty means all. It is
	// a set so that a subtree read can resolve the tree once and sum once.
	AgentAccountIDs []string
	// Provider narrows the read to one provider; empty means all.
	Provider string
}

// Bucket is one bucket's usage, summed over every rollup row a query matched.
type Bucket struct {
	StartUnixMs      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
	CostMicroUSD     int64
}

// ComputeInterval is one completed, or currently open, agent-active interval.
// EndUnixMs is zero while open; only closed intervals are rolled up.
type ComputeInterval struct {
	IntervalID     string
	StartUnixMs    int64
	EndUnixMs      int64
	AgentAccountID string
	OwnerUserID    string
}

// ComputeBucket holds active duration and the number of intervals that started
// in one UTC bucket.
type ComputeBucket struct {
	StartUnixMs int64
	ActiveMs    int64
	Intervals   int64
}

// Store is the Plane-A usage store seam: an append-only event write plus rollup
// reads. Each call is scoped to ctx's tenant, which the backend resolves, except
// the two Prune calls: they advance a global horizon and prune every tenant.
type Store interface {
	AppendTokenUsage(ctx context.Context, events []TokenUsageEvent) error
	TokenUsageSeries(ctx context.Context, query SeriesQuery) ([]Bucket, error)
	RebuildTokenUsageRollups(ctx context.Context) error
	PruneTokenUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error)
	ComputeUsageSeries(ctx context.Context, query SeriesQuery) ([]ComputeBucket, error)
	RebuildComputeUsageRollups(ctx context.Context) error
	PruneComputeUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error)
}

// BucketStart returns the start of the UTC-aligned bucket that holds unixMs,
// or unixMs unchanged for a granularity no rollup is kept at.
func (g Granularity) BucketStart(unixMs int64) int64 {
	width := g.widthMs()
	if width == 0 {
		return unixMs
	}
	return unixMs - unixMs%width
}

// widthMs is the bucket width, or 0 for a granularity no rollup is kept at.
func (g Granularity) widthMs() int64 {
	switch g {
	case GranularityHour:
		return hourMs
	case GranularityDay:
		return dayMs
	case GranularityUnspecified:
		return 0
	}
	return 0
}

// Validate rejects a granularity that no rollup is kept at. Without the check,
// the read would return an empty series and hide the error.
func (q *SeriesQuery) Validate() error {
	if q.Granularity.widthMs() == 0 {
		return fmt.Errorf("%w: granularity %d has no rollup", ErrInvalidArgument, q.Granularity)
	}
	return nil
}

// ValidateEvents rejects a batch that holds an event the rollups cannot key or
// sum. Backends call it before they write, so a bad batch writes nothing.
func ValidateEvents(events []TokenUsageEvent) error {
	for i := range events {
		if problem := events[i].problem(); problem != "" {
			return fmt.Errorf("%w: event %d (%q): %s", ErrInvalidArgument, i, events[i].ID, problem)
		}
	}
	return nil
}

// problem names the first defect in e, or returns "" if e is valid.
func (e *TokenUsageEvent) problem() string {
	switch {
	case e.ID == "":
		return "empty id"
	case e.OccurredAtUnixMs <= 0:
		return "non-positive occurred_at"
	case e.OccurredAtUnixMs > maxTimestamptzMs:
		return "occurred_at past the last storable timestamp"
	case e.AgentAccountID == "", e.OwnerUserID == "", e.Provider == "", e.Model == "":
		return "empty rollup key (agent, owner, provider, model)"
	case e.InputTokens < 0, e.OutputTokens < 0, e.CacheReadTokens < 0, e.CacheWriteTokens < 0,
		e.TotalTokens < 0, e.CostMicroUSD < 0:
		return "negative token count or cost"
	case e.Outcome != OutcomeOK && e.Outcome != OutcomeError && e.Outcome != OutcomeAborted:
		return "unknown outcome " + strconv.Quote(e.Outcome)
	}
	return ""
}

// usage is the event's contribution to one rollup bucket.
func (e *TokenUsageEvent) usage() Bucket {
	return Bucket{
		InputTokens:      e.InputTokens,
		OutputTokens:     e.OutputTokens,
		CacheReadTokens:  e.CacheReadTokens,
		CacheWriteTokens: e.CacheWriteTokens,
		TotalTokens:      e.TotalTokens,
		CostMicroUSD:     e.CostMicroUSD,
	}
}

// plus adds o's sums to b and keeps b's StartUnixMs.
func (b Bucket) plus(o Bucket) Bucket {
	b.InputTokens += o.InputTokens
	b.OutputTokens += o.OutputTokens
	b.CacheReadTokens += o.CacheReadTokens
	b.CacheWriteTokens += o.CacheWriteTokens
	b.TotalTokens += o.TotalTokens
	b.CostMicroUSD += o.CostMicroUSD
	return b
}
