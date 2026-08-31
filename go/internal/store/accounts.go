package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// accountFromRow reconstructs a domain Account from the shared ten-column account
// projection (id, handle, display_name, the user role, the five agent columns,
// and the system account id) that every account read selects. It replaces the
// former hand-written scanAccount helper: sqlc emits the same columns as typed
// nullables, so the subtype discriminator is identical — a non-null user role is
// a user account, else a non-null owner id is an agent account, else a non-null
// system id is the system account. The system column is scanned last, matching
// the projection order every account query shares. Callers whose generated row
// carries the agent columns as non-nullable strings (the tree reads, INNER-joined
// to agent_accounts) wrap them into a valid pgtype.Text before calling.
func accountFromRow(
	id, handle, displayName string,
	userRole pgtype.Int2,
	ownerUserID, homeChannelID, persona, agentRole, parentAgentID, systemAccountID pgtype.Text,
) Account {
	acc := Account{ID: AccountID(id), Handle: handle, DisplayName: displayName}
	switch {
	case userRole.Valid:
		acc.User = &UserAccount{Role: UserRole(userRole.Int16)}
	case ownerUserID.Valid:
		agent := &AgentAccount{OwnerUserID: AccountID(ownerUserID.String)}
		if homeChannelID.Valid {
			agent.HomeChannelID = ChannelID(homeChannelID.String)
		}
		if persona.Valid {
			agent.Persona = persona.String
		}
		if agentRole.Valid {
			agent.Role = agentRole.String
		}
		if parentAgentID.Valid {
			agent.ParentAgentID = AccountID(parentAgentID.String)
		}
		acc.Agent = agent
	case systemAccountID.Valid:
		acc.System = &SystemAccount{}
	}
	return acc
}

// insertAccountHandle records accountID's row in the account_handles resolution
// index (RIG-2751 handle cutover). ownerUserID is empty for a user/system handle
// (stored NULL, globally unique) and the owning user's id for an agent handle
// (unique only within that owner). A duplicate handle in the applicable
// namespace is ErrConflict, mirroring the former accounts.handle unique. Runs on
// the caller's tx-scoped queries so the handle row commits atomically with the
// account insert.
func insertAccountHandle(ctx context.Context, q *db.Queries, accountID, handle string, ownerUserID AccountID) error {
	if err := q.InsertAccountHandle(ctx, db.InsertAccountHandleParams{
		AccountID: accountID,
		Handle:    handle,
		Column3:   string(ownerUserID),
	}); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: handle %q already taken", ErrConflict, handle)
		}
		return fmt.Errorf("store: insert account_handle: %w", err)
	}
	return nil
}

