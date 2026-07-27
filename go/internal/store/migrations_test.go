package store

// checkContiguous is the build-time half of the refuse-to-serve-on-a-gap
// contract (store.go): the embedded migration set must be a gapless 1..N
// sequence, so a missing version can never pass the max-version serve check
// while silently deploying an incomplete schema. Unit-testable without Postgres
// because it operates on a plain, version-sorted []migration.

import (
	"errors"
	"testing"
)

func TestCheckContiguous(t *testing.T) {
	tests := []struct {
		name    string
		migs    []migration
		wantErr bool
	}{
		{
			name:    "empty set is contiguous",
			migs:    nil,
			wantErr: false,
		},
		{
			name:    "single v1 is contiguous",
			migs:    []migration{{version: 1, name: "0001_init.sql"}},
			wantErr: false,
		},
		{
			name: "1..3 gapless is contiguous",
			migs: []migration{
				{version: 1, name: "0001_a.sql"},
				{version: 2, name: "0002_b.sql"},
				{version: 3, name: "0003_c.sql"},
			},
			wantErr: false,
		},
		{
			name: "gap 1,3 (missing 2) is rejected",
			migs: []migration{
				{version: 1, name: "0001_a.sql"},
				{version: 3, name: "0003_c.sql"},
			},
			wantErr: true,
		},
		{
			name:    "not starting at 1 is rejected",
			migs:    []migration{{version: 2, name: "0002_b.sql"}},
			wantErr: true,
		},
		{
			name: "gap after a contiguous prefix is rejected at the first gap",
			migs: []migration{
				{version: 1, name: "0001_a.sql"},
				{version: 2, name: "0002_b.sql"},
				{version: 4, name: "0004_d.sql"},
			},
			wantErr: true,
		},
		{
			name: "duplicate version breaks contiguity",
			migs: []migration{
				{version: 1, name: "0001_a.sql"},
				{version: 1, name: "0001_dup.sql"},
				{version: 2, name: "0002_b.sql"},
			},
			wantErr: true,
		},
		{
			name:    "version 0 is rejected (sequence is 1-based)",
			migs:    []migration{{version: 0, name: "0000_zero.sql"}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkContiguous(tc.migs)
			if tc.wantErr {
				if !errors.Is(err, ErrSchemaVersion) {
					t.Fatalf("checkContiguous(%v) err = %v, want errors.Is(_, ErrSchemaVersion)", tc.migs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkContiguous(%v) err = %v, want nil", tc.migs, err)
			}
		})
	}
}
