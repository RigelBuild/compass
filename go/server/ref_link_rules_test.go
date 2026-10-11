//go:build unix

package server

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store"
)

type refLinkTeamsFake struct {
	teams []store.ForgeRepoSubscription
	err   error
	calls int
}

func (f *refLinkTeamsFake) ListEnabledForgeRepoSubscriptions(_ context.Context, provider store.ForgeProvider, host string) ([]store.ForgeRepoSubscription, error) {
	f.calls++
	if provider != store.ForgeProviderLinear || host != forge.LinearHost {
		return nil, errors.New("unexpected team lookup coordinate")
	}
	return f.teams, f.err
}

type refLinkURLKeyFake struct {
	key   string
	err   error
	calls int
}

func (f *refLinkURLKeyFake) OrganizationURLKey(context.Context) (string, error) {
	f.calls++
	return f.key, f.err
}

func TestGetRefLinkRules(t *testing.T) {
	for _, tc := range []struct {
		name      string
		source    *refLinkRuleSource
		want      []*compassv1.RefLinkRule
		wantTeams int
		wantKey   int
	}{
		{
			name: "hosts and sorted enabled teams",
			source: newRefLinkRuleSource(
				[]string{"ghe.example", "github.com"},
				&refLinkTeamsFake{teams: []store.ForgeRepoSubscription{{Repo: "ZED", Enabled: true}, {Repo: "RIG", Enabled: true}}},
				&refLinkURLKeyFake{key: "workspace"}, nil),
			want: []*compassv1.RefLinkRule{
				{Kind: "github", Prefix: "ghe.example", UrlTemplate: "https://ghe.example/{owner}/{repo}/issues/{number}"},
				{Kind: "github", Prefix: "github.com", UrlTemplate: "https://github.com/{owner}/{repo}/issues/{number}"},
				{Kind: "linear", Prefix: "RIG", UrlTemplate: "https://linear.app/workspace/issue/{key}-{number}"},
				{Kind: "linear", Prefix: "ZED", UrlTemplate: "https://linear.app/workspace/issue/{key}-{number}"},
			},
			wantTeams: 1,
			wantKey:   1,
		},
		{
			name: "url key failure keeps GitHub rules",
			source: newRefLinkRuleSource(
				[]string{"github.com"},
				&refLinkTeamsFake{teams: []store.ForgeRepoSubscription{{Repo: "RIG", Enabled: true}}},
				&refLinkURLKeyFake{err: errors.New("offline")}, nil),
			want: []*compassv1.RefLinkRule{
				{Kind: "github", Prefix: "github.com", UrlTemplate: "https://github.com/{owner}/{repo}/issues/{number}"},
			},
			wantTeams: 1,
			wantKey:   1,
		},
		{
			name: "team listing failure keeps GitHub rules",
			source: newRefLinkRuleSource(
				[]string{"github.com"},
				&refLinkTeamsFake{err: errors.New("store offline")},
				&refLinkURLKeyFake{key: "workspace"}, nil),
			want: []*compassv1.RefLinkRule{
				{Kind: "github", Prefix: "github.com", UrlTemplate: "https://github.com/{owner}/{repo}/issues/{number}"},
			},
			wantTeams: 1,
			wantKey:   0,
		},
		{
			name:   "nil source returns empty list",
			source: nil,
			want:   []*compassv1.RefLinkRule{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &service{refLinkRules: tc.source}
			resp, err := svc.GetRefLinkRules(t.Context(), connect.NewRequest(&compassv1.GetRefLinkRulesRequest{}))
			if err != nil {
				t.Fatalf("GetRefLinkRules: %v", err)
			}
			want := &compassv1.GetRefLinkRulesResponse{Rules: tc.want}
			if !proto.Equal(resp.Msg, want) {
				t.Fatalf("rules = %v, want %v", resp.Msg.GetRules(), want.GetRules())
			}
			if tc.source != nil {
				teams := tc.source.teams.(*refLinkTeamsFake)
				key := tc.source.linear.(*refLinkURLKeyFake)
				if teams.calls != tc.wantTeams || key.calls != tc.wantKey {
					t.Errorf("calls teams/key = %d/%d, want %d/%d", teams.calls, key.calls, tc.wantTeams, tc.wantKey)
				}
			}
		})
	}
}

func TestBuildRefLinkRuleSourceUsesConfiguredGitHubHost(t *testing.T) {
	cfg := ServeConfig{Forge: ForgeConfig{Host: "ghe.example"}}
	for _, tc := range []struct {
		name       string
		configured bool
		want       []string
	}{
		{"primary App configured", true, []string{"ghe.example"}},
		{"no GitHub App", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := buildRefLinkRuleSource(cfg, tc.configured, nil, nil, nil)
			if !slices.Equal(src.githubHosts, tc.want) {
				t.Fatalf("githubHosts = %v, want %v", src.githubHosts, tc.want)
			}
		})
	}
}
