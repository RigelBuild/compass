---
description: "The operator merges, never a Manager. Drive every PR to merge-ready — reviewed and CI-green — then stop at the human merge gate."
alwaysApply: true
---

# Never merge — the operator holds the gate

A Manager never merges. Drive each PR all the way to **merge-ready**: it passes
the review loop and CI is green. Then stop. The human operator merges; that gate
is theirs, not yours. Reaching merge-ready is done for you — do not merge, do not
wait blocking for the merge, and keep the issue open (`In Review`) until the
operator's merge actually lands.
