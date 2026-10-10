# renovate

Holds the self-hosted Renovate configuration and the post-upgrade scripts that
keep related dependency pins and lockfiles in sync. The config groups updates
into reviewable PRs and limits which helper commands Renovate can run. A Meissa
bump relocks the `meissa` input and `refresh-biome-catalog.ts` moves the
`@biomejs/biome` catalog pin to Meissa's biome.

## Run

```sh
moon run renovate:ci
bunx renovate@44.46.2
```

The `ci` task typechecks the helpers and tests the config. The Renovate
invocation runs in the `renovate` job in `.github/workflows/renovate.yml`, with
`tools/renovate/bot-config.json5` selected as its global config.

## Credentials

- `RENOVATE_APP_CLIENT_ID` (GitHub Actions variable) and
  `RENOVATE_APP_PRIVATE_KEY` (GitHub Actions secret) authenticate the GitHub
  App token action. The job uses the `main` environment.
- The action mints a per-run installation token. The workflow passes it as
  `RENOVATE_TOKEN` to Renovate and as `GH_TOKEN` to the preflight probe.
- No persistent Renovate token or credential file is read.

## Rotate

GitHub mints a new installation token for each run; it expires after about one
hour. Infrastructure-as-code in the platform repo stages
`RENOVATE_APP_PRIVATE_KEY` and `RENOVATE_APP_CLIENT_ID`. To rotate the key,
a maintainer issues the new value and sets it in the platform repo's secrets
stack, applies that stack, then applies the stack that stages the GitHub
Actions secrets. Revoke the old key in the App settings only after the
second apply. Nobody edits the Actions secret by hand.
