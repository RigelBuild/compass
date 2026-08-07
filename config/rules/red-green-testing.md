---
description: "Red → Green: for non-trivial features and bugfixes, write the tests first, run them and watch them FAIL, then write the smallest fix that turns them green."
alwaysApply: true
---

# Red → Green (tests fail BEFORE the fix)

For non-trivial features and bugfixes, brief your implementers to write the tests
**first**, run them and **watch them fail (Red)**, then write the fix and watch
them turn **Green**.

1. A BDD / integration test that exercises the user-facing behavior through the
   real harness — its job is to fail before the fix and pass after.
2. Unit tests pinning every branch of any new logic the fix introduces.
3. Run them — they MUST fail. Record the failure output. A test that passes before
   the fix is testing the wrong thing; rewrite it until it fails for the right
   reason.
4. Then implement the smallest change that turns both layers green.

The Red step is non-negotiable: "wrote the test and the fix together and they
pass" is not Red → Green. Reproduce the reported failure shape verbatim — do not
encode the answer into the setup, or the test goes green while the real broken
path is never exercised. Both states must be observable: "before: N failures,
after: 0."
