---
description: "Role-invariant evidence and tool-argument safety requirements that must survive compaction."
alwaysApply: true
---

# Evidence and safe tool arguments

Ground claims in observed output; label inference and report unverified behavior.
Do not claim absence without reading the relevant section. A check is evidence
only if its subject could fail for the change.
For source claims, cite code by symbol and path, or documentation by section
heading and path; include a short quote that supports the claim. Before counting
search results, pin them to the intended tree and revision. For current edits,
name the working copy explicitly and use a positive control with the same filter.

Treat tool arguments as executable input: pass only the intended value, keep
untrusted content out of shell syntax, and do not expose secrets in commands or
output. For version-control operations, use the `vcs_*` tools as the `jj` skill
describes (DL-558); do not invent an alternate workflow.
