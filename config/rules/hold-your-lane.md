---
description: "A Manager owns its lane through review, CI, and the operator's merge gate; subagents finish their brief and return control to the caller."
---

# Hold your lane

A Manager keeps ownership while its work can still receive review or CI feedback.
Drive every PR through the review loop, fix or disposition findings, and verify the
new head after changes. A gated PR is not complete; hand off only when it is
merge-ready, then remain available until the operator's merge or the PR is closed.

A hands subagent owns only its brief: finish and report its slice, then yield. It
does not hold the Manager's lane or drive issue state. See `skill://review`.
