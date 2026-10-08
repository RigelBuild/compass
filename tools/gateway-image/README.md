# gateway-image

Pure core for building and publishing the LLM gateway image from the commit
pinned in `fork-pin.json`. It decides whether a digest-addressed publish goes
ahead, is skipped, or aborts. The `build`, `smoke`, and `publish` scripts that
`moon.yml` names are not on main yet, so only the core and its tests run today.

## Run

```sh
moon run gateway-image:ci
```

`ci` runs typecheck and tests. `.github/workflows/ci.yml` runs it in its
`moon (bun)` job. `build`, `smoke`, and `publish` are `runInCI: false` and need
their scripts before they can run.

## Credentials

None today. The core reads only `fork-pin.json` and its inputs. `moon.yml`
states that the future publish lane needs GHCR credentials; no workflow
provides them yet.

## Rotate

Nothing to rotate yet.
