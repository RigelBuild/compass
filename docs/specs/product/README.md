# Product Specs

Living source-of-truth for **Compass** — how it currently behaves and is
architected. The point-in-time *design records* (the why) live in the
[Compass design corpus](https://github.com/RigelBuild/docs/tree/main/docs/designs/compass/)
in `RigelBuild/docs`, bucketed by domain and indexed by
[`decisions/`](https://github.com/RigelBuild/docs/blob/main/docs/designs/compass/decisions/README.md).

Available specs:

- [`compass/`](compass/README.md) — how **Compass** currently behaves, one file
  per area: the `compass.v1` contract, server transport and lifecycle, the
  event stream, agent sessions and their relay, the communication layer, the
  agent tree, the network door, and Runner enrollment.

> These specs describe current behavior. The *why* — including Compass's full
> ADE design, much of which is designed but not yet built — lives in the design
> records in the [Compass design corpus](https://github.com/RigelBuild/docs/tree/main/docs/designs/compass/)
> (bucketed by domain, indexed by `decisions/`); each spec's "Not yet specified" section names the surfaces
> still ahead of the code.
