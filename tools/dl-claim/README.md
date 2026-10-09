# dl-claim

Claims design-ledger IDs (`DL-NNN`) for new `docs/designs/decisions/<area>/DL-NNN.md` files
from the shared counter at `https://dl.rigel.build`. The counter hands out each
ID once, so concurrent PRs never pick the same number. The
`dl-reconcile` workflow marks claimed IDs as landed after the PR merges.

## Run

```sh
bun tools/dl-claim --ref RIG-1234 --lane <your-branch>
bun tools/dl-claim --ref none --lane <your-branch> --count 3
```

- `--ref`: the tracker issue (`RIG-<n>`) or `none`.
- `--lane`: your branch name. It identifies your claims in `/status`.
- `--count`: 1 to 10 IDs (default 1).

It prints one `DL-NNN (claimed YYYY-MM-DD)` line per ID. Put each ID in its
row's `ID` cell, in the same PR as the record.

If the claim fails after the request was sent, the IDs may already be used.
Do not rerun. List your lane's claims instead:

```sh
curl -H "Authorization: Bearer $DL_CLAIM_TOKEN" 'https://dl.rigel.build/status?repo=compass'
```

## Credentials

`DL_CLAIM_TOKEN` (env): the counter's bearer token. The tool exits early
with an error if it is empty.

- Agent shells on the team's dev hosts get it from the hosts' encrypted secret
  store.
- CI: the `dl-reconcile` workflow reads it from the `main` environment
  secret `DL_CLAIM_TOKEN`. Infrastructure-as-code stages that secret; nobody
  pastes it by hand.

One token covers every partition. Infrastructure-as-code in the platform repo
generates it and binds it to the counter Worker.

## Rotate

The token is a protected generated value in the platform repo's Cloudflare
stack. A maintainer rotates it there through IaC:

1. Replace the generated token resource. It is protected, so this takes two
   merges: unprotect, then replace. The apply binds the new value to the
   Worker, and the old token stops working.
2. Apply the stack that stages the GitHub Actions secret. It reads the new
   stack output and updates the `main` environment secret.
3. Update the dev hosts' secret store entry and restart agent shells.

Between steps 1 and 3, claims made with the old token fail with HTTP 401
("Token missing or wrong"). Nothing is minted on a 401, so retry once the new
token is in place.
