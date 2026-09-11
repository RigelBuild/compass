# Runtime Specs

Living source-of-truth for the **Compass runtime tier strategy** — which
runner backends exist, what each isolates, and how a user adopts them. The
point-in-time *design records* (the why) live in the
[design corpus](../../designs/) (`../../designs/`), bucketed by domain and
indexed by [`DECISIONS.md`](../../designs/DECISIONS.md).

Available specs:

- [`runner-tiers.md`](runner-tiers.md) — the runner tier strategy: the
  trust-model axis (DL-325), the three tiers (host / podman / microVM) with
  each tier's isolation boundary and egress posture, the adoption funnel from
  host tier through embedded-local to self-host graduation, and the standing
  guidance on when each tier is (and is not) the right choice.

> These specs describe the strategy and current behavior. The *why* — the
> rulings behind the trust-model split and each tier — lives in the design
> records under [`../../designs/`](../../designs/) (bucketed by domain,
> indexed by `DECISIONS.md`); each spec's "Not yet specified" section names
> the surfaces still ahead of the code.
