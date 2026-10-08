# forge-linear-token

Mints a fresh Linear OAuth app-actor token for the forge live-oracle. It uses
Linear's client-credentials grant so CI does not depend on a stored token that
expires without a refresh token.

## Run

```sh
moon run forge-linear-token:ci
bun run tools/forge-linear-token/index.ts
```

The `ci` task runs typecheck and unit tests. The mint command requires GitHub
Actions because it writes the token to `GITHUB_ENV`. The `.github/workflows/ci.yml`
workflow runs it in the `forge-oracle` and `regen-forge-fixtures` jobs.

## Credentials

- `LINEAR_FORGE_CLIENT_ID` and `LINEAR_FORGE_CLIENT_SECRET`: Linear OAuth app
  credentials. The workflow supplies both from Actions secrets, which
  infrastructure-as-code stages.
- The helper masks the minted token, then appends `LINEAR_FORGE=<token>` to
  `GITHUB_ENV`. Later steps receive it as `LINEAR_FORGE`.
- `GITHUB_ENV` is provided by Actions and is not a credential.

## Rotate

The minted `LINEAR_FORGE` token is fresh for each run and expires after about
30 days, so there is no stored token to rotate. Infrastructure-as-code in the
platform repo stages both Actions secrets. To rotate the client secret,
a maintainer issues the new value and sets it in the platform repo's secrets
stack, applies that stack, then applies the stack that stages the GitHub
Actions secrets. Revoke the old secret in Linear only after the second apply.
Nobody edits the Actions secret by hand.
