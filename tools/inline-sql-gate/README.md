# inline-sql-gate

Checks Go source for inline SQL passed to pgx `Query`, `QueryRow`, or `Exec`
calls. The generated database package, tests, and current ratcheting allowlist
are excluded; stale allowlist entries fail the gate.

## Run

```sh
moon run inline-sql-gate:check
moon run inline-sql-gate:ci
```

`check` scans the repository Go files. `ci` also runs the typecheck and fixture
tests. CI runs the selected task in the `moon (bun)` job of
`.github/workflows/ci.yml`.

## Credentials

None. The gate scans Go files in the checkout and does not call an authenticated
service.

## Rotate

Nothing to rotate.
