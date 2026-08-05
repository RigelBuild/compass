package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CreateUser inserts a human account (a regular member; admin elevation is a
// separate path, comms.proto:39-42) and returns it with its server-assigned id.
// A duplicate handle is ErrConflict.
func (s *Store) CreateUser(ctx context.Context, u NewUser) (Account, error) {
	if u.Handle == "" {
		return Account{}, fmt.Errorf("%w: user handle is required", ErrInvalidArgument)
	}

	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin create user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		"INSERT INTO accounts (id, handle, display_name) VALUES ($1, $2, $3)",
		id, u.Handle, u.DisplayName,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return Account{}, fmt.Errorf("%w: handle %q already taken", ErrConflict, u.Handle)
		}
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO user_accounts (account_id, role) VALUES ($1, $2)", id, int32(UserRoleMember),
	); err != nil {
		return Account{}, fmt.Errorf("store: insert user_account: %w", err)
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

	id := newID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("store: begin bootstrap admin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		"INSERT INTO accounts (id, handle, display_name) VALUES ($1, $2, $3)",
		id, u.Handle, u.DisplayName,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			// Already bootstrapped (restart): fetch and return the existing admin.
			return s.adminByHandle(ctx, u.Handle)
		}
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO user_accounts (account_id, role) VALUES ($1, $2)", id, int32(UserRoleAdmin),
	); err != nil {
		return Account{}, fmt.Errorf("store: insert user_account: %w", err)
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
	const q = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.parent_agent_id
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		LEFT JOIN agent_accounts ag ON ag.account_id = a.id
		WHERE a.handle = $1`
	acc, err := scanAccount(s.pool.QueryRow(ctx, q, handle))
	if err != nil {
		return Account{}, err
	}
	if acc.User == nil || acc.User.Role != UserRoleAdmin {
		return Account{}, fmt.Errorf("%w: handle %q exists but is not an admin", ErrConflict, handle)
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

	if _, err := tx.Exec(ctx,
		"INSERT INTO accounts (id, handle, display_name) VALUES ($1, $2, $3)",
		accountID, a.Handle, a.DisplayName,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return Account{}, fmt.Errorf("%w: handle %q already taken", ErrConflict, a.Handle)
		}
		return Account{}, fmt.Errorf("store: insert account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO agent_accounts (account_id, owner_user_id, home_channel_id, persona, parent_agent_id) VALUES ($1, $2, $3, $4, NULLIF($5, ''))",
		accountID, string(ownerUserID), channelID, a.Persona, string(a.ParentAgentID),
	); err != nil {
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

	// INVARIANT: every write of agent_accounts.parent_agent_id must invoke the
	// registered coordination hook. The INSERT above just wrote it; invoke the
	// hook on THIS tx for the new agent's PARENT (the manager that gains this
	// report), so the coordination-channel reconcile commits atomically with the
	// tree edge (SEA-1722 T5, design.md:550-551). Skipped when parent is empty: a
	// root agent has no manager, so there is no coordination channel to reconcile.
	if a.ParentAgentID != "" {
		if err := s.invokeCoordinationHook(ctx, tx, a.ParentAgentID); err != nil {
			return Account{}, err
		}
	}

	// Mint the home channel: named for the agent, ungrouped (owner-scoped), with
	// the owner and the agent as members and the agent always-subscribed.
	if _, err := tx.Exec(ctx,
		"INSERT INTO channels (id, name, group_id, kind) VALUES ($1, $2, NULL, $3)",
		channelID, a.Handle, int32(ChannelKindChannel),
	); err != nil {
		return Account{}, fmt.Errorf("store: insert home channel: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE), ($1, $3, TRUE)",
		channelID, string(ownerUserID), accountID,
	); err != nil {
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
			ParentAgentID: a.ParentAgentID,
		},
	}, nil
}

// GetAccount returns one account by id, or ErrNotFound if it does not exist.
// GetAccount is an id-addressed fetch used internally by other store methods
// and the auth layer; caller-facing visibility scoping is applied by
// ListAccounts and the per-container reads, not here.
func (s *Store) GetAccount(ctx context.Context, id AccountID) (Account, error) {
	const q = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.parent_agent_id
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		LEFT JOIN agent_accounts ag ON ag.account_id = a.id
		WHERE a.id = $1`
	acc, err := scanAccount(s.pool.QueryRow(ctx, q, string(id)))
	if err != nil {
		if noRows(err) {
			return Account{}, fmt.Errorf("%w: account %q", ErrNotFound, id)
		}
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	return acc, nil
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
	var ownerUserID string
	if err := s.pool.QueryRow(ctx,
		`SELECT owner_user_id FROM agent_accounts WHERE account_id = $1`,
		string(agentAccountID),
	).Scan(&ownerUserID); err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: agent %q", ErrNotFound, agentAccountID)
		}
		return "", fmt.Errorf("store: resolve agent owner: %w", err)
	}
	return AccountID(ownerUserID), nil
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

	// Resolve the moved agent's owner. Missing row = the id is unknown or names a
	// non-agent; either way clause 0 cannot hold, so it is folded into the
	// authority failure below rather than surfaced as a distinct existence error.
	var agentOwner string
	agentExists := true
	if err := tx.QueryRow(ctx,
		`SELECT owner_user_id FROM agent_accounts WHERE account_id = $1`,
		string(agentAccountID),
	).Scan(&agentOwner); err != nil {
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return Account{}, fmt.Errorf("store: lock owner tree: %w", err)
	}

	// (0) Caller authority: the caller's resolved owner (its owner_user_id if it
	// is an agent, else itself) must equal the moved agent's owner.
	var callerOwner string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)`,
		string(caller),
	).Scan(&callerOwner); err != nil {
		return Account{}, fmt.Errorf("store: resolve caller owner: %w", err)
	}
	if !agentExists || callerOwner != agentOwner {
		return Account{}, fmt.Errorf("%w: caller may not re-parent agent %q", ErrPermissionDenied, agentAccountID)
	}

	if err := validateNewParent(ctx, tx, agentAccountID, newParentAgentID, AccountID(agentOwner)); err != nil {
		return Account{}, err
	}

	// Capture the CURRENT parent before the UPDATE overwrites it: reparent-out
	// must reconcile the OLD manager's coordination channel too (its report set
	// loses this agent), per design.md:567 "reparent-out removes it". NULL parent
	// (a root) scans to empty — no old manager to reconcile.
	var oldParent *string
	if err := tx.QueryRow(ctx,
		`SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1`,
		string(agentAccountID),
	).Scan(&oldParent); err != nil {
		return Account{}, fmt.Errorf("store: resolve old parent: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE agent_accounts SET parent_agent_id = NULLIF($2, '') WHERE account_id = $1`,
		string(agentAccountID), string(newParentAgentID),
	); err != nil {
		return Account{}, fmt.Errorf("store: update parent: %w", err)
	}

	// INVARIANT: every write of agent_accounts.parent_agent_id must invoke the
	// registered coordination hook. The UPDATE above rewrote it, so reconcile
	// BOTH affected managers' coordination channels on THIS tx (SEA-1722 T5,
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
	if oldParent != nil && *oldParent != "" && AccountID(*oldParent) != newParentAgentID {
		if err := s.invokeCoordinationHook(ctx, tx, AccountID(*oldParent)); err != nil {
			return Account{}, err
		}
	}

	const q = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.parent_agent_id
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		LEFT JOIN agent_accounts ag ON ag.account_id = a.id
		WHERE a.id = $1`
	acc, err := scanAccount(tx.QueryRow(ctx, q, string(agentAccountID)))
	if err != nil {
		return Account{}, fmt.Errorf("store: re-read reparented account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("store: commit reparent agent: %w", err)
	}
	return acc, nil
}

// validateNewParent runs clauses 1–3 of the re-parent validation against a
// proposed non-empty parent, inside the caller's transaction (which already
// holds the per-owner-tree lock). An empty newParentAgentID is a promote-to-root
// with no parent to check, so it returns nil immediately. agentOwner is the
// moved agent's already-resolved owner.
func validateNewParent(ctx context.Context, tx pgx.Tx, agentAccountID, newParentAgentID, agentOwner AccountID) error {
	if newParentAgentID == "" {
		return nil
	}

	// (1)+(3) existence and same-owner for the proposed parent.
	var parentOwner string
	if err := tx.QueryRow(ctx,
		`SELECT owner_user_id FROM agent_accounts WHERE account_id = $1`,
		string(newParentAgentID),
	).Scan(&parentOwner); err != nil {
		if noRows(err) {
			return fmt.Errorf("%w: parent agent %q", ErrNotFound, newParentAgentID)
		}
		return fmt.Errorf("store: resolve new parent owner: %w", err)
	}
	if AccountID(parentOwner) != agentOwner {
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
		var next *string
		if err := tx.QueryRow(ctx,
			`SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1`,
			string(cur),
		).Scan(&next); err != nil {
			if noRows(err) {
				break
			}
			return fmt.Errorf("store: walk parent chain: %w", err)
		}
		if next == nil {
			break
		}
		cur = AccountID(*next)
	}
	return nil
}

// AgentByHandle returns the agent account with the given handle. The crash-
// recovery resume path needs an owner-checkable handle lookup, and the private
// adminByHandle cannot be reused because it asserts admin — this one never
// asserts or elevates. It returns the full Account so the caller owner-checks the
// result itself. A handle that is unknown, or that names a non-agent account, is
// ErrNotFound: a user handle is deliberately indistinguishable from an unknown
// one, so this fails closed and never resolves or elevates a non-agent.
func (s *Store) AgentByHandle(ctx context.Context, handle string) (Account, error) {
	if handle == "" {
		return Account{}, fmt.Errorf("%w: handle is required", ErrInvalidArgument)
	}
	const q = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.parent_agent_id
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		LEFT JOIN agent_accounts ag ON ag.account_id = a.id
		WHERE a.handle = $1`
	acc, err := scanAccount(s.pool.QueryRow(ctx, q, handle))
	if err != nil {
		if noRows(err) {
			return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
		}
		return Account{}, fmt.Errorf("store: resolve agent by handle: %w", err)
	}
	if !acc.IsAgent() {
		// Identical wrapped text to the noRows branch above: a user handle must
		// be indistinguishable from an unknown one at the message-text level too,
		// not just the sentinel — the reason this lookup never reuses the
		// admin-asserting adminByHandle. The distinguishing detail stays out of
		// the client-visible error (the edge maps the store err verbatim).
		return Account{}, fmt.Errorf("%w: handle %q", ErrNotFound, handle)
	}
	return acc, nil
}

