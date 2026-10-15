---
name: devenv
description: "Set up and use a per-repository devenv/direnv shell for tools and gates. Use when entering a repository or running its development commands."
---

# devenv shells

Use the repository's devenv shell for every tool and gate. Set up one shell per repository; do not reuse another repository's environment.

1. Check the repository root. With an `.envrc`, direnv activates the shell. With only `devenv.nix`, enter it with `devenv shell`, or run one command with `devenv shell <cmd> <args>`; direnv has nothing to load.
2. Before `direnv allow`, read the `.envrc` and whatever it sources: allowing runs that code with your environment and credentials. Allow it only when the checkout is one the operator asked you to work in and the code does what it says. The approval covers the current contents; when the file changes, direnv blocks it again, so review the change before re-allowing.
3. Enter the repository and let the shell activate. The first evaluation can be cold and slow; let it finish to warm the shell before diagnosing missing tools.
4. Keep work for this repository in that shell. For a one-off command from outside the repository, or from a subshell that lacks the environment, run `direnv exec <repo> <cmd>`.
5. Before blaming `PATH`, check from inside the shell that the requested tool resolves there.
6. Find the repository's gate command in its `AGENTS.md` or `README`; run it through the shell.

A missing tool usually points to shell setup, not a missing system install. `direnv reload` re-runs the `.envrc` but can reuse devenv's cached profile, so it does not repair a stale or broken profile. If tools are still missing, read the activation output for the evaluation error; fix it, or remove the repository's generated profile cache under `.direnv/` and re-enter to rebuild it.
