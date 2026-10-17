//go:build unix

package server

import (
	"context"
	"log/slog"
	"slices"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store"
)

type refLinkTeamLister interface {
	ListEnabledForgeRepoSubscriptions(ctx context.Context, provider store.ForgeProvider, host string) ([]store.ForgeRepoSubscription, error)
}

type refLinkURLKeyReader interface {
	OrganizationURLKey(ctx context.Context) (string, error)
}

type refLinkRuleSource struct {
	githubHosts []string
	teams       refLinkTeamLister
	linear      refLinkURLKeyReader
	log         *slog.Logger
}

func newRefLinkRuleSource(
	hosts []string,
	teams refLinkTeamLister,
	linear refLinkURLKeyReader,
	log *slog.Logger,
) *refLinkRuleSource {
	if log == nil {
		log = slog.Default()
	}
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	return &refLinkRuleSource{githubHosts: hosts, teams: teams, linear: linear, log: log}
}

// buildRefLinkRuleSource links GitHub refs whenever the primary App reads the
// forge, and reuses the Linear notify lane client for the urlKey read.
func buildRefLinkRuleSource(cfg ServeConfig, githubConfigured bool, st refLinkTeamLister, linearNotify *forgeNotifyLane, log *slog.Logger) *refLinkRuleSource {
	var githubHosts []string
	if githubConfigured {
		githubHosts = []string{cfg.Forge.resolved().Host}
	}
	var urlKey refLinkURLKeyReader
	if linearNotify != nil {
		if reader, ok := linearNotify.reader.(*forge.Linear); ok {
			urlKey = reader
		}
	}
	return newRefLinkRuleSource(githubHosts, st, urlKey, log)
}

func (s *refLinkRuleSource) rules(ctx context.Context) []*compassv1.RefLinkRule {
	rules := make([]*compassv1.RefLinkRule, 0, len(s.githubHosts))
	for _, host := range s.githubHosts {
		if host == "" {
			continue
		}
		rules = append(rules, &compassv1.RefLinkRule{
			Kind:        "github",
			Prefix:      host,
			UrlTemplate: "https://" + host + "/{owner}/{repo}/issues/{number}",
		})
	}

	if s.linear == nil || s.teams == nil {
		return rules
	}
	teams, err := s.teams.ListEnabledForgeRepoSubscriptions(ctx, store.ForgeProviderLinear, forge.LinearHost)
	if err != nil {
		s.log.WarnContext(ctx, "ref-link rules: listing enabled Linear teams failed", "err", err)
		return rules
	}
	if len(teams) == 0 {
		return rules
	}
	urlKey, err := s.linear.OrganizationURLKey(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "ref-link rules: reading Linear organization URL key failed", "err", err)
		return rules
	}

	keys := make([]string, 0, len(teams))
	seenKeys := make(map[string]struct{}, len(teams))
	for _, team := range teams {
		if team.Repo == "" {
			continue
		}
		if _, exists := seenKeys[team.Repo]; exists {
			continue
		}
		seenKeys[team.Repo] = struct{}{}
		keys = append(keys, team.Repo)
	}
	slices.Sort(keys)
	for _, key := range keys {
		rules = append(rules, &compassv1.RefLinkRule{
			Kind:        "linear",
			Prefix:      key,
			UrlTemplate: "https://linear.app/" + urlKey + "/issue/{key}-{number}",
		})
	}
	return rules
}

// GetRefLinkRules returns link templates for the forges configured on this server.
func (s *service) GetRefLinkRules(
	ctx context.Context,
	_ *compassv1.GetRefLinkRulesRequest,
) (*compassv1.GetRefLinkRulesResponse, error) {
	rules := []*compassv1.RefLinkRule{}
	if s.refLinkRules != nil {
		rules = s.refLinkRules.rules(ctx)
	}
	return &compassv1.GetRefLinkRulesResponse{Rules: rules}, nil
}