// accountVisibleFromWhere is the FROM + JOINs + visibility predicate shared by
// ListAccounts and AccountVisibleTo, so the stream edge's per-event account
// filter cannot drift from the ListAccounts read (the anti-drift guarantee the
// frozen "store is the D9 source of truth" requires). $1 is the viewer; the
// predicate is parenthesized so a caller may AND a row selector onto it.
//
// Visibility rule (D9, owner-gated access — the frozen record pins DM/channel
// visibility precisely but delegates account-listing scope to "the accounts
// visible to the caller", comms.proto:48-49; this is the store's conservative
// realization, flagged for review): the caller always sees itself and every user
// account (the first-class member directory the management hierarchy needs), and
// sees an agent account only when it owns that agent or shares a channel with it
// — so an owner-scoped agent never leaks to an unrelated account.
const accountVisibleFromWhere = `
		FROM accounts a
		LEFT JOIN user_accounts u ON u.account_id = a.id
		LEFT JOIN agent_accounts ag ON ag.account_id = a.id
		WHERE (
		        a.id = $1
		     OR u.account_id IS NOT NULL
		     OR ag.owner_user_id = $1
		     OR EXISTS (
		         SELECT 1
		         FROM channel_members cm_self
		         JOIN channel_members cm_them ON cm_them.channel_id = cm_self.channel_id
		         WHERE cm_self.account_id = $1 AND cm_them.account_id = a.id
		     )
		      )`

