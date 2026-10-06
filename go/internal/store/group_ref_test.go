package store

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveGroupRef(t *testing.T) {
	groups := []ChannelGroup{
		{ID: "alpha", Name: "alpha"},
		{ID: "beta", Name: "beta"},
		{ID: "alpha-svc", Name: "svc", ParentGroupID: "alpha"},
		{ID: "beta-svc", Name: "svc", ParentGroupID: "beta"},
		{ID: "alpha-dup-1", Name: "dup", ParentGroupID: "alpha"},
		{ID: "alpha-dup-2", Name: "dup", ParentGroupID: "alpha"},
		{ID: "root-svc-1", Name: "svc"},
		{ID: "root-svc-2", Name: "svc"},
		{ID: "root-svc-child", Name: "nested", ParentGroupID: "root-svc-1"},
		{ID: "beta-svc-leaf", Name: "leaf", ParentGroupID: "beta-svc"},
		// The viewer's own top-level eng beside a stranger's shared one.
		{ID: "viewer-eng", Name: "eng", NamespaceOwnerID: "viewer-id"},
		{ID: "stranger-eng", Name: "eng", NamespaceOwnerID: "stranger-id"},
		// A top-level infra beside an infra nested under the viewer's eng.
		{ID: "top-infra", Name: "infra", NamespaceOwnerID: "viewer-id"},
		{ID: "eng-infra", Name: "infra", ParentGroupID: "viewer-eng", NamespaceOwnerID: "viewer-id"},
	}
	handles := map[AccountID]string{"viewer-id": "viewer", "stranger-id": "stranger"}

	tests := []struct {
		name    string
		ref     string
		wantID  ChannelGroupID
		wantErr error
	}{
		{name: "leaf hit", ref: "alpha", wantID: "alpha"},
		{name: "leaf miss", ref: "unknown", wantErr: ErrNotFound},
		{name: "ambiguous leaf", ref: "svc", wantErr: ErrInvalidArgument},
		{name: "path disambiguates repeated leaf", ref: "beta/svc", wantID: "beta-svc"},
		{name: "ambiguous intermediate path resolves unique final group", ref: "svc/nested", wantID: "root-svc-child"},
		{name: "path starts at the root, not at a nested group", ref: "svc/leaf", wantErr: ErrNotFound},
		{name: "full path reaches the nested leaf", ref: "beta/svc/leaf", wantID: "beta-svc-leaf"},
		{name: "missing intermediate path segment", ref: "alpha/missing/svc", wantErr: ErrNotFound},
		{name: "path ends in same-named siblings", ref: "alpha/dup", wantErr: ErrInvalidArgument},
		{name: "empty ref", ref: "", wantErr: ErrInvalidArgument},
		{name: "trailing slash", ref: "a/", wantErr: ErrInvalidArgument},
		{name: "doubled slash", ref: "a//b", wantErr: ErrInvalidArgument},
		{name: "bare slash", ref: "/", wantErr: ErrInvalidArgument},
		{name: "cross-owner top-level leaf is ambiguous", ref: "eng", wantErr: ErrInvalidArgument},
		{name: "anchored cross-owner top level stays ambiguous", ref: "/eng", wantErr: ErrInvalidArgument},
		{name: "owner qualifier picks the viewer's group", ref: "/~viewer/eng", wantID: "viewer-eng"},
		{name: "owner qualifier picks the stranger's group", ref: "/~stranger/eng", wantID: "stranger-eng"},
		{name: "owner qualifier with a path", ref: "/~viewer/eng/infra", wantID: "eng-infra"},
		{name: "unknown owner qualifier", ref: "/~nobody/eng", wantErr: ErrNotFound},
		{name: "owner qualifier needs a group", ref: "/~viewer", wantErr: ErrInvalidArgument},
		{name: "empty owner qualifier", ref: "/~/eng", wantErr: ErrInvalidArgument},
		{name: "tilde is only an owner qualifier when anchored", ref: "~viewer/eng", wantErr: ErrNotFound},
		{name: "top-level vs nested leaf is ambiguous", ref: "infra", wantErr: ErrInvalidArgument},
		{name: "anchor picks the top-level group", ref: "/infra", wantID: "top-infra"},
		{name: "unanchored path picks the nested group", ref: "eng/infra", wantID: "eng-infra"},
		{name: "anchored path through an ambiguous top level", ref: "/eng/infra", wantID: "eng-infra"},
		{name: "anchor never matches a nested group", ref: "/leaf", wantErr: ErrNotFound},
		{name: "qualifier ignores case", ref: "/~Viewer/eng", wantID: "viewer-eng"},
		{name: "qualifier strips one leading @", ref: "/~@stranger/eng", wantID: "stranger-eng"},
		{name: "qualifier strips only one @", ref: "/~@@stranger/eng", wantErr: ErrNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveGroupRef(groups, handles, tt.ref)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveGroupRef(%q) error = %v, want %v", tt.ref, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveGroupRef(%q): %v", tt.ref, err)
			}
			if got.ID != tt.wantID {
				t.Fatalf("resolveGroupRef(%q) id = %q, want %q", tt.ref, got.ID, tt.wantID)
			}
		})
	}
}

