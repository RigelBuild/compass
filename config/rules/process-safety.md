---
description: "When stopping or killing a process, use only an explicit PID for a process you started this turn; ask if you cannot identify one."
---

# Process safety

Never use `pkill`, `killall`, `kill -1`, `kill -9 -1`, or another broad or pattern-matching process kill. A Manager and its in-process subagents share one container, so a broad or wrong-PID kill can stop a sibling's work.

Kill a process only when you know its specific PID and started it this turn. If you cannot name that PID or confirm you started it, ask before killing anything. Processes you did not start this turn are off-limits.
