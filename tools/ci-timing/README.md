# ci-timing

Measures GitHub Actions run and job timing for the `ci.yml` workflow by run
class. It reports wall time, runner wait, job duration, skip rate, long pole,
runner minutes, and UTC hours with long waits.

```sh
bun tools/ci-timing/index.ts --since 2026-10-01 [--until 2026-10-15] [--json]
moon run ci-timing:ci
```

The CLI uses the authenticated GitHub CLI to read workflow runs and their job
metadata. It does not read logs or change repository state.
