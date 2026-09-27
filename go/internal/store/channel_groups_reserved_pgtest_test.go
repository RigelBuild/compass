//go:build pgtest

package store

import "testing"

// TestCreateChannelGroupRefusesReservedTopLevelNames pins that a caller cannot
// squat a system group name at top level, while nested reuse stays legal.
func TestCreateChannelGroupRefusesReservedTopLevelNames(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	parent, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "normal", Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	cases := []struct {
		name    string
		group   NewChannelGroup
		refused bool
	}{
		{"dm owner", NewChannelGroup{Name: dmGroupName, Visibility: VisibilityOwner}, true},
		{"dm shared", NewChannelGroup{Name: dmGroupName, Visibility: VisibilityShared}, true},
		{"coordination owner", NewChannelGroup{Name: coordinationGroupName, Visibility: VisibilityOwner}, true},
		{"coordination shared", NewChannelGroup{Name: coordinationGroupName, Visibility: VisibilityShared}, true},
		{"dm nested", NewChannelGroup{Name: dmGroupName, ParentGroupID: parent.ID, Visibility: VisibilityOwner}, false},
		{"ordinary", NewChannelGroup{Name: "ordinary", Visibility: VisibilityShared}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateChannelGroup(t.Context(), owner.ID, tc.group)
			if tc.refused {
				sentinelIs(t, err, ErrInvalidArgument, "CreateChannelGroup("+tc.group.Name+")")
				return
			}
			if err != nil {
				t.Fatalf("CreateChannelGroup(%s): %v", tc.group.Name, err)
			}
		})
	}
}

// TestNestedReservedNameGroupIsOrdinary: a nested owner-visible __dm__ is not the
// reserved DM group, so its owner can create channels in it.
func TestNestedReservedNameGroupIsOrdinary(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	parent, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "normal", Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	nested, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: dmGroupName, ParentGroupID: parent.ID, Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("create nested __dm__: %v", err)
	}
	if _, err := s.CreateChannel(t.Context(), owner.ID, NewChannel{Name: "notes", GroupID: nested.ID}); err != nil {
		t.Fatalf("CreateChannel into nested __dm__: %v, want success (not the reserved DM group)", err)
	}
}
