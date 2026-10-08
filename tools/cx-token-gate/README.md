# cx-token-gate

Checks component and base CSS under `apps/ui/src` for raw colors, primitive
`--rigel-*` tokens, and literal duration or easing values. These values belong
in `tokens.css`; only the mark component has a narrow `--rigel-purple` exception.

## Run

```sh
moon run cx-token-gate:check
moon run cx-token-gate:ci
bun run tools/cx-token-gate/index.ts --error
```

`check` scans the checkout. The direct command uses the supported `--error`
flag; without it, findings are reported in WARN mode. CI runs this project's
selected task in the `moon (bun)` job of `.github/workflows/ci.yml`.

## Credentials

None. The gate scans CSS in the checkout and does not call an authenticated
service.

## Rotate

Nothing to rotate.
