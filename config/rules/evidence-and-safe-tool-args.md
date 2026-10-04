---
description: "Role-invariant evidence and tool-argument safety requirements that must survive compaction."
alwaysApply: true
---

# Evidence and safe tool arguments

Ground claims in observed output; label inference and report unverified behavior.
Do not claim absence without reading the relevant section. A check is evidence
only if its subject could fail for the change.

Treat tool arguments as executable input: pass only the intended value, keep
untrusted content out of shell syntax, and do not expose secrets in commands or
output. For version-control operations, follow the `jj` skill and DL-142; do not
invent an alternate workflow.
