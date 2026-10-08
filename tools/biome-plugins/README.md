# biome-plugins

Contains the custom Grit rules loaded by the root `biome.json`. They flag fixed
`Bun.sleep` and `Bun.sleepSync` calls, plus numeric positional timeout budgets
passed to `test()` or `it()`.

## Run

```sh
moon run root:lint
```

The root lint task runs Biome with both plugins. The `.github/workflows/ci.yml`
workflow runs root lint through its `moon (bun)` job when the concern matrix
selects the root project.

## Credentials

None. Biome checks the checkout and local configuration only.

## Rotate

Nothing to rotate.
