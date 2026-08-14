package store

import (
	"fmt"
	"regexp"
)

// handleRE is the positive grammar every account handle must fully match: the
// server mention grammar (delivery.mentionRE, consumer.go:319) anchored and
// stripped of the leading `@`. It is deliberately NOT case-insensitive — a
// handle is stored lowercase, so an uppercase handle like `Compass` is rejected
// rather than folded, which is what forecloses the uppercase/Unicode-confusable
// spoof class (e.g. a zero-width-space handle `Compass\u200b` that renders as
// `Compass`) along with a leading-`@` or whitespace handle.
var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// reservedHandles are handles no account may register. `compass` is the system
// handle; `everyone`/`agents`/`users` are the server's reserved broadcast
// mentions — an account registered as one of those would shadow a live
// broadcast semantic. The reserved-mention names duplicate delivery's
// reservedMentions (consumer.go:326, the source of truth) rather than importing
// the delivery package, to avoid a store->delivery dependency; keep them in
// sync.
var reservedHandles = map[string]bool{
	"compass":  true,
	"everyone": true,
	"agents":   true,
	"users":    true,
}

// validateHandle rejects reserved and malformed handles. ErrInvalidArgument on
// failure. It enforces the positive mention grammar (handleRE) and a
// reserved-name blocklist (reservedHandles); the grammar guarantees the handle
// is already lowercase, so the blocklist compares against it directly.
func validateHandle(handle string) error {
	if !handleRE.MatchString(handle) {
		return fmt.Errorf("%w: handle %q is malformed", ErrInvalidArgument, handle)
	}
	if reservedHandles[handle] {
		return fmt.Errorf("%w: handle %q is reserved", ErrInvalidArgument, handle)
	}
	return nil
}
