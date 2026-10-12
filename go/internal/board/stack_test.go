//go:build unix

package board

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

func TestStackOf(t *testing.T) {
	tests := []struct {
		name string
		prs  []*compassv1.PullRequest
		want map[string]Stack
	}{
		{
			name: "three pull request chain",
			prs: []*compassv1.PullRequest{
				stackTestPR(1, "main", "one"),
				stackTestPR(2, "one", "two"),
				stackTestPR(3, "two", "three"),
			},
			want: map[string]Stack{
				"owner/repo#1": {Position: 1, Tip: "owner/repo#3"},
				"owner/repo#2": {Position: 2, Tip: "owner/repo#3"},
				"owner/repo#3": {Position: 3, Tip: "owner/repo#3"},
			},
		},
		{
			name: "fork has two tips",
			prs: []*compassv1.PullRequest{
				stackTestPR(1, "main", "one"),
				stackTestPR(2, "one", "two"),
				stackTestPR(3, "one", "three"),
			},
			want: map[string]Stack{
				"owner/repo#1": {Position: 1, Tip: "owner/repo#2"},
				"owner/repo#2": {Position: 2, Tip: "owner/repo#2"},
				"owner/repo#3": {Position: 2, Tip: "owner/repo#3"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StackOf(tt.prs)
			if len(got) != len(tt.want) {
				t.Fatalf("stack size = %d, want %d: %v", len(got), len(tt.want), got)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("stack[%q] = %+v, want %+v", key, got[key], want)
				}
			}
		})
	}
}

func stackTestPR(number uint32, base, head string) *compassv1.PullRequest {
	return &compassv1.PullRequest{
		Forge:      &compassv1.ForgeRef{Host: "github.com"},
		Repo:       "owner/repo",
		Number:     number,
		ForgeState: "open",
		BaseRef:    base,
		HeadRef:    head,
	}
}
