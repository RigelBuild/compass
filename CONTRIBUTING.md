# Contributing to Compass

Thanks for your interest in Compass. This guide covers the development setup,
the gates a change has to pass, and the pull-request conventions.

## Development setup

Compass uses [proto](https://moonrepo.dev/proto) for language toolchains and
[devenv](https://devenv.sh) for everything else. The recommended path uses
[direnv](https://direnv.net):

```bash
direnv allow      # loads the devenv shell (toolchain on PATH)
bun install       # install the workspace JS deps
```

Without nix, install proto and let it bootstrap the pinned toolchains (go,
bun, node, moon) from the repository toolchain config, then `bun install`.

## The local gate

`moon run :ci` is the whole CI gate, run locally:

```bash
moon run :ci
```

This one task graph is the whole gate — there is no remote pipeline yet
(SEA-1507), so a green local run is the only check your change gets. It
covers, across the workspace:

- Go: `go build ./...`, `go test ./...` (the fuller `-race` / lint / vuln
  battery lands with the backend tiers).
- Contract: `buf lint`, `buf breaking`, the codegen drift gate (regenerate +
  `git diff`).
- TypeScript: `tsc`, `biome`, `bun test`, and the UI build.

Run `moon run <project>:<task>` for a single piece (e.g. `moon run
compass-go:build`). A `hk` pre-push hook runs the cheap subset (fmt, buf lint,
biome, drift) before a push; install it with the hk CLI.

## Tests

Add tests for new behavior, and add a regression test for a bug fix (it should
fail before the fix and pass after).

- Go: `go test ./...` in `go/`.
- TypeScript: `bun test` in the package.

## Changing the `compass.v1` contract

The contract is the sole, owned door between any UI and the server. Generated
clients are the only sanctioned way to reach the server — never hand-edit the
generated code under `go/gen` or `packages/compass-client/src/gen`, and never
reach the server through a raw socket or stub.

To change the contract:

1. Edit the schema under `proto/compass/v1`.
2. Regenerate: `moon run compass-proto:gen`.
3. Commit the regenerated clients alongside the schema change.

CI enforces `buf lint`, backward compatibility (`buf breaking`), and the drift
gate, so the checked-in clients always match the schema.

## Version control

The repository is git-backed. [jujutsu](https://github.com/jj-vcs/jj) (jj) works
against it too if you prefer — colocate a fresh clone with
`jj git init --colocate`.

- **Conventional Commits** for subjects: `feat(scope): …`, `fix(scope): …`,
  `refactor(scope): …`, etc. If a PR closes a tracked issue, reference it in
  the body.
- During review, add a new commit per round of feedback rather than rewriting
  pushed history.

## Pull requests

- Keep PRs focused — one feature or fix per PR.
- Include tests, and update docs when you change a public API or the contract.
- Make sure `moon run :ci` is green before you open or update a PR.

## Security

Please do **not** open a public issue for a security vulnerability. Email
[security@sealedsecurity.com](mailto:security@sealedsecurity.com) instead.

## License of contributions

By contributing, you agree that your contributions are licensed under the
project's [AGPL-3.0-only](./LICENSE) license — except contributions to the
permissively-licensed `compass.v1` protocol schema and the generated
TypeScript client (`@compass/client`), which are licensed under
`MIT OR Apache-2.0`. See the README's License section for the rationale.
