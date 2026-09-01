# Product Specs

Living source-of-truth for **Compass** — how it currently behaves and is
architected. The point-in-time *design records* (the why) live in the
[design corpus](../../designs/) (`../../designs/`), bucketed by domain and indexed by
[`DECISIONS.md`](../../designs/DECISIONS.md).

Available specs:

- [`compass.md`](compass.md) — how **Compass** currently behaves: the
  `compass.v1` UI/backend contract (the owned door, generated-client-only
  access, contract-drift gate), the `compass-server` local transport (UDS
  dual-protocol, no-TCP posture, single-instance startup, serving lifecycle,
  dev endpoint), and the event-stream resubscribe semantics.

> These specs describe current behavior. The *why* — including Compass's full
> ADE design, much of which is designed but not yet built — lives in the design
> records under [`../../designs/`](../../designs/) (bucketed by domain, indexed by
> `DECISIONS.md`); each spec's "Not yet specified" section names the surfaces
> still ahead of the code.
