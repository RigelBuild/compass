# orion-ref-gate

Checks that public checkout files do not disclose the private platform repo by
name or recognizable reference. The gate's own name is the one exempt use.
It scans tracked files and fails on a match. The boundary rule is in
`docs/concepts/self-host-and-managed.md`.

## Run

```sh
moon run orion-ref-gate:check
moon run orion-ref-gate:ci
```

The `ci` task also runs its typecheck and tests. The `.github/workflows/ci.yml`
workflow runs that task in its `moon (bun)` job when the concern matrix selects
it. The push and scheduled runs check the full set.

## Credentials

None. The gate scans tracked checkout files and does not call an authenticated
service.

## Rotate

Nothing to rotate.
