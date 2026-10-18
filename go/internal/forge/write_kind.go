package forge

// WriteKind identifies a forge object created under a write intent.
type WriteKind uint8

const (
	WriteKindIssue       WriteKind = 1
	WriteKindPullRequest WriteKind = 2
	WriteKindComment     WriteKind = 3
	WriteKindReview      WriteKind = 4
)

// String returns the persisted name of the write kind.
func (k WriteKind) String() string {
	switch k {
	case WriteKindIssue:
		return "issue"
	case WriteKindPullRequest:
		return "pull_request"
	case WriteKindComment:
		return "comment"
	case WriteKindReview:
		return "review"
	default:
		return "unknown"
	}
}
