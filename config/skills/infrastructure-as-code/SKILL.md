---
name: infrastructure-as-code
description: "Manage infrastructure through the repository's declarative state and deployment workflow, with previews and explicit stateful-change boundaries."
---

# Infrastructure as code

Before adopting a service or changing infrastructure, find the repository's
declarative configuration and deployment path. Prefer a reviewable change to
manual console work or one-off commands.

- Inspect the affected resource, dependencies, state, and existing conventions
  before editing. Keep credentials out of code, logs, and prompts.
- Preview the exact change with the repository's supported tool. Read the
  proposed actions and errors; do not treat a successful preview as deployment.
- Apply only through the documented deployment path and verify the resulting
  resource or service behavior with observable evidence.
- If a change requires staged applies because later state depends on an earlier
  apply, keep those changes separate and explain the dependency. Never bypass
  declarative state with an ad hoc mutation.

Provider and deployment adapters belong in user configuration or later skill
bundles. This skill defines the workflow, not provider-specific commands.
