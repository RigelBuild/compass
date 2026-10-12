//go:build unix

package board

import (
	"math"
	"slices"
	"sort"
	"strings"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

func stackPosition(length int) uint32 {
	if length > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(length) //nolint:gosec // length was checked against the wire field's maximum.
}

// Stack locates a pull request in its stack and identifies that stack's tip.
type Stack struct {
	Position uint32
	Tip      string
}

// StackOf derives positions and tips from the supplied open authored pull requests.
func StackOf(open []*compassv1.PullRequest) map[string]Stack {
	byRepo := make(map[string][]*compassv1.PullRequest)
	for _, pr := range open {
		if pr == nil || pr.GetForgeState() != "open" || pr.GetRepo() == "" || pr.GetNumber() == 0 {
			continue
		}
		repo := strings.ToLower(pr.GetRepo())
		byRepo[repo] = append(byRepo[repo], pr)
	}

	out := make(map[string]Stack)
	for repo, prs := range byRepo {
		byHead := make(map[string]*compassv1.PullRequest, len(prs))
		children := make(map[string][]*compassv1.PullRequest, len(prs))
		for _, pr := range prs {
			byHead[pr.GetHeadRef()] = pr
		}
		for _, pr := range prs {
			if parent := byHead[pr.GetBaseRef()]; parent != nil && parent != pr {
				parentKey := boardRefKey(repo, uint64(parent.GetNumber()))
				children[parentKey] = append(children[parentKey], pr)
			}
		}

		var tips []*compassv1.PullRequest
		for _, pr := range prs {
			if len(children[boardRefKey(repo, uint64(pr.GetNumber()))]) == 0 {
				tips = append(tips, pr)
			}
		}
		sort.Slice(tips, func(i, j int) bool {
			return boardRefKey(repo, uint64(tips[i].GetNumber())) < boardRefKey(repo, uint64(tips[j].GetNumber()))
		})
		// Shared ancestors use the lexically first tip's ref.
		for _, tip := range tips {
			var chain []*compassv1.PullRequest
			current := tip
			seen := make(map[uint32]struct{}, len(prs))
			for current != nil {
				if _, ok := seen[current.GetNumber()]; ok {
					break
				}
				seen[current.GetNumber()] = struct{}{}
				chain = append(chain, current)
				current = byHead[current.GetBaseRef()]
			}
			for i, pr := range slices.Backward(chain) {
				key := boardRefKey(repo, uint64(pr.GetNumber()))
				if _, assigned := out[key]; !assigned {
					position := stackPosition(len(chain) - i)
					out[key] = Stack{Position: position, Tip: boardRefKey(repo, uint64(tip.GetNumber()))}
				}
			}
		}
	}
	return out
}
