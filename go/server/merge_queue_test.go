//go:build unix

package server

import (
	"context"
	"strings"
	"testing"
)

func TestValidateMergeQueueConfigs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		configs []MergeQueueConfig
		wantErr string
	}{
		{name: "valid", configs: []MergeQueueConfig{{Repo: "owner/repo", Provider: "trunk", Branch: "main"}}},
		{name: "missing branch", configs: []MergeQueueConfig{{Repo: "owner/repo", Provider: "trunk"}}, wantErr: "required"},
		{name: "unsupported provider", configs: []MergeQueueConfig{{Repo: "owner/repo", Provider: "github", Branch: "main"}}, wantErr: "unsupported"},
		{name: "duplicate repo", configs: []MergeQueueConfig{{Repo: "owner/repo", Provider: "trunk", Branch: "main"}, {Repo: "owner/repo", Provider: "trunk", Branch: "release"}}, wantErr: "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMergeQueueConfigs(tc.configs)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateMergeQueueConfigs: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateMergeQueueConfigs = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPrepareMergeQueueSweeperDoesNotBuildWhenNoReposConfigured(t *testing.T) {
	runner, err := prepareMergeQueueSweeper(context.Background(), ServeConfig{}, nil, nil, nil)
	if err != nil || runner.sweeper != nil {
		t.Fatalf("prepareMergeQueueSweeper = %v, %v; want empty runner, nil", runner, err)
	}
}
