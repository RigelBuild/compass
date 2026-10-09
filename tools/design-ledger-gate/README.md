# design-ledger-gate

Validates per-area decision files under `docs/designs/decisions/`, record
`Status:` headers, record links, and supersession pointers. On pull requests, it
also checks that a changed design record is coupled to a changed decision file
or a `Ledger-impact:` note.

## Run

```sh
moon run design-ledger-gate:check
moon run design-ledger-gate:ci
```

The snapshot check runs locally. CI runs `design-ledger-gate:ci` on every PR
in the `moon (bun)` job of `.github/workflows/ci.yml`; that job supplies PR
coordinates for the touch-coupling check.

## Credentials

`GH_TOKEN` lets the GitHub CLI read pull request metadata and changed files.
`.github/workflows/ci.yml` sets it from `github.token` when this gate runs. A
local PR check needs an operator-provided `GH_TOKEN` and the `REPO` and
`PR_NUMBER` environment variables.

## Rotate

GitHub issues the CI `github.token` for each workflow run and expires it
automatically. There is no long-lived repository secret to rotate. Rotate a
local `GH_TOKEN` at the issuer that created it.
