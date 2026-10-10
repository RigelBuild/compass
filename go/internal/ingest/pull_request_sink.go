package ingest

import (
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
)

// IngestedPullRequest carries what the wire PullRequest lacks: forge times and closing refs.
type IngestedPullRequest struct {
	PR          *compassv1.PullRequest
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosingRefs []forge.IssueRef
}
