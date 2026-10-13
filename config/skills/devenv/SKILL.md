---
name: devenv
description: "Set up and use a per-repository devenv/direnv shell for tools and gates. Use when entering a repository or running its development commands."
---

# devenv shells

Use the repository's devenv shell for every tool and gate. Set up one shell per repository; do not reuse another repository's environment.

1. Check the repository root for `devenv.nix` or `.envrc`. If either exists, use that repository's shell.
2. Run `direnv allow` once per checkout. It trusts that checkout's environment configuration so direnv can load its declared tools.
3. Enter the repository and let the shell activate. The first evaluation can be cold and slow; let it finish to warm the shell before diagnosing missing tools.
4. Keep work for this repository in that shell. For a one-off command from outside the repository, or from a subshell that lacks the environment, run `direnv exec <repo> <cmd>`.
5. Before blaming `PATH`, check from inside the shell that the requested tool resolves there.
6. Find the repository's gate command in its `AGENTS.md` or `README`; run it through the shell.

A missing tool, or a stale or broken shell cache, usually points to shell setup rather than a missing system installation. Re-enter the shell or run `direnv reload`, then check tool resolution again.
