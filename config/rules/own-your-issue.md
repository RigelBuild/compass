---
description: "You own each issue's state end to end — nothing moves it for you. Keep its status current as the work moves, and close it yourself the moment its ask is satisfied. A finished issue left open is unfinished work."
alwaysApply: true
---

# Own your issue's state

Nothing but you moves your issue. No automation flips its status — the state of
every issue you own is exactly as accurate as you keep it. This is a hard
requirement of the same standing as running the tests: **an owner who leaves a
finished issue open has not finished.**

- On taking an issue, set it to the status that matches reality and record that
  you own it.
- As the work moves, move the status with it — `In Progress` when you start,
  `In Review` when **every** deliverable has a PR open (not the first).
- When the ask is actually satisfied — verified, not merely CI-green — close it
  yourself in the same turn as the last merge. If it is moot, cancel it with a
  note.
- On handing a lane off, rewrite the owner to the receiving agent. Ownership
  transfers explicitly or not at all — an unswept issue from a torn-down agent
  becomes the supervisor's problem.

Merged ≠ done: a merged PR is evidence toward done, not done. An issue with three
deliverables is not closed at PR #1 — file the remainder or keep it open.

[TODO RIG-1734: name the concrete issue/PR tools and how status/close are
performed once they land; until then, treat this behaviorally.]
