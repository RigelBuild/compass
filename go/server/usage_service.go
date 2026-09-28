package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
)

// usageService serves token-usage reads. Tenancy comes from the request context;
// the handler narrows the read to the agents the caller may see.
type usageService struct {
	compassv1connect.UnimplementedUsageServiceHandler
	usageStore usage.Store
	store      *store.Store
}

func newUsageService(us usage.Store, st *store.Store) *usageService {
	return &usageService{usageStore: us, store: st}
}

var _ compassv1connect.UsageServiceHandler = (*usageService)(nil)

// GetUsageSeries returns rollup buckets for the caller's scope: admins see the
// tenant, users their own agents, and agents their own subtree.
func (s *usageService) GetUsageSeries(ctx context.Context, req *connect.Request[compassv1.GetUsageSeriesRequest]) (*connect.Response[compassv1.GetUsageSeriesResponse], error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing authenticated caller"))
	}
	start, end := req.Msg.GetStartUnixMs(), req.Msg.GetEndUnixMs()
	if end <= start {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("usage window end must be after start"))
	}
	granularity := usage.Granularity(req.Msg.GetGranularity())
	if err := (&usage.SeriesQuery{Granularity: granularity}).Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	agentID, subtree := req.Msg.GetAgentAccountId(), req.Msg.GetIncludeSubtree()
	if subtree && agentID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("include_subtree requires agent_account_id"))
	}
	filterIDs, admin, err := s.agentFilter(ctx, caller, agentID, subtree)
	if err != nil {
		return nil, err
	}
	if !admin && len(filterIDs) == 0 {
		return connect.NewResponse(&compassv1.GetUsageSeriesResponse{}), nil
	}
	buckets, err := s.usageStore.TokenUsageSeries(ctx, usage.SeriesQuery{
		Granularity: granularity, StartUnixMs: start, EndUnixMs: end,
		AgentAccountIDs: filterIDs, Provider: req.Msg.GetProvider(),
	})
	if err != nil {
		if errors.Is(err, usage.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reading usage series: %w", err))
	}
	out := make([]*compassv1.UsageBucket, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, &compassv1.UsageBucket{
			BucketStartUnixMs: bucket.StartUnixMs,
			InputTokens:       bucket.InputTokens,
			OutputTokens:      bucket.OutputTokens,
			CacheReadTokens:   bucket.CacheReadTokens,
			CacheWriteTokens:  bucket.CacheWriteTokens,
			CostMicroUsd:      bucket.CostMicroUSD,
		})
	}
	return connect.NewResponse(&compassv1.GetUsageSeriesResponse{Buckets: out}), nil
}

// agentFilter resolves the agent IDs a read may cover. A non-admin with no agents
// gets an empty set, which the caller must not pass on as "no filter".
func (s *usageService) agentFilter(ctx context.Context, caller store.AccountID, requested string, subtree bool) ([]string, bool, error) {
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving usage caller: %w", err))
	}
	admin := account.User != nil && account.User.Role == store.UserRoleAdmin
	var scope []store.Account
	switch {
	case account.IsAgent():
		scope, err = s.store.AgentSubtree(ctx, caller)
		if err == nil {
			if account.Agent == nil {
				return nil, false, connect.NewError(connect.CodePermissionDenied, errors.New("caller account cannot read usage"))
			}
			scope, err = s.intersectOwnerScope(ctx, account.Agent.OwnerUserID, scope)
		}
	case account.User != nil && !admin:
		scope, err = s.store.AgentsByOwner(ctx, caller)
	case account.User == nil:
		return nil, false, connect.NewError(connect.CodePermissionDenied, errors.New("caller account cannot read usage"))
	}
	if err != nil {
		return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving usage scope: %w", err))
	}
	ids := accountIDs(scope)
	if requested == "" {
		return ids, admin, nil
	}
	targetID := store.AccountID(requested)
	target, err := s.store.GetAccount(ctx, targetID)
	if errors.Is(err, store.ErrNotFound) {
		if admin {
			return nil, false, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, false, connect.NewError(connect.CodePermissionDenied, errors.New("agent is outside caller usage scope"))
	}
	if err != nil {
		return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving usage agent: %w", err))
	}
	if !target.IsAgent() {
		if admin {
			return nil, false, connect.NewError(connect.CodeNotFound, errors.New("agent not found"))
		}
		return nil, false, connect.NewError(connect.CodePermissionDenied, errors.New("agent is outside caller usage scope"))
	}
	if !admin && !slices.Contains(ids, string(target.ID)) {
		return nil, false, connect.NewError(connect.CodePermissionDenied, errors.New("agent is outside caller usage scope"))
	}
	if !subtree {
		return []string{string(targetID)}, admin, nil
	}
	descendants, err := s.store.AgentSubtree(ctx, targetID)
	if err != nil {
		return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving requested usage subtree: %w", err))
	}
	if !admin {
		owner := caller
		if account.Agent != nil {
			owner = account.Agent.OwnerUserID
		}
		descendants, err = s.intersectOwnerScope(ctx, owner, descendants)
		if err != nil {
			return nil, false, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller usage owner scope: %w", err))
		}
	}
	return accountIDs(descendants), admin, nil
}

func (s *usageService) intersectOwnerScope(ctx context.Context, owner store.AccountID, subtree []store.Account) ([]store.Account, error) {
	owned, err := s.store.AgentsByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	ownedIDs := make(map[store.AccountID]struct{}, len(owned))
	for _, agent := range owned {
		ownedIDs[agent.ID] = struct{}{}
	}
	clipped := make([]store.Account, 0, len(subtree))
	for _, agent := range subtree {
		if _, ok := ownedIDs[agent.ID]; ok {
			clipped = append(clipped, agent)
		}
	}
	return clipped, nil
}

func accountIDs(accounts []store.Account) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, string(account.ID))
	}
	return ids
}