// CreateUser inserts a human account (a regular member; admin elevation is a
// separate path, comms.proto:39-42) and returns it with its server-assigned id.
// A duplicate handle is ErrConflict.
func (s *Store) CreateUser(ctx context.Context, u NewUser) (Account, error) {
	if u.Handle == "" {
		return Account{}, fmt.Errorf("%w: user handle is required", ErrInvalidArgument)
	}
	if err := validateHandle(u.Handle); err != nil {
		return Account{}, err
	}

	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin create user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.InsertAccount(ctx, db.InsertAccountParams{
		ID:          id,
		Handle:      u.Handle,
		DisplayName: u.DisplayName,
		TenantID:    string(s.resolveTenant(ctx)),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if err := qtx.InsertUserAccount(ctx, db.InsertUserAccountParams{
		AccountID: id,
		Role:      int16(UserRoleMember),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert user_account: %w", err)
	}
	// The handle uniqueness now lives on account_handles (a user handle is
	// globally unique, owner_user_id NULL), not accounts.handle: a duplicate
	// surfaces here as ErrConflict.
	if err := insertAccountHandle(ctx, qtx, id, u.Handle, ""); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit create user: %w", err)
	}

	return Account{
		ID:          AccountID(id),
		Handle:      u.Handle,
		DisplayName: u.DisplayName,
		User:        &UserAccount{Role: UserRoleMember},
	}, nil
}

// BootstrapAdmin ensures the first admin user exists and returns it, idempotently.
// It is the local-socket door's startup attribution target: every RPC on the
// shipped socket path is authorized as this account until the T3 interceptor
// sets a real caller identity. Unlike CreateUser (always a member), it seeds an
// admin (UserRoleAdmin) — the one account created without an authorizing actor,
// safe only because it runs at server start before any request is served.
//
// Idempotent by handle: on a restart the admin already exists, so a duplicate
// insert is not an error — the existing account is fetched and returned. A
// handle that exists as a non-admin is a misconfiguration and returns
// ErrConflict rather than silently elevating it.
func (s *Store) BootstrapAdmin(ctx context.Context, u NewUser) (Account, error) {
	if u.Handle == "" {
		return Account{}, fmt.Errorf("%w: admin handle is required", ErrInvalidArgument)
	}
	if err := validateHandle(u.Handle); err != nil {
		return Account{}, err
	}

	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin bootstrap admin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.InsertAccount(ctx, db.InsertAccountParams{
		ID:          id,
		Handle:      u.Handle,
		DisplayName: u.DisplayName,
		TenantID:    string(s.resolveTenant(ctx)),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if err := qtx.InsertUserAccount(ctx, db.InsertUserAccountParams{
		AccountID: id,
		Role:      int16(UserRoleAdmin),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert user_account: %w", err)
	}
	// Handle uniqueness lives on account_handles now; the restart's duplicate
	// surfaces here (ErrConflict) rather than on the accounts insert. Already
	// bootstrapped (restart): fetch and return the existing admin.
	if err := insertAccountHandle(ctx, qtx, id, u.Handle, ""); err != nil {
		if errors.Is(err, ErrConflict) {
			return s.adminByHandle(ctx, u.Handle)
		}
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit bootstrap admin: %w", err)
	}

	return Account{
		ID:          AccountID(id),
		Handle:      u.Handle,
		DisplayName: u.DisplayName,
		User:        &UserAccount{Role: UserRoleAdmin},
	}, nil
}

// adminByHandle fetches an existing account by handle and asserts it is an
// admin, backing BootstrapAdmin's idempotent restart path. A handle that exists
// as a non-admin (or as an agent) is a misconfiguration: ErrConflict, never a
// silent elevation.
func (s *Store) adminByHandle(ctx context.Context, handle string) (Account, error) {
	row, err := s.q.GetAccountByGlobalHandle(ctx, handle)
	if err != nil {
		return Account{}, err
	}
	acc := accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID)
	if acc.User == nil || acc.User.Role != UserRoleAdmin {
		return Account{}, fmt.Errorf("%w: handle %q exists but is not an admin", ErrConflict, handle)
	}
	return acc, nil
}

// EnsureSystemAccount ensures the reserved system sender (@compass) exists and
// returns it, idempotently — the seed for the platform's first-turn delivery.
// Mirrors BootstrapAdmin's unique-violation-means-fetch shape: on first boot it
// mints one accounts row (handle SystemAccountHandle, display name "Compass")
// with a system_accounts subtype row — NOT a user or agent row; on every later
// boot the insert hits the unique handle and the existing row is fetched and
// returned. Its own insert is deliberately NOT routed through validateHandle:
// this is the one path that mints the reserved handle. A pre-existing @compass
// row of the wrong shape (a user or agent row from a pre-guard database) is
// ErrConflict and fails startup — never silent adoption, mirroring
// adminByHandle's posture.
func (s *Store) EnsureSystemAccount(ctx context.Context) (Account, error) {
	return s.ensureSystemSubtypeAccount(ctx, SystemAccountHandle, systemAccountDisplayName)
}

// EnsureLinearBridgeAccount ensures the reserved Linear bridge sender (@linear)
// exists and returns it, idempotently — the author of Part 2 bridge posts. It
// mints a SECOND system-subtype account beside @compass with the exact same
// find-or-create shape (see ensureSystemSubtypeAccount): one accounts row plus a
// system_accounts row on first boot, the existing row fetched on every later
// boot. A pre-existing @linear row of the wrong shape is ErrConflict.
func (s *Store) EnsureLinearBridgeAccount(ctx context.Context) (Account, error) {
	return s.ensureSystemSubtypeAccount(ctx, LinearBridgeAccountHandle, linearBridgeDisplayName)
}

// ensureSystemSubtypeAccount is the shared find-or-create for a reserved
// system-subtype account (handle + display name). Mirrors BootstrapAdmin's
// unique-violation-means-fetch shape: on first boot it mints one accounts row
// with a system_accounts subtype row — NOT a user or agent row; on every later
// boot the insert hits the unique handle and the existing row is fetched and
// returned. Its own insert is deliberately NOT routed through validateHandle:
// this is the one path that mints a reserved handle. A pre-existing row of the
// wrong shape (a user or agent row from a pre-guard database) is ErrConflict and
// fails startup — never silent adoption, mirroring adminByHandle's posture.
func (s *Store) ensureSystemSubtypeAccount(ctx context.Context, handle, displayName string) (Account, error) {
	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin ensure system account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit; safe on every non-commit path.
	qtx := s.q.WithTx(tx)

	if err := qtx.InsertAccount(ctx, db.InsertAccountParams{
		ID:          id,
		Handle:      handle,
		DisplayName: displayName,
		TenantID:    string(s.resolveTenant(ctx)),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if err := qtx.InsertSystemAccount(ctx, id); err != nil {
		return Account{}, fmt.Errorf("store: insert system_account: %w", err)
	}
	// A system handle is globally unique (owner_user_id NULL) on account_handles;
	// the restart's duplicate surfaces here. Already seeded (restart): fetch and
	// return the existing system account.
	if err := insertAccountHandle(ctx, qtx, id, handle, ""); err != nil {
		if errors.Is(err, ErrConflict) {
			return s.systemByHandle(ctx, handle)
		}
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit ensure system account: %w", err)
	}

	return Account{
		ID:          AccountID(id),
		Handle:      handle,
		DisplayName: displayName,
		System:      &SystemAccount{},
	}, nil
}

// systemByHandle fetches an existing account by handle and asserts it is the
// system subtype, backing EnsureSystemAccount's idempotent restart path. A
// handle that exists as a non-system account (a user or agent row from a
// pre-guard database) is a misconfiguration: ErrConflict, never a silent
// adoption of a foreign row as the privileged system sender.
func (s *Store) systemByHandle(ctx context.Context, handle string) (Account, error) {
	row, err := s.q.GetAccountByGlobalHandle(ctx, handle)
	if err != nil {
		return Account{}, fmt.Errorf("store: resolve system account by handle: %w", err)
	}
	acc := accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID)
	if acc.System == nil {
		return Account{}, fmt.Errorf("%w: handle %q exists but is not the system account", ErrConflict, handle)
	}
	return acc, nil
}

// CreateAgent inserts an agent account owned by ownerUserID and, in the same
// transaction, mints the agent's home channel (RT-2) — a channel named for the
// agent, owner-scoped, with the owning user and the agent as members and the
// agent always-subscribed — then records it as home_channel_id. Minting them
// together is what makes "the agent's own channel" exist from creation for
// turn-end delivery and the observation-pane ACL. A duplicate handle is
// ErrConflict; an unknown owner is ErrInvalidArgument.
func (s *Store) CreateAgent(ctx context.Context, ownerUserID AccountID, a NewAgent) (Account, error) {
	if a.Handle == "" {
		return Account{}, fmt.Errorf("%w: agent handle is required", ErrInvalidArgument)
	}
	if err := validateHandle(a.Handle); err != nil {
		return Account{}, err
	}
	if ownerUserID == "" {
		return Account{}, fmt.Errorf("%w: owner user id is required", ErrInvalidArgument)
	}

	accountID := newID()
	channelID := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin create agent: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := qtx.InsertAccount(ctx, db.InsertAccountParams{
		ID:          accountID,
		Handle:      a.Handle,
		DisplayName: a.DisplayName,
		TenantID:    string(s.resolveTenant(ctx)),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if err := qtx.InsertAgentAccount(ctx, db.InsertAgentAccountParams{
		AccountID:     accountID,
		OwnerUserID:   string(ownerUserID),
		HomeChannelID: pgtype.Text{String: channelID, Valid: true},
		Persona:       a.Persona,
		Role:          a.Role,
		Column6:       string(a.ParentAgentID),
	}); err != nil {
		// Both FKs on agent_accounts land here: parent_agent_id (a supplied
		// parent that does not resolve to an agent) and owner_user_id (an
		// unknown owner). ConstraintName tells them apart — the parent FK is a
		// missing referent (ErrNotFound), an unknown owner a caller error
		// (ErrInvalidArgument), not a store fault.
		if pgErrIs(err, pgForeignKeyViolation) {
			if pgConstraintName(err) == "agent_accounts_parent_agent_id_fkey" {
				return Account{}, fmt.Errorf("%w: parent agent %q", ErrNotFound, a.ParentAgentID)
			}
			return Account{}, fmt.Errorf("%w: unknown owner user %q", ErrInvalidArgument, ownerUserID)
		}
		return Account{}, fmt.Errorf("store: insert agent_account: %w", err)
	}

	// Record the agent handle in the resolution index, scoped to its owner
	// (owner_user_id = ownerUserID), so it is unique only within that owner's
	// namespace. Handle uniqueness moved off accounts.handle: a duplicate agent
	// handle under the same owner surfaces here as ErrConflict.
	if err := insertAccountHandle(ctx, qtx, accountID, a.Handle, ownerUserID); err != nil {
		return Account{}, err
	}

	// INVARIANT: every write of agent_accounts.parent_agent_id must invoke the
	// registered coordination hook. The INSERT above just wrote it; invoke the
	// hook on THIS tx for the new agent's PARENT (the manager that gains this
	// report), so the coordination-channel reconcile commits atomically with the
	// tree edge (RIG-1722 T5, design.md:550-551). Skipped when parent is empty: a
	// root agent has no manager, so there is no coordination channel to reconcile.
	if a.ParentAgentID != "" {
		if err := s.invokeCoordinationHook(ctx, tx, a.ParentAgentID); err != nil {
			return Account{}, err
		}
	}

	// Mint the home channel: named for the agent, ungrouped (owner-scoped), with
	// the owner and the agent as members and the agent always-subscribed.
	if err := qtx.InsertHomeChannel(ctx, db.InsertHomeChannelParams{
		ID:   channelID,
		Name: a.Handle,
		Kind: int16(ChannelKindChannel),
	}); err != nil {
		return Account{}, fmt.Errorf("store: insert home channel: %w", err)
	}
	if err := qtx.SeedHomeChannelMembers(ctx, db.SeedHomeChannelMembersParams{
		ChannelID:   channelID,
		AccountID:   string(ownerUserID),
		AccountID_2: accountID,
	}); err != nil {
		return Account{}, fmt.Errorf("store: seed home channel members: %w", err)
	}
	if err := seedDeliveryCursor(ctx, tx, AccountID(accountID), ChannelID(channelID)); err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit create agent: %w", err)
	}

	return Account{
		ID:          AccountID(accountID),
		Handle:      a.Handle,
		DisplayName: a.DisplayName,
		Agent: &AgentAccount{
			OwnerUserID:   ownerUserID,
			HomeChannelID: ChannelID(channelID),
			Persona:       a.Persona,
			Role:          a.Role,
			ParentAgentID: a.ParentAgentID,
		},
	}, nil
}

// CountRootAgents returns how many root agents (parent_agent_id IS NULL) the
// owner holds. It backs the first-launch seed's idempotency gate: zero means an
// empty tree to seed, non-zero means a root already exists and the seed is a
// no-op. Scoped to ownerUserID so it counts only this owner's tree.
func (s *Store) CountRootAgents(ctx context.Context, ownerUserID AccountID) (int, error) {
	if ownerUserID == "" {
		return 0, fmt.Errorf("%w: owner user id is required", ErrInvalidArgument)
	}
	n, err := s.q.CountRootAgents(ctx, string(ownerUserID))
	if err != nil {
		return 0, fmt.Errorf("store: count root agents: %w", err)
	}
	return int(n), nil
}

// EnsureChannelMember idempotently adds account to channelID as an UNSUBSCRIBED
// member (subscribed=FALSE), mirroring the coordination-hook insert
// (coordination.go:273-282) but as a single-statement pool write, not in a tx:
// there is no delivery cursor to seed alongside it. It backs the first-launch
// Setup post (T4), which must make @compass a member of the supervisor's home
// channel BEFORE posting — PostMessage D9-gates the post on membership
// (messages.go, requireChannelMember). It is deliberately NOT UpdateChannelMembers:
// that path D9-gates on the ACTOR already being a member (a chicken-and-egg for
// this seed insert, which runs server-internal with no naturally-authorized
// actor) and fires membership events + owner-transitive add logic the seed does
// not want. NO delivery cursor is seeded: @compass is a system account with no
// agent_accounts row, and cursors are agent-only (delivery_cursors.go); it posts,
// never receives. ON CONFLICT DO NOTHING makes a re-fire a no-op. An unknown
// channel or account is ErrInvalidArgument (the FK violation), never a store fault.
func (s *Store) EnsureChannelMember(ctx context.Context, channelID ChannelID, accountID AccountID) error {
	if err := s.q.EnsureChannelMember(ctx, db.EnsureChannelMemberParams{
		ChannelID: string(channelID),
		AccountID: string(accountID),
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: unknown channel %q or account %q", ErrInvalidArgument, channelID, accountID)
		}
		return fmt.Errorf("store: ensure channel member: %w", err)
	}
	return nil
}

// GetAccount returns one account by id, or ErrNotFound if it does not exist.
// GetAccount is an id-addressed fetch used internally by other store methods
// and the auth layer; caller-facing visibility scoping is applied by
// ListAccounts and the per-container reads, not here.
func (s *Store) GetAccount(ctx context.Context, id AccountID) (Account, error) {
	row, err := s.q.GetAccount(ctx, string(id))
	if err != nil {
		if noRows(err) {
			return Account{}, fmt.Errorf("%w: account %q", ErrNotFound, id)
		}
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	return accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID), nil
}

// AgentOwner returns the owning user of an agent account. It is a thin
// projection over agent_accounts.owner_user_id, resolving an agent's owner for
// the despawn authority check (only the owner may despawn) and spawn inheritance.
// ErrNotFound covers BOTH an unknown id AND an id that names a non-agent account:
// querying agent_accounts directly means a user id simply misses the row, so
// there is no separate existence probe — the not-found/forbidden merge (D9) the
// store's sentinel semantics require.
func (s *Store) AgentOwner(ctx context.Context, agentAccountID AccountID) (AccountID, error) {
	if agentAccountID == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	ownerUserID, err := s.q.GetAgentOwner(ctx, string(agentAccountID))
	if err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: agent %q", ErrNotFound, agentAccountID)
		}
		return "", fmt.Errorf("store: resolve agent owner: %w", err)
	}
	return AccountID(ownerUserID), nil
}

// ResolveOwner resolves a caller to the user account it acts under. It mirrors
// the caller-owner resolution ReparentAgent applies inline (clause 0): an agent
// caller resolves to its agent_accounts.owner_user_id, while a user caller has
// no agent_accounts row and so COALESCE falls through to the caller id itself —
// a user owns itself. An unknown id likewise resolves to itself; that is
// deliberate and matches ReparentAgent's form, which leans on the subsequent
// same-owner and owner-FK checks to reject a bogus caller rather than probing
// existence here. ReparentAgent keeps its own inline copy of this resolution
// because it must run inside its serialized transaction (on tx, not s.pool) — do
// not route it through ResolveOwner, which would drop that guarantee.
func (s *Store) ResolveOwner(ctx context.Context, caller AccountID) (AccountID, error) {
	if caller == "" {
		return "", fmt.Errorf("%w: caller is required", ErrInvalidArgument)
	}
	owner, err := s.q.ResolveOwner(ctx, string(caller))
	if err != nil {
		return "", fmt.Errorf("store: resolve owner: %w", err)
	}
	return AccountID(owner), nil
}

// ReparentAgent moves an agent to a new parent in the agent tree, or promotes it
// to a root (empty newParentAgentID), as the serialized validate-and-write the
// agent-trees record §Server validation requires. The whole check-then-write
// runs in ONE transaction that first takes a per-owner-tree advisory lock
// (pg_advisory_xact_lock keyed on the moved agent's owner), so two concurrent
// individually-acyclic re-parents under the same owner cannot interleave into a
// persisted cycle — each serializes behind the lock, sees the other's write, and
// re-checks. The mutated account is re-read inside the txn and returned.
//
// Validation, each mapped to a distinct sentinel the edge turns into a gRPC code:
//   - (0) caller authority — the caller must be the moved agent's owner, or an
//     agent of that owner (its resolved owner equals the agent's owner). An
//     unknown moved agent is indistinguishable from a foreign one here: both
//     fail this clause. → ErrPermissionDenied.
//   - (1) same-owner — a non-empty new parent's owner must equal the moved
//     agent's owner. → ErrPermissionDenied.
//   - (2) no cycle — the new parent must be neither the agent itself nor any of
//     its transitive descendants; walk the parent chain up from the proposed
//     parent and reject if the agent is reached. The walk carries a visited set
//     so a pre-existing bad cycle cannot spin it. → ErrFailedPrecondition.
//   - (3) existence — a non-empty new parent must resolve to an existing agent
//     account. → ErrNotFound.
//
// Set-at-creation cannot cycle (a new account has no descendants), so the cycle
// check and its serialization live only here, on the mutable edge.
func (s *Store) ReparentAgent(ctx context.Context, caller, agentAccountID, newParentAgentID AccountID) (Account, error) {
	if caller == "" {
		return Account{}, fmt.Errorf("%w: caller is required", ErrInvalidArgument)
	}
	if agentAccountID == "" {
		return Account{}, fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin reparent agent: %w", err)
	}
	// Rolled back on every path that does not commit; a rollback after a
	// successful commit is a no-op the driver ignores, so the discard is safe.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// Resolve the moved agent's owner. Missing row = the id is unknown or names a
	// non-agent; either way clause 0 cannot hold, so it is folded into the
	// authority failure below rather than surfaced as a distinct existence error.
	agentExists := true
	agentOwner, err := qtx.GetAgentOwner(ctx, string(agentAccountID))
	if err != nil {
		if noRows(err) {
			agentExists = false
		} else {
			return Account{}, fmt.Errorf("store: resolve moved agent owner: %w", err)
		}
	}

	// Per-owner-tree lock: serialize every re-parent under this owner's tree so
	// the cycle check below reads a stable tree and no concurrent acyclic move
	// can interleave into a persisted cycle. hashtext -> int4 widens to the
	// bigint the advisory lock takes; the lock auto-releases at txn end. An
	// unknown agent has no owner to key on, so its (already-doomed) request locks
	// on its own id — never colliding with a real owner's tree. Two distinct
	// owners can hash-collide on the int4 hashtext key and spuriously serialize
	// each other's reparents — a benign liveness/throughput cost (a redundant
	// wait), never a wrong result, acceptable at expected fleet size.
	lockKey := agentOwner
	if !agentExists {
		lockKey = string(agentAccountID)
	}
	if err := qtx.AcquireOwnerTreeLock(ctx, lockKey); err != nil {
		return Account{}, fmt.Errorf("store: lock owner tree: %w", err)
	}

	// (0) Caller authority: the caller's resolved owner (its owner_user_id if it
	// is an agent, else itself) must equal the moved agent's owner.
	callerOwner, err := qtx.ResolveOwner(ctx, string(caller))
	if err != nil {
		return Account{}, fmt.Errorf("store: resolve caller owner: %w", err)
	}
	if !agentExists || callerOwner != agentOwner {
		return Account{}, fmt.Errorf("%w: caller may not re-parent agent %q", ErrPermissionDenied, agentAccountID)
	}

	if err := validateNewParent(ctx, qtx, agentAccountID, newParentAgentID, AccountID(agentOwner)); err != nil {
		return Account{}, err
	}

	// Capture the CURRENT parent before the UPDATE overwrites it: reparent-out
	// must reconcile the OLD manager's coordination channel too (its report set
	// loses this agent), per design.md:567 "reparent-out removes it". NULL parent
	// (a root) reads as an invalid pgtype.Text — no old manager to reconcile.
	oldParent, err := qtx.GetAgentParent(ctx, string(agentAccountID))
	if err != nil {
		return Account{}, fmt.Errorf("store: resolve old parent: %w", err)
	}

	if err := qtx.UpdateAgentParent(ctx, db.UpdateAgentParentParams{
		AccountID: string(agentAccountID),
		Column2:   string(newParentAgentID),
	}); err != nil {
		return Account{}, fmt.Errorf("store: update parent: %w", err)
	}

	// INVARIANT: every write of agent_accounts.parent_agent_id must invoke the
	// registered coordination hook. The UPDATE above rewrote it, so reconcile
	// BOTH affected managers' coordination channels on THIS tx (RIG-1722 T5,
	// design.md:550-551,567): the NEW parent gains this report (reparent-in adds
	// it) and the OLD parent loses it (reparent-out removes it). The reconcile is
	// a per-manager membership resync (idempotent), so invoking it for each with
	// a full resync naturally adds-on-new and removes-on-old. A promote-to-root
	// (empty new parent) or a former-root move (empty old parent) skips the empty
	// side — that manager does not exist. Skip the old side when it equals the
	// new (a no-op move) to avoid a redundant second resync of the same channel.
	if newParentAgentID != "" {
		if err := s.invokeCoordinationHook(ctx, tx, newParentAgentID); err != nil {
			return Account{}, err
		}
	}
	if oldParent.Valid && oldParent.String != "" && AccountID(oldParent.String) != newParentAgentID {
		if err := s.invokeCoordinationHook(ctx, tx, AccountID(oldParent.String)); err != nil {
			return Account{}, err
		}
	}

	row, err := qtx.GetAccount(ctx, string(agentAccountID))
	if err != nil {
		return Account{}, fmt.Errorf("store: re-read reparented account: %w", err)
	}
	acc := accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID)
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit reparent agent: %w", err)
	}
	return acc, nil
}

// validateNewParent runs clauses 1–3 of the re-parent validation against a
// proposed non-empty parent, inside the caller's transaction (which already
// holds the per-owner-tree lock). An empty newParentAgentID is a promote-to-root
// with no parent to check, so it returns nil immediately. agentOwner is the
// moved agent's already-resolved owner. It reads through the caller's tx-scoped
// queries so every probe sees the serialized tree.
func validateNewParent(ctx context.Context, q *db.Queries, agentAccountID, newParentAgentID, agentOwner AccountID) error {
	if newParentAgentID == "" {
		return nil
	}

	// (1)+(3) existence and same-owner for the proposed parent.
	parentOwner, err := q.GetAgentOwner(ctx, string(newParentAgentID))
	if err != nil {
		if noRows(err) {
			return fmt.Errorf("%w: parent agent %q", ErrNotFound, newParentAgentID)
		}
		return fmt.Errorf("store: resolve new parent owner: %w", err)
	}
	if AccountID(parentOwner) != agentOwner {
		// Cross-owner reparent is rejected. On the ReparentAgent RPC path this
		// clause is edge-shadowed: comms.ReparentAgent rejects a foreign parent
		// at the service edge (naming the submitted handle, DL-269 oracle
		// invariant) BEFORE calling the store, so this ErrPermissionDenied only
		// surfaces to a direct store caller (independently tested) — it remains
		// as store-layer defense-in-depth, not dead code.
		return fmt.Errorf("%w: parent agent %q has a different owner", ErrPermissionDenied, newParentAgentID)
	}

	// (2) No cycle: walk up the parent chain from the proposed parent; if the
	// moved agent is reached, the move would make it its own ancestor. The
	// visited set bounds the walk so a pre-existing cycle in the data cannot spin
	// it forever.
	cur := newParentAgentID
	visited := map[AccountID]bool{}
	for cur != "" {
		if cur == agentAccountID {
			return fmt.Errorf("%w: re-parenting agent %q under %q would form a cycle", ErrFailedPrecondition, agentAccountID, newParentAgentID)
		}
		if visited[cur] {
			break
		}
		visited[cur] = true
		next, err := q.GetAgentParent(ctx, string(cur))
		if err != nil {
			if noRows(err) {
				break
			}
			return fmt.Errorf("store: walk parent chain: %w", err)
		}
		if !next.Valid {
			break
		}
		cur = AccountID(next.String)
	}
	return nil
}

// AgentByHandle returns the agent account with the given handle in owner's agent
// namespace (RIG-2751 handle cutover: agent handles are unique only per owner,
// so resolution is owner-qualified over account_handles' agent index,
// `UNIQUE(owner_user_id, handle) WHERE owner_user_id IS NOT NULL`). owner is the
// owning user's account id — from a parsed `owner/` qualifier, or the caller's
// own owner for a bare handle. It returns the full Account so the caller
// owner-checks the result itself. A handle that is unknown in this owner's
// namespace (or that resolves to a non-agent) is ErrNotFound: an unknown,
// wrong-owner, or non-agent handle is deliberately indistinguishable, so this
// fails closed and never resolves or elevates a non-agent.
func (s *Store) AgentByHandle(ctx context.Context, owner AccountID, handle string) (Account, error) {
	if handle == "" {
		return Account{}, fmt.Errorf("%w: handle is required", ErrInvalidArgument)
	}
	if owner == "" {
		// No owner namespace to resolve in: fail closed exactly like an unknown
		// handle (indistinguishable from the wrong-owner miss below).
		return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
	}
	row, err := s.q.GetAccountByOwnerHandle(ctx, db.GetAccountByOwnerHandleParams{
		OwnerUserID: pgtype.Text{String: string(owner), Valid: true},
		Handle:      handle,
	})
	if err != nil {
		if noRows(err) {
			return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
		}
		return Account{}, fmt.Errorf("store: resolve agent by handle: %w", err)
	}
	acc := accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID)
	if !acc.IsAgent() {
		// Identical wrapped text to the noRows branch above: a non-agent handle
		// must be indistinguishable from an unknown one at the message-text level
		// too, not just the sentinel. The distinguishing detail stays out of the
		// client-visible error (the edge maps the store err verbatim).
		return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
	}
	return acc, nil
}

// UserByHandle resolves a bare user/system handle in the global handle index
// (`UNIQUE(handle) WHERE owner_user_id IS NULL`) to its full account. It is the
// global-tier counterpart to the owner-qualified AgentByHandle, for a caller
// that holds a bare user handle and needs the account (e.g. resolving an owner
// namespace before an agent lookup). An unknown handle is ErrNotFound.
func (s *Store) UserByHandle(ctx context.Context, handle string) (Account, error) {
	if handle == "" {
		return Account{}, fmt.Errorf("%w: handle is required", ErrInvalidArgument)
	}
	row, err := s.q.GetAccountByGlobalHandle(ctx, handle)
	if err != nil {
		if noRows(err) {
			return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
		}
		return Account{}, fmt.Errorf("store: resolve user by handle: %w", err)
	}
	return accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
		row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID), nil
}

