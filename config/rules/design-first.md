---
description: "Non-trivial or forked work gets a design record PR first — reviewed and operator-merged — before any implementation begins."
alwaysApply: true
---

# Design first

Non-trivial or forked work starts with a **design record**, not code. Write the
record, open it as its own PR, run it through the review loop, and let the
operator merge it. Only then dispatch implementation against the frozen record.
The merged record is the contract implementers build to; a plausible approach in
your head is not one. When the ask is small and unambiguous, skip straight to
implementation — this gate is for the changes whose shape is in question.