// TestResolveGroupRefAmbiguityNamesTheFix checks that an ambiguous ref names a
// ref that resolves each match, so a caller can retry without guessing.
func TestResolveGroupRefAmbiguityNamesTheFix(t *testing.T) {
	groups := []ChannelGroup{
		{ID: "viewer-eng", Name: "eng", NamespaceOwnerID: "viewer-id"},
		{ID: "stranger-eng", Name: "eng", NamespaceOwnerID: "stranger-id"},
		{ID: "top-infra", Name: "infra", NamespaceOwnerID: "viewer-id"},
		{ID: "eng-infra", Name: "infra", ParentGroupID: "viewer-eng", NamespaceOwnerID: "viewer-id"},
	}
	handles := map[AccountID]string{"viewer-id": "viewer", "stranger-id": "stranger"}

	for _, tt := range []struct {
		ref   string
		hints map[string]ChannelGroupID
	}{
		{ref: "eng", hints: map[string]ChannelGroupID{"/~viewer/eng": "viewer-eng", "/~stranger/eng": "stranger-eng"}},
		{ref: "infra", hints: map[string]ChannelGroupID{"/infra": "top-infra", "/eng/infra": "eng-infra"}},
	} {
		_, err := resolveGroupRef(groups, handles, tt.ref)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("resolveGroupRef(%q) error = %v, want invalid argument", tt.ref, err)
		}
		for hint, wantID := range tt.hints {
			if !strings.Contains(err.Error(), hint) {
				t.Errorf("resolveGroupRef(%q) error %q does not name %q", tt.ref, err, hint)
			}
			got, err := resolveGroupRef(groups, handles, hint)
			if err != nil || got.ID != wantID {
				t.Errorf("hint %q resolves to %q, %v; want %q", hint, got.ID, err, wantID)
			}
		}
	}
}

// TestResolveGroupRefHintForLegacyTildeName: a pre-guard top-level name that
// starts with '~' would parse as an owner qualifier, so its hint is qualified.
func TestResolveGroupRefHintForLegacyTildeName(t *testing.T) {
	groups := []ChannelGroup{
		{ID: "top-ops", Name: "~ops", NamespaceOwnerID: "viewer-id"},
		{ID: "eng", Name: "eng", NamespaceOwnerID: "viewer-id"},
		{ID: "eng-ops", Name: "~ops", ParentGroupID: "eng", NamespaceOwnerID: "viewer-id"},
	}
	handles := map[AccountID]string{"viewer-id": "viewer"}
	_, err := resolveGroupRef(groups, handles, "~ops")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("resolveGroupRef(~ops) error = %v, want invalid argument", err)
	}
	for item := range strings.SplitSeq(err.Error(), ", ") {
		if strings.HasSuffix(item, "use one of /~ops") || strings.HasPrefix(item, "/~ops") {
			t.Errorf("error %q offers the unparseable anchored ref /~ops", err)
		}
	}
	for hint, wantID := range map[string]ChannelGroupID{"/~viewer/~ops": "top-ops", "/eng/~ops": "eng-ops"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("error %q does not name %q", err, hint)
		}
		if got, err := resolveGroupRef(groups, handles, hint); err != nil || got.ID != wantID {
			t.Errorf("hint %q resolves to %q, %v; want %q", hint, got.ID, err, wantID)
		}
	}
}

// TestResolveGroupRefNoHintOmitsClause: with no ref to offer, the error must not
// promise one.
func TestResolveGroupRefNoHintOmitsClause(t *testing.T) {
	groups := []ChannelGroup{
		{ID: "x-1", Name: "x", NamespaceOwnerID: "ghost-1"},
		{ID: "x-2", Name: "x", NamespaceOwnerID: "ghost-2"},
	}
	_, err := resolveGroupRef(groups, nil, "x")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("resolveGroupRef(x) error = %v, want invalid argument", err)
	}
	if strings.Contains(err.Error(), "use one of") {
		t.Errorf("error %q offers an empty hint list", err)
	}
}
