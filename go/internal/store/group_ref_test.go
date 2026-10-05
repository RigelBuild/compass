package store

import (
	"errors"
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
	}

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
		{name: "leading slash", ref: "/a", wantErr: ErrInvalidArgument},
		{name: "trailing slash", ref: "a/", wantErr: ErrInvalidArgument},
		{name: "doubled slash", ref: "a//b", wantErr: ErrInvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveGroupRef(groups, tt.ref)
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
