---
name: ci-failure-triage
description: "Classify failed checks from their actual logs, separate change failures from environment failures, and verify a fix in a reproducing environment."
---

# CI failure triage

Read the failing check's logs before changing code. Identify the failing task and
its first relevant error; a summary or status alone is not evidence.

1. Classify the failure as caused by this change, unrelated to it, or uncertain.
2. Classify it as a code defect, environment/permission gap, or infrastructure
   failure. Name the evidence for the classification.
3. Reproduce the failure in the matching environment when possible. Do not hide
   it with retries, ignored failures, or broader permissions.
4. After a fix, run the smallest relevant check and the affected acceptance
   checks. Report the observed result and any unverified part.

CI-engine adapters may add log retrieval and check decoding. Keep this core
engine-neutral; do not infer success from one status surface when another is
available.
