package store

import "errors"

// Postgres SQLSTATE codes the store maps to its sentinels (pgErrIs). 23505 is a
// unique_violation (duplicate handle / channel name / re-used id → ErrConflict);
// 23503 is a foreign_key_violation (an input referencing a row that does not
// exist → ErrInvalidArgument).
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// The store's sentinel errors. Callers (the comms service, the auth layer)
// discriminate with errors.Is and map each to a connect status code at the RPC
// edge — a package-level var per class so the mapping is stable and %w-wrapped
// context can ride along (F-oops composes with this later without changing the
// sentinels). Package-level error vars are the idiomatic Go form the lint
// config keeps (gochecknoglobals disabled by design, .golangci.yml).

var (
	// ErrNotFound is returned when a row addressed by id does not exist, or
	// exists but the actor may not see it — the two are deliberately
	// indistinguishable to the caller so a probe cannot enumerate ids it lacks
	// visibility for (the not-found/forbidden merge, D9).
	ErrNotFound = errors.New("store: not found")

	// ErrConflict is a conflict the store refuses: a uniqueness violation (a
	// duplicate account handle, a channel name already taken in its group, or a
	// re-used id), or an operation rejected because its target is already in a
	// terminal state (a second answer to an already-answered ask, RIG-1243).
	ErrConflict = errors.New("store: conflict")

	// ErrInvalidArgument is a malformed input the store rejects before touching
	// Postgres: an empty required field, a page cursor that isn't a known id, a
	// message with no blocks, or a group visibility wider than its parent's.
	ErrInvalidArgument = errors.New("store: invalid argument")

	// ErrTokenRevoked is returned by ResolveTokenHash when the hash matches a
	// token that has been revoked — distinct from ErrNotFound (never issued) so
	// the door can tell a withdrawn credential from an unknown one.
	ErrTokenRevoked = errors.New("store: token revoked")

	// ErrPermissionDenied is returned when the caller is not authorized to
	// perform an operation on a row it can address: a ReparentAgent where the
	// caller is neither the moved agent's owner nor an agent of that owner
	// (clause 0), or where the proposed parent belongs to a different owner
	// (clause 1). Distinct from ErrNotFound so the edge can map it to
	// PERMISSION_DENIED (agent-trees record §Server validation).
	ErrPermissionDenied = errors.New("store: permission denied")

	// ErrFailedPrecondition is returned when an operation is well-formed and
	// authorized but would violate a structural invariant: a ReparentAgent that
	// would make an agent its own ancestor (a cycle, clause 2). Distinct so the
	// edge maps it to FAILED_PRECONDITION.
	ErrFailedPrecondition = errors.New("store: failed precondition")

	// ErrSchemaVersion is returned by Open when the database's applied schema
	// version does not match the version this binary's embedded migrations
	// define — the refuse-to-serve guard (design.md:1136-1137). A newer
	// database than the binary (a rollback) is never silently served.
	ErrSchemaVersion = errors.New("store: schema version mismatch")
)
