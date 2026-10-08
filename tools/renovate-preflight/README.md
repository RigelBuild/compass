# renovate-preflight

Checks that the Renovate GitHub App token can access the repository before
Renovate starts. It reports an actionable failure instead of Renovate's opaque
authentication error.

## Run

```sh
moon run renovate-preflight:ci
bun tools/renovate-preflight/index.ts
```

The `ci` task runs typecheck and unit tests. The preflight command needs `REPO`,
`RENOVATE_TOKEN`, `GH_TOKEN` set to the same token, and `gh` on `PATH`.
Without `GH_TOKEN`, `gh` uses your own login and the probe does not test the
App token. The `renovate` job in
`.github/workflows/renovate.yml` runs it before Renovate.

## Credentials

- `RENOVATE_TOKEN` is the per-run GitHub App installation token. The workflow
  also sets `GH_TOKEN` to the same token for the `gh` GraphQL probe.
- The workflow mints the token from `vars.RENOVATE_APP_CLIENT_ID` and
  `secrets.RENOVATE_APP_PRIVATE_KEY` in the `main` environment.

## Rotate

GitHub mints a new installation token for each workflow run; it expires after
about one hour. There is no stored `RENOVATE_TOKEN` to rotate. Infrastructure-as-code in the platform repo stages
`RENOVATE_APP_PRIVATE_KEY` and `RENOVATE_APP_CLIENT_ID`. To rotate the key,
a maintainer issues the new value and sets it in the platform repo's secrets
stack, applies that stack, then applies the stack that stages the GitHub
Actions secrets. Revoke the old key in the App settings only after the
second apply. Nobody edits the Actions secret by hand.