// QualifiedHandle is a submitted account handle parsed into its owner qualifier
// and bare handle. Owner is empty for a bare handle (`matt`, `compass-ux`),
// non-empty for an owner-qualified agent handle (`matt/compass-ux` → Owner
// "matt", Handle "compass-ux"). Raw preserves the exact submitted spelling so a
// resolver error can name it back verbatim (the oracle-safe message contract).
type QualifiedHandle struct {
	Owner  string
	Handle string
	Raw    string
}

// ParseQualifiedHandle splits a submitted handle on the FIRST '/': everything
// before it is the owner qualifier, everything after is the agent handle. No
// '/' means a bare handle (Owner empty). It is a pure edge helper — the split
// only; namespace resolution is AccountsByHandles' job.
func ParseQualifiedHandle(raw string) QualifiedHandle {
	if owner, handle, ok := strings.Cut(raw, "/"); ok {
		return QualifiedHandle{Owner: owner, Handle: handle, Raw: raw}
	}
	return QualifiedHandle{Handle: raw, Raw: raw}
}

// AccountsByHandles resolves a batch of owner-qualified-or-bare handles to their
// account ids over the account_handles resolution index (RIG-2751 handle
// cutover), for the member/owner request fields that legitimately name users as
// well as agents. Resolution per input (§"The storage contract"):
//
//   - owner-qualified (`matt/compass-ux`): resolve the owner segment bare in the
//     user/system global index → its account_id is the owner_user_id → resolve
//     the agent segment in that owner's agent index.
//   - bare (`matt`, `compass-ux`): resolve EITHER as a user handle in the global
//     index OR as an agent handle in the CALLER'S OWN owner namespace
//     (callerOwner). The system account is never a member/owner target, so the
//     global arm excludes system_accounts rows.
//
// Every arm is intersected with the account-visibility predicate keyed on viewer
// (OQ-6 SCOPED): a real-but-invisible handle misses exactly like an unknown one,
// so resolution and the roster clip stay aligned by construction.
//
// ATOMIC (OQ-2): any handle that fails to resolve fails the whole call with
// ErrNotFound naming EVERY unresolved handle in its submitted spelling (same
// message template as AgentByHandle). On success the returned map is keyed by
// each input's submitted spelling (QualifiedHandle.Raw) → resolved id, so the
// caller gets the full hit set (the set-difference is free). Empty input is a
// no-op (empty map, nil error).
func (s *Store) AccountsByHandles(ctx context.Context, viewer, callerOwner AccountID, handles []QualifiedHandle) (map[string]AccountID, error) {
	hits := make(map[string]AccountID, len(handles))
	var missing []string
	for _, qh := range handles {
		id, err := s.resolveOneHandle(ctx, viewer, callerOwner, qh)
		if err != nil {
			return nil, err
		}
		if id == "" {
			missing = append(missing, qh.Raw)
			continue
		}
		hits[qh.Raw] = id
	}
	if len(missing) > 0 {
		// Name ALL unresolved handles in their submitted spelling, so the caller
		// cannot probe which specific handle was the miss (oracle-safe), same
		// wrapped-text template as AgentByHandle.
		return nil, fmt.Errorf("%w: handle %q", ErrNotFound, strings.Join(missing, ", "))
	}
	return hits, nil
}

