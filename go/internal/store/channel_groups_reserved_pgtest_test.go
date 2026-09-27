//go:build pgtest

package store

import "testing"

// TestCreateChannelGroupRefusesReservedTopLevelNames pins that a caller cannot
// squat a system group name at top level, while nested reuse stays legal.
func TestCreateChannelGroupRefusesReservedTopLevelNames(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	parent, err := s.CreateChannelGroup(t.Context(), owner.ID, NewChannelGroup{Name: "normal", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	cases := []struct {
		name    string
		group   NewChannelGroup
		refused bool
	}{
		{"dm owner", NewChannelGroup{Name: "__dm__", Visibility: VisibilityOwner}, true},
		{"dm shared", NewChannelGroup{Name: "__dm__", Visibility: VisibilityShared}, true},
		{"coordination shared", NewChannelGroup{Name: "__coordination__", Visibility: VisibilityShared}, true},
		{"dm nested", NewChannelGroup{Name: "__dm__", ParentGroupID: parent.ID, Visibility: VisibilityShared}, false},
		{"ordinary", NewChannelGroup{Name: "ordinary", Visibility: VisibilityShared}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateChannelGroup(t.Context(), owner.ID, tc.group)
			if tc.refused {
				sentinelIs(t, err, ErrNotFound, "CreateChannelGroup("+tc.group.Name+")")
				return
			}
			if err != nil {
				t.Fatalf("CreateChannelGroup(%s): %v", tc.group.Name, err)
			}
		})
	}
}
