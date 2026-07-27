package store

import (
	"crypto/rand"
	"encoding/hex"
)

// Page-size clamps for the paginated reads (ListMessages, SearchMessages). A
// caller's requested Limit is clamped to maxPageLimit and defaulted to
// defaultPageLimit when zero, so no read can demand an unbounded page
// (comms.proto:452-453,497-498 "the server clamps to a maximum").
const (
	defaultPageLimit uint32 = 50
	maxPageLimit     uint32 = 200
)

// newID mints a server-assigned stable id: 16 random bytes, hex-encoded. Used
// for every row's primary key (accounts, groups, channels, workspaces,
// messages) and for the ask correlation id. 128 bits of CSPRNG entropy makes a
// collision negligible without coordinating a sequence.
func newID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error on the platforms we target (it
	// reads the OS CSPRNG); a failure here is unrecoverable, so the read is
	// documented as infallible rather than threaded through every caller's
	// (T, error). See crypto/rand.Read godoc.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// clampLimit resolves a requested page limit to the store's bounds: zero
// becomes defaultPageLimit, anything above maxPageLimit is capped.
func clampLimit(requested uint32) uint32 {
	if requested == 0 {
		return defaultPageLimit
	}
	if requested > maxPageLimit {
		return maxPageLimit
	}
	return requested
}
