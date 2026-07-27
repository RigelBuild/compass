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
		       ag.owner_user_id, ag.home_channel_id
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
		"INSERT INTO agent_accounts (account_id, owner_user_id, home_channel_id) VALUES ($1, $2, $3)",
		accountID, string(ownerUserID), channelID,
	); err != nil {
		// owner_user_id references user_accounts; an unknown owner is a caller
		// error, not a store fault.
		if pgErrIs(err, pgForeignKeyViolation) {
			return Account{}, fmt.Errorf("%w: unknown owner user %q", ErrInvalidArgument, ownerUserID)
		}
		return Account{}, fmt.Errorf("store: insert agent_account: %w", err)
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
		       ag.owner_user_id, ag.home_channel_id
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
		       ag.owner_user_id, ag.home_channel_id` +
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
	)
	if err := row.Scan(&id, &handle, &displayName, &role, &ownerUserID, &homeChannelID); err != nil {
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
		acc.Agent = agent
	}
	return acc, nil
}
