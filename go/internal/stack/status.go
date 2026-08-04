//go:build unix

package stack

// StatusState is the lifecycle state of a supervised stack.
type StatusState int

const (
	// StatusUnknown is the zero value — no state has been established.
	StatusUnknown StatusState = iota
	// StatusStarting: the spawn sequence is in progress.
	StatusStarting
	// StatusReady: this process spawned the chain and the server answers.
	StatusReady
	// StatusAttached: a live server was already answering, so Up attached to the
	// existing stack rather than spawning; this process owns no children.
	StatusAttached
	// StatusFailed: the stack is not answering (a failed spawn or a dead server).
	StatusFailed
)

// String renders the state for logs and error details.
func (s StatusState) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusReady:
		return "ready"
	case StatusAttached:
		return "attached"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Status is a state plus a human-legible detail (a version string on attach, a
// failure reason on failure).
type Status struct {
	State  StatusState
	Detail string
}
