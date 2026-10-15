---
description: "Before reporting code changes as done, run format, lint, and relevant tests for the affected area in the repository's devenv shell and report their results."
---

# Pre-finish checks

Before reporting a code change as done, run formatting, lint, and tests for the affected area in the repository's `devenv` shell (see the `devenv` skill). Prefer the repository's aggregate gate when it covers all three.

- Format the changed files, lint the affected area, and run tests that exercise the change. Run only tests added or changed unless broader coverage is requested.
- A green result covers only cases that ran. Report the per-case result line for the case that exercises the change; a suite summary alone is not evidence that it ran.
- State explicitly which checks ran and passed. If a check did not run or failed, say so. Keep unrelated formatting churn with the change that caused it.
