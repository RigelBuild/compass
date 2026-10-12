# design-ledger-gate

Validates per-area decision files under `docs/designs/decisions/`, record
`Status:` headers, record links, and supersession pointers. On pull requests, it
also checks that a changed design record is coupled to a changed decision file
or a `Ledger-impact:` note.

## Claim checks

On PRs, new decision IDs must be claimed for the PR head branch. The gate calls
the anonymous `GET https://dl.rigel.build/claims?repo=compass&lane=<lane>&ids=<ids>`
route in batches of 100. An ID not claimed for that lane is a violation. Service
failure exits 2 with `cannot reach the DL claim service`; rate-limit exhaustion
exits 2 with `the DL claim service rate-limited this runner for 65 s`. Fork PRs
must come from a branch in this repository. Non-PR runs and bot-pinned exempt
branches skip this leg.

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
