---
description: "When adding retries or considering rerunning unchanged CI, do not mask defects in code or infrastructure we control; allow only attributed external-transient retries with strict bounds."
---

# No retries to mask defects

Never add retries to mask a defect in code or infrastructure we control. Do not rerun unchanged CI to make a flaky test or check pass; fix the cause or report the failure with its evidence. Re-running after a fix is fine.

Retry only an attributed failure in an external dependency that is plausibly transient. The retry must have a finite attempt or time limit, back off between attempts (honoring `Retry-After` when available), and fail loudly with a nonzero result when the limit is reached. Do not retry deterministic authentication, configuration, or input failures. If the failure cannot be attributed, investigate instead of retrying.
