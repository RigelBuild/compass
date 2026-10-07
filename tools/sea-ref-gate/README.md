# sea-ref-gate

Finds retired `SEA-<digits>` issue references in tracked files. The Linear team
key is now RIG, with issue numbers preserved, so this gate prevents old issue
references from returning.

## Run

```sh
moon run sea-ref-gate:check
moon run sea-ref-gate:ci
```

`check` scans the tracked tree. `ci` also runs typechecking and tests. The
`.github/workflows/ci.yml` workflow runs the `ci` task in its `moon (bun)` job
when the concern matrix selects it.

## Credentials

None. The gate scans tracked checkout files and does not call an authenticated
service.

## Rotate

Nothing to rotate.
