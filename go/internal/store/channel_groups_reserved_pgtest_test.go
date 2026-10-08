//go:build pgtest

package store

import "testing"

// TestCreateChannelGroupRefusesReservedTopLevelNames pins that a caller cannot
// squat a system group name at top level (nested reuse stays legal), and that no
// group name holds a slash, which agent tools read as a path separator.
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
		{"linear owner", NewChannelGroup{Name: linearRoutingGroupName, Visibility: VisibilityOwner}, true},
		{"linear shared", NewChannelGroup{Name: linearRoutingGroupName, Visibility: VisibilityShared}, true},
		{"dm nested", NewChannelGroup{Name: dmGroupName, ParentGroupID: parent.ID, Visibility: VisibilityOwner}, false},
		{"slash top level", NewChannelGroup{Name: "a/b", Visibility: VisibilityOwner}, true},
		{"slash nested", NewChannelGroup{Name: "a/b", ParentGroupID: parent.ID, Visibility: VisibilityOwner}, true},
		{"tilde top level", NewChannelGroup{Name: "~matt", Visibility: VisibilityOwner}, true},
		{"tilde nested", NewChannelGroup{Name: "~ops", ParentGroupID: parent.ID, Visibility: VisibilityOwner}, true},
		{"inner tilde", NewChannelGroup{Name: "a~b", Visibility: VisibilityOwner}, false},
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

// TestCreateChannelGroupUnderReservedDMGroupIsNotFound prevents child groups
// from nesting under the system-owned DM group.
func TestCreateChannelGroupUnderReservedDMGroupIsNotFound(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	// Materialize the reserved group by opening a real DM first.
	openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})
	reservedGroupID := dmGroupIDFor(t, s, owner.ID)

	_, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{
		Name: "reserved-child", ParentGroupID: reservedGroupID, Visibility: VisibilityOwner,
	})
	sentinelIs(t, err, ErrNotFound, "child under the reserved DM group")

	parent, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{
		Name: "normal-parent", Visibility: VisibilityOwner,
	})
	if err != nil {
		t.Fatalf("CreateChannelGroup(normal parent): %v", err)
	}
	if _, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{
		Name: "normal-child", ParentGroupID: parent.ID, Visibility: VisibilityOwner,
	}); err != nil {
		t.Fatalf("CreateChannelGroup(under normal parent): %v", err)
	}
}

// TestNestedReservedNameGroupIsOrdinary: a nested owner-visible __dm__ is not the
// reserved DM group, so its owner can create channels and child groups in it.
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
	if _, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "child", ParentGroupID: nested.ID, Visibility: VisibilityOwner}); err != nil {
		t.Fatalf("CreateChannelGroup under nested __dm__: %v, want success (not the reserved DM group)", err)
	}
}
