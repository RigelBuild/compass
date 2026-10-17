package auth

import (
	"context"

	"connectrpc.com/connect/v2"

	compassv1connect "github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// privilege is the access class of a CompassService or CommsService RPC on the
// network door: a sealed sum (//sumtype:decl) so every switch over it — check()
// below — must handle each class exhaustively or gochecksumtype fails the build.
// The sum does NOT police procedure coverage (classifyProcedure switches over a
// string, which gochecksumtype cannot see); classify_exhaustive_test enforces
// that every generated procedure is classified. See classifyProcedure.
//
//sumtype:decl
type privilege interface{ isPrivilege() }

// adminOnly marks an RPC reachable on the network door only by the bootstrap
// admin. These are the privileged CompassService agent-session RPCs (and token
// issuance): they were written for the single-user socket trust boundary and take
// no account argument, so on the shared network door a non-admin account must not
// reach them.
type adminOnly struct{}

func (adminOnly) isPrivilege() {}

// authenticatedOpen marks an RPC reachable on the network door by any
// authenticated account: the connect-time probe and every CommsService method,
// which carry their own per-account authorization in the service body.
type authenticatedOpen struct{}

func (authenticatedOpen) isPrivilege() {}

// classifyProcedure maps a connect procedure path to its network-door access
// class, returning ok=false when the path is not a recognized generated
// procedure. The switch enumerates every generated CompassService and
// CommsService procedure; classify_exhaustive_test ranges the proto service
// descriptors and fails if any generated procedure is missing here, so a newly
// added RPC reddens CI until it is classified. gochecksumtype cannot police this
// coverage — the switch is over a string, not the privilege sum. An unrecognized
// path (ok=false) is treated as adminOnly — fail closed, never admit an unknown
// method as open.
func classifyProcedure(procedure string) (privilege, bool) {
	switch procedure {
	// Privileged CompassService agent-session RPCs + token issuance + account rename +
	// container teardown + fleet config writes: admin only. RemoveAgentWorkspace, PutAgentConfig,
	// DeleteAgentConfig are operator-scoped; SpawnAgent (composite Provision+Start) is
	// strictly more powerful, so admin-gated like them (DL-171).
	case compassv1connect.CompassServiceProvisionAgentWorkspaceProcedure,
		compassv1connect.CompassServiceRemoveAgentWorkspaceProcedure,
		compassv1connect.CompassServiceStartAgentSessionProcedure,
		compassv1connect.CompassServiceSpawnAgentProcedure,
		compassv1connect.CompassServiceStopAgentSessionProcedure,
		compassv1connect.CompassServiceReloadAgentSessionProcedure,
		compassv1connect.CompassServiceGetAgentStatusProcedure,
		compassv1connect.CompassServiceIssueTokenProcedure,
		compassv1connect.CompassServiceRenameAccountProcedure,
		compassv1connect.CompassServiceRevokeTokenProcedure,
		compassv1connect.CompassServicePutAgentConfigProcedure,
		compassv1connect.CompassServicePutModelRegistryProcedure,
		compassv1connect.CompassServiceDeleteModelRegistryProcedure,
		compassv1connect.CompassServiceDeleteAgentConfigProcedure:
		return adminOnly{}, true

	// The connect-time probe, the two event streams, the board catch-up read, and the
	// config info view: open to any authenticated account. SubscribeAgentSession
	// carries its own per-account authorization, so the door admits non-admin members;
	// GetAgentConfigInfo returns names only; WhoAmI reflects the caller's own identity;
	// tour state is keyed to the caller's account; GetRefLinkRules holds no secrets.
	case compassv1connect.CompassServiceGetServerInfoProcedure,
		compassv1connect.CompassServiceWhoAmIProcedure,
		compassv1connect.CompassServiceGetTourStateProcedure,
		compassv1connect.CompassServiceClaimTourStartProcedure,
		compassv1connect.CompassServiceSetTourStateProcedure,
		compassv1connect.CompassServiceSubscribeEventsProcedure,
		compassv1connect.CompassServiceListBoardIssuesProcedure,
		compassv1connect.CompassServiceSearchIssuesProcedure,
		compassv1connect.CompassServiceSkipBatchWindowProcedure,
		compassv1connect.CompassServiceGetAgentConfigInfoProcedure,
		compassv1connect.CompassServiceSubscribeAgentSessionProcedure,
		compassv1connect.CompassServiceGetRefLinkRulesProcedure,
		compassv1connect.CompassServiceGetModelRegistryProcedure,
		// Forge mirror settings: Set checks the tenant's UserRoleAdmin in the
		// handler, which differs from this gate's single bootstrap admin.
		compassv1connect.CompassServiceListForgeMirrorSettingsProcedure,
		compassv1connect.CompassServiceSetForgeMirrorSettingProcedure:
		return authenticatedOpen{}, true

	// Every CommsService method: open to any authenticated account (the service
	// body enforces per-account authorization).
	case compassv1connect.CommsServiceCreateUserProcedure,
		compassv1connect.CommsServiceCreateAgentProcedure,
		compassv1connect.CommsServiceListAccountsProcedure,
		compassv1connect.CommsServiceCreateChannelGroupProcedure,
		compassv1connect.CommsServiceListChannelGroupsProcedure,
		compassv1connect.CommsServiceCreateChannelProcedure,
		compassv1connect.CommsServiceListChannelsProcedure,
		compassv1connect.CommsServiceUpdateChannelMembersProcedure,
		compassv1connect.CommsServiceReparentAgentProcedure,
		compassv1connect.CommsServiceReparentChannelProcedure,
		compassv1connect.CommsServiceOpenAgentWorkspaceProcedure,
		compassv1connect.CommsServiceListMessagesProcedure,
		compassv1connect.CommsServicePostMessageProcedure,
		compassv1connect.CommsServiceRespondToAskProcedure,
		compassv1connect.CommsServiceMirrorToForgeProcedure,
		compassv1connect.CommsServiceSearchMessagesProcedure,
		compassv1connect.CommsServiceListTopicsProcedure,
		compassv1connect.CommsServiceUpdateTopicProcedure,
		compassv1connect.CommsServiceSetChannelPolicyProcedure,
		compassv1connect.CommsServiceGetRosterProcedure,
		compassv1connect.CommsServiceUpdatePinnedBoardProcedure,
		compassv1connect.CommsServiceOpenDMProcedure,
		compassv1connect.CommsServiceApprovePeerProcedure,
		compassv1connect.CommsServiceRevokePeerProcedure,
		compassv1connect.CommsServiceListPeersProcedure,
		compassv1connect.CommsServiceSubscribeCommsProcedure,
		compassv1connect.CommsServiceListAgentBoardsProcedure:
		return authenticatedOpen{}, true

	// The SecretsService write + read RPCs: open to any authenticated account. The
	// secrets record scopes these to the user (writes) and to the user and its agents
	// (ListSecrets) — none is admin-only, so the door admits any account and the
	// service body enforces per-account authorization.
	case compassv1connect.SecretsServiceSetSecretProcedure,
		compassv1connect.SecretsServiceListSecretsProcedure,
		compassv1connect.SecretsServiceDeleteSecretProcedure:
		return authenticatedOpen{}, true
	// AgentRepositoryService handles per-agent authorization in its body: owning
	// users write rows, while agents can list only their own rows.
	case compassv1connect.AgentRepositoryServiceGrantAgentRepositoryProcedure,
		compassv1connect.AgentRepositoryServiceRevokeAgentRepositoryProcedure,
		compassv1connect.AgentRepositoryServiceListAgentRepositoriesProcedure:
		return authenticatedOpen{}, true

	// ListServerSecrets is ADMIN-only: the declared server-secret names are the
	// deployment's own inventory (forge App PEMs, webhook secrets, the gateway
	// master-key family), not something an agent token has any business
	// enumerating.
	case compassv1connect.SecretsServiceListServerSecretsProcedure:
		return adminOnly{}, true

	// UsageService admits authenticated callers; the handler applies account scope.
	case compassv1connect.UsageServiceGetUsageSeriesProcedure:
		return authenticatedOpen{}, true

	default:
		// Fail closed: an unrecognized path (not a generated procedure) is gated
		// to admin and reported unclassified (ok=false). classify_exhaustive_test
		// guarantees no generated procedure reaches here; this guards only a
		// runtime path that is not a generated procedure at all.
		return adminOnly{}, false
	}
}

// AdminGate is the method-level authorization guard over the network door's
// privileged CompassService session RPCs. BearerInterceptor runs first and
// injects the caller; open procedures pass through untouched.
type AdminGate struct {
	admin store.AccountID
}

// NewAdminGate returns a gate parameterized by the bootstrap-admin account.
func NewAdminGate(admin store.AccountID) connect.ServerInterceptor {
	gate := &AdminGate{admin: admin}
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if err := gate.check(ctx, spec.Procedure); err != nil {
				return err
			}
			return next(ctx, spec, stream)
		}
	}
}

// check rejects a non-admin caller on an adminOnly procedure. It runs after
// BearerInterceptor, so a missing caller means the gated request never cleared
// authentication — treated as permission denied (the authn layer already
// rejected it as unauthenticated; this is defense in depth).
func (g *AdminGate) check(ctx context.Context, procedure string) error {
	class, _ := classifyProcedure(procedure)
	switch class.(type) {
	case adminOnly:
		caller, ok := CallerFrom(ctx)
		if !ok || caller != g.admin {
			return connect.NewError(connect.CodePermissionDenied, errAdminOnly.Error()).WithCause(errAdminOnly)
		}
		return nil
	case authenticatedOpen:
		return nil
	}
	// Fail closed: privilege is a sealed two-variant sum, so this fallthrough is
	// unreachable today, but an authorization gate must deny — not admit — any
	// class it does not explicitly recognize.
	return connect.NewError(connect.CodePermissionDenied, errAdminOnly.Error()).WithCause(errAdminOnly)
}
