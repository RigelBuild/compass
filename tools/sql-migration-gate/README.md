# sql-migration-gate

Checks first-party SQL migrations for unsafe schema changes, SQL style issues,
and edits to migrations already present at the comparison ref. It runs all
three checks and fails if any one reports a problem.

## Run

```sh
moon run sql-migration-gate:check
moon run sql-migration-gate:ci
```

`check` runs `squawk`, `sqruff`, and the append-only comparison. It needs both
linters on `PATH`. `ci` also runs typechecking and tests. The
`.github/workflows/ci.yml` workflow runs the `ci` task in its `moon (bun)` job
when the concern matrix selects it.

## Credentials

None. The gate reads local migration files and repository refs. It does not
call an authenticated service.

## Rotate

Nothing to rotate.
