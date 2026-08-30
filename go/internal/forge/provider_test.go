package forge

// Contracts for the raw-forge error surface: ErrUnsupported is a distinct
// sentinel recoverable via errors.Is, and StatusError carries an HTTP status the
// later Service layer flattens (403/404) — recoverable via errors.As without
// inspecting the wire. Pure value assertions; no network.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestErrUnsupportedIsSentinel(t *testing.T) {
	wrapped := errors.Join(errors.New("context"), ErrUnsupported)
	if !errors.Is(wrapped, ErrUnsupported) {
		t.Error("ErrUnsupported not recoverable via errors.Is after wrapping")
	}
	if errors.Is(errors.New("unrelated"), ErrUnsupported) {
		t.Error("an unrelated error matched ErrUnsupported")
	}
}

func TestStatusError(t *testing.T) {
	base := &StatusError{Status: 403, Message: "forbidden"}
	if got, want := base.Error(), "forge: http 403: forbidden"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// A provider returns it; the Service recovers the status via errors.As
	// without touching the wire.
	var err error = base
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatal("StatusError not recoverable via errors.As")
	}
	if se.Status != 403 {
		t.Errorf("recovered status = %d, want 403", se.Status)
	}
}

func TestRateLimitError(t *testing.T) {
	// Error() renders the stable message.
	rle := &RateLimitError{RetryAfter: 60 * time.Second}
	if got, want := rle.Error(), "forge: rate budget exhausted"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// Unwrap keeps every existing errors.Is(ErrBudgetExhausted) site matching,
	// even through an fmt.Errorf("...: %w", ...) wrap.
	wrapped := fmt.Errorf("forge: github http 403: %w", rle)
	if !errors.Is(wrapped, ErrBudgetExhausted) {
		t.Error("RateLimitError not recoverable via errors.Is(ErrBudgetExhausted) after wrapping")
	}

	// errors.As recovers the hint through the same wrap.
	var got *RateLimitError
	if !errors.As(wrapped, &got) {
		t.Fatal("RateLimitError not recoverable via errors.As")
	}
	if got.RetryAfter != 60*time.Second {
		t.Errorf("recovered RetryAfter = %v, want 60s", got.RetryAfter)
	}

	// A zero-hint value still unwraps to the sentinel.
	if !errors.Is(&RateLimitError{}, ErrBudgetExhausted) {
		t.Error("zero-hint RateLimitError not recoverable via errors.Is(ErrBudgetExhausted)")
	}
}
