# Review flow: when more than one manager reviews

Compass's review posture is a single review pass per PR plus the human's final
merge gate. When more than one Manager has a stake in a PR, a UX question
follows: does the PR need **every** interested Manager to approve, or **one**
approval plus comments from the rest? This doc records the question and the
current posture; it is a product/review-flow convention, not a frozen
mechanism.

## The two shapes

- **All approve.** Each interested Manager must post an approval before the PR
  is considered review-clean. Strongest signal — every stakeholder has actually
  looked — but it serializes on the slowest reviewer and can deadlock if one is
  unavailable.
- **One approves, others comment.** One Manager's approval clears the review
  gate; the other stakeholders leave comments (blocking or not) but are not each
  required to approve. Faster and deadlock-free, at the cost of a weaker "everyone
  looked" guarantee — a comment is not an approval, and a stakeholder who only
  comments has not signed off.

## Current posture

The human is **always** the final merge gate, regardless of how many Managers
review — an agent approval is a "review passed" signal, never the merge itself.
So the multi-Manager question is only about the *review-clean* signal that
precedes the human gate, not about who merges (the human, always).

The default today is **one approval clears the review gate**: the single review
pass per PR produces the review-clean signal, other stakeholders comment, and
the human merges having seen the whole thread. The "all approve" shape is
available as a stricter convention a lane can adopt when a change genuinely needs
every stakeholder's explicit sign-off — but it is opt-in, not the default,
precisely because the default must not deadlock on an unavailable reviewer.

## Open — the UX is not settled

Which shape should be the *product* default (and whether the UI should make the
choice per-PR, per-lane, or per-repo) is an open review-flow UX question, parked
here rather than decided. It is deliberately separate from forge account
identity: whether an agent's approval "counts" is a review-flow / product
question, not a question about which forge account the agent uses. Record a
decision here when the product flow settles it; until then the one-approval
default above is what ships.
