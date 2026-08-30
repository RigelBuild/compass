//go:build pgtest

package store

// ChannelByNameForViewer: the viewer-scoped name->id resolve the agent tool edge
// runs ahead of the id-typed store calls (peer-DM record R1). Every test here
// defends one arm of its contract: a visible name resolves to exactly its
// channel; an unknown OR invisible name collapses to the same ErrNotFound (the
// D9 not-found/forbidden merge, so a probe cannot enumerate names it lacks
// visibility for); and two visible channels sharing a name are ErrInvalidArgument
// (there is no ErrAmbiguous sentinel — invalid_argument naming the collision is
// the R1 rule).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestChannelByNameForViewerHit: a channel the viewer is a member of resolves by
// name to that exact channel.
func TestChannelByNameForViewerHit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "war-room", Kind: ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	got, err := s.ChannelByNameForViewer(ctx, owner.ID, "war-room")
	if err != nil {
		t.Fatalf("ChannelByNameForViewer(hit): %v", err)
	}
	if got.ID != ch.ID {
		t.Fatalf("resolved id = %q, want %q", got.ID, ch.ID)
	}
	if got.Name != "war-room" {
		t.Fatalf("resolved name = %q, want %q", got.Name, "war-room")
	}
}

// TestChannelByNameForViewerMissIsNotFound: a name no channel carries is
// ErrNotFound — never a distinct "no such name" that a caller could tell apart
// from an invisible one.
func TestChannelByNameForViewerMissIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	_, err := s.ChannelByNameForViewer(ctx, owner.ID, "ghost")
	sentinelIs(t, err, ErrNotFound, "unknown channel name")
}

// TestChannelByNameForViewerInvisibleIsNotFound: a channel that genuinely EXISTS
// but the viewer may not see resolves to the SAME ErrNotFound an unknown name
// gets — the not-found/forbidden merge, so an outsider cannot confirm a private
// channel's name exists by resolving it. An ungrouped channel is owner-scoped
// (visible only through membership), so the outsider is a non-member and the
// resolve must miss.
func TestChannelByNameForViewerInvisibleIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	outsider := mustUser(t, s, "outsider")

	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "secret-room", Kind: ChannelKindChannel}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// The owner resolves it (control: it really exists and is visible to a member).
	if _, err := s.ChannelByNameForViewer(ctx, owner.ID, "secret-room"); err != nil {
		t.Fatalf("owner resolve of own channel: %v", err)
	}
	// The outsider gets ErrNotFound — indistinguishable from an unknown name.
	_, err := s.ChannelByNameForViewer(ctx, outsider.ID, "secret-room")
	sentinelIs(t, err, ErrNotFound, "invisible channel resolves like unknown")
}

// TestChannelByNameForViewerAmbiguousIsInvalidArgument: two channels the viewer
// can see sharing a name is ErrInvalidArgument naming the collision, not a
// silent pick of one. Ungrouped channels are name-unconstrained
// (channels_group_name_key is partial WHERE group_id IS NOT NULL), so two
// same-named ungrouped channels both visible to their owner is the achievable
// collision.
func TestChannelByNameForViewerAmbiguousIsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "dupe", Kind: ChannelKindChannel}); err != nil {
		t.Fatalf("CreateChannel(#1): %v", err)
	}
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{Name: "dupe", Kind: ChannelKindChannel}); err != nil {
		t.Fatalf("CreateChannel(#2): %v", err)
	}

	_, err := s.ChannelByNameForViewer(ctx, owner.ID, "dupe")
	sentinelIs(t, err, ErrInvalidArgument, "ambiguous channel name")
	// The error names the collision so the caller can disambiguate.
	if err != nil && !strings.Contains(err.Error(), "dupe") {
		t.Fatalf("ambiguous error %q does not name the colliding name", err)
	}
	// And it is NOT ErrNotFound — a real collision is a caller mistake to fix,
	// not an indistinguishable miss.
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("ambiguous resolve returned ErrNotFound, want ErrInvalidArgument only")
	}
}
