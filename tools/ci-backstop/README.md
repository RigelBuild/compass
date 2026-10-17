# ci-backstop

Routes failed full CI sweeps to one `ci-backstop` issue. A green full sweep
closes that issue. `core.ts` holds the pure action decision; `index.ts` reads
GitHub Actions data and applies it through `gh`.

## Run

```sh
bun tools/ci-backstop/index.ts [--dry-run]
moon run ci-backstop:ci
```

The `backstop` job in `.github/workflows/ci.yml` runs on push and schedule
results after `rollup`. `GH_TOKEN` must have issue write access. `--dry-run`
prints the action and does not create the label or change an issue.