// resolveOneHandle resolves a single QualifiedHandle to a visible, non-system
// account id, or returns ("", nil) for a clean miss (unknown, wrong-namespace,
// or invisible — all indistinguishable). A real query fault is a non-nil error.
func (s *Store) resolveOneHandle(ctx context.Context, viewer, callerOwner AccountID, qh QualifiedHandle) (AccountID, error) {
	if qh.Handle == "" {
		return "", nil
	}
	if qh.Owner != "" {
		// owner-qualified: resolve the owner segment in the global user/system
		// index (excluding system, which owns no agents), then the agent segment
		// under it.
		ownerID, err := s.globalHandleID(ctx, qh.Owner)
		if err != nil {
			return "", err
		}
		if ownerID == "" {
			return "", nil
		}
		return s.visibleAgentHandleID(ctx, viewer, ownerID, qh.Handle)
	}
	// bare: a user/system-tier handle in the global index (visible, non-system),
	// OR an agent handle in the caller's own owner namespace.
	id, err := s.visibleGlobalHandleID(ctx, viewer, qh.Handle)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return s.visibleAgentHandleID(ctx, viewer, callerOwner, qh.Handle)
}

// globalHandleID resolves a bare handle in the global user/system index
// (owner_user_id IS NULL), excluding the system account, WITHOUT a visibility
// clip — it backs the owner-qualifier lookup, whose owner is a namespace key,
// not an addressed target. Empty id on a clean miss.
func (s *Store) globalHandleID(ctx context.Context, handle string) (AccountID, error) {
	id, err := s.q.GetGlobalHandleID(ctx, handle)
	if err != nil {
		if noRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("store: resolve global handle: %w", err)
	}
	return AccountID(id), nil
}

