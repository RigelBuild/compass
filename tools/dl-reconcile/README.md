# dl-reconcile

Reconciles the decision-file frontier with the shared counter at
`https://dl.rigel.build`. The workflow posts one claim for each validated
decision file so claimed IDs are marked as landed.

## Run

```sh
moon run dl-reconcile:check
bun run tools/dl-reconcile/index.ts
```

The `check` task validates every file under `docs/designs/decisions/` without
contacting the counter. The reconcile command runs in the `reconcile` job of
`.github/workflows/dl-reconcile.yml`, using the `main` environment.

## Credentials

`DL_CLAIM_TOKEN` is the counter's bearer token. The workflow reads it from the
`main` environment secret `DL_CLAIM_TOKEN`. Infrastructure-as-code stages the
secret. The platform repo's IaC generates one token and binds it to the counter
Worker; agent shells use the team's encrypted secret store.

## Rotate

A maintainer rotates the protected token in the platform repo's Cloudflare
stack through IaC. Replacement takes two merges: unprotect the resource, then
replace it. The apply binds the new token to the Worker and disables the old
one. Apply the stack that stages the GitHub Actions secret from the new output,
then update the dev hosts' secret store and restart agent shells.
