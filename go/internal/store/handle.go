package store

import (
	"fmt"
	"regexp"
)

// handleRE matches the lowercase ASCII handle grammar and excludes leading @,
// whitespace, and Unicode-confusable spellings.
var handleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// reservedHandles blocks system handles and broadcast mentions. The mention
// names mirror delivery's list to avoid a store-to-delivery dependency.
var reservedHandles = map[string]bool{
	"compass":  true,
	"linear":   true,
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