// visibleGlobalHandleID resolves a bare handle in the global user/system index,
// excluding the system account AND intersecting the viewer's account-visible set
// (the shared visibility predicate). Empty id on a clean miss (unknown or
// invisible).
func (s *Store) visibleGlobalHandleID(ctx context.Context, viewer AccountID, handle string) (AccountID, error) {
	id, err := s.q.GetVisibleGlobalHandleID(ctx, db.GetVisibleGlobalHandleIDParams{
		ID:     string(viewer),
		Handle: handle,
	})
	if err != nil {
		if noRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("store: resolve visible global handle: %w", err)
	}
	return AccountID(id), nil
}

// visibleAgentHandleID resolves an agent handle in owner's agent namespace
// (owner_user_id = owner), intersecting the viewer's account-visible set. An
// empty owner (no caller namespace) or a clean miss returns an empty id.
func (s *Store) visibleAgentHandleID(ctx context.Context, viewer, owner AccountID, handle string) (AccountID, error) {
	if owner == "" {
		return "", nil
	}
	id, err := s.q.GetVisibleAgentHandleID(ctx, db.GetVisibleAgentHandleIDParams{
		ID:          string(viewer),
		OwnerUserID: pgtype.Text{String: string(owner), Valid: true},
		Handle:      handle,
	})
	if err != nil {
		if noRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("store: resolve visible agent handle: %w", err)
	}
	return AccountID(id), nil
}

