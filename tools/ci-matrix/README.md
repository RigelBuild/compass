# ci-matrix

Builds the GitHub Actions concern matrix from Moon's project list, CI-group
tags, and affected-project results. It emits special-leg flags and JSON package
root arrays for the pgtest and microVM CI legs.

## Run

```sh
bun run tools/ci-matrix/index.ts
moon run ci-matrix:ci
```

The first command generates the matrix and prints a summary. In CI, the
`.github/workflows/ci.yml` `setup` job runs it and publishes the matrix through
`GITHUB_OUTPUT`. The `ci` task runs typechecking and tests.

## Credentials

None. The generator reads workflow metadata and local Moon and Git data; it does
not read a token or call an authenticated service.

## Rotate

Nothing to rotate.