// ListAccounts returns the accounts visible to visibleTo (see
// accountVisibleFromWhere for the visibility rule).
func (s *Store) ListAccounts(ctx context.Context, visibleTo AccountID) ([]Account, error) {
	const q = `
		SELECT a.id, a.handle, a.display_name,
		       u.role,
		       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.parent_agent_id` +
		accountVisibleFromWhere + `
		ORDER BY a.handle`
	rows, err := s.pool.Query(ctx, q, string(visibleTo))
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		accounts = append(accounts, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate accounts: %w", err)
	}
	return accounts, nil
}

// AccountVisibleTo reports whether actor may see target — the single-id form of
// the ListAccounts predicate, used by the SubscribeComms stream edge to filter
// AccountChanged so the directory event rides at read-parity (a viewer never
// learns of an agent it could not list). Shares accountVisibleFromWhere with the
// list read so the two cannot drift.
func (s *Store) AccountVisibleTo(ctx context.Context, actor AccountID, target AccountID) (bool, error) {
	var visible bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1`+accountVisibleFromWhere+` AND a.id = $2)`,
		string(actor), string(target),
	).Scan(&visible); err != nil {
		return false, fmt.Errorf("store: check account visibility: %w", err)
	}
	return visible, nil
}

// scanAccount reads one joined account row (accounts LEFT JOIN user_accounts
// LEFT JOIN agent_accounts) into an Account, setting exactly the User or Agent
// subtype by which side of the join populated. Shared by GetAccount and
// ListAccounts so the oneof reconstruction lives in one place.
func scanAccount(row pgx.Row) (Account, error) {
	var (
		acc           Account
		id, handle    string
		displayName   string
		role          *int32
		ownerUserID   *string
		homeChannelID *string
		persona       *string
		parentAgentID *string
	)
	if err := row.Scan(&id, &handle, &displayName, &role, &ownerUserID, &homeChannelID, &persona, &parentAgentID); err != nil {
		return Account{}, err
	}
	acc.ID = AccountID(id)
	acc.Handle = handle
	acc.DisplayName = displayName
	switch {
	case role != nil:
		acc.User = &UserAccount{Role: UserRole(*role)}
	case ownerUserID != nil:
		agent := &AgentAccount{OwnerUserID: AccountID(*ownerUserID)}
		if homeChannelID != nil {
			agent.HomeChannelID = ChannelID(*homeChannelID)
		}
		if persona != nil {
			agent.Persona = *persona
		}
		if parentAgentID != nil {
			agent.ParentAgentID = AccountID(*parentAgentID)
		}
		acc.Agent = agent
	}
	return acc, nil
}