// ListAccounts returns the accounts visible to visibleTo. The visibility rule
// (D9, owner-gated access — the frozen record pins DM/channel visibility
// precisely but delegates account-listing scope to "the accounts visible to the
// caller", comms.proto:48-49; this is the store's conservative realization,
// flagged for review): the caller always sees itself and every user account (the
// first-class member directory the management hierarchy needs), and sees an agent
// account only when it owns that agent or shares a channel with it — so an
// owner-scoped agent never leaks to an unrelated account. The predicate is
// textually shared across the ListVisibleAccounts, AccountVisibleTo, and the two
// visible-handle queries (queries/accounts.sql) so the stream edge's per-event
// account filter cannot drift from this list read (the anti-drift guarantee the
// frozen "store is the D9 source of truth" requires).
func (s *Store) ListAccounts(ctx context.Context, visibleTo AccountID) ([]Account, error) {
	rows, err := s.q.ListVisibleAccounts(ctx, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, accountFromRow(row.ID, row.Handle, row.DisplayName, row.UserRole,
			row.OwnerUserID, row.HomeChannelID, row.Persona, row.AgentRole, row.ParentAgentID, row.SystemAccountID))
	}
	return accounts, nil
}

// AccountVisibleTo reports whether actor may see target — the single-id form of
// the ListAccounts predicate, used by the SubscribeComms stream edge to filter
// AccountChanged so the directory event rides at read-parity (a viewer never
// learns of an agent it could not list). Shares the visibility predicate with
// the list read (queries/accounts.sql) so the two cannot drift.
func (s *Store) AccountVisibleTo(ctx context.Context, actor AccountID, target AccountID) (bool, error) {
	visible, err := s.q.AccountVisibleTo(ctx, db.AccountVisibleToParams{
		ID:   string(actor),
		ID_2: string(target),
	})
	if err != nil {
		return false, fmt.Errorf("store: check account visibility: %w", err)
	}
	return visible, nil
}
