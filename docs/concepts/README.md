# Compass concepts

Higher-level orientation for anyone — human or agent — landing in the
repository and asking "how does the agent system actually work?" These are the
load-bearing ideas the code and prompts assume you already hold. Each doc is one
concept; the tool and prompt material carry the detail, these carry the model.

## The product model

- **[Self-hosted and managed](./self-host-and-managed.md)** — Compass ships as
  two products over one shared core: the open-source self-hosted core (this
  repo, any deployer URL) and the private, commercially-licensed managed
  multi-tenant service (private monorepo, `compass.rigel.build`, reuses the
  core). Which product a change lives in, and what a design record here may
  assume.

## The org model

- **[Handles, accounts, and attribution](./handle-vs-account.md)** — a handle
  names one running agent; `mintaka` is the shared forge *account* all agents
  bill through, not a handle. How work is attributed to an agent without a
  per-agent forge seat.
- **[The persona convention](./persona.md)** — role vs persona; a persona is the
  agent's *stable* working context (repos / projects / lanes), never the
  churning per-issue detail.

## The comms model

- **[The comms model](./comms-model.md)** — threads for conversation, the
  session log for work: an agent's two surfaces and why they are split. The
  load-bearing premise any external-session integration maps onto, never
  replaces.

## The runtime

- **[Durable agents, disposable compute](./durable-agents-disposable-compute.md)**
  — the agent and its session are durable and Server-owned (Postgres + S3);
  the container/microVM it runs in is disposable and dies with the session.
  Resume rebuilds the compute and reconstructs the transcript into it.
- **[Isolation and egress](./isolation-and-egress.md)** — model-written code is
  contained, not trusted: a per-agent sandbox (container today, microVM in the
  end state) with default-deny egress and no server credential.

## The tools

- **[The agent tool set](./tools.md)** — the native tools an agent drives
  Compass through (comms, presence/roster, lifecycle), and the general flow of
  using them. Kept current as new tools land.

## The principles

- **[No human clicks](./no-human-clicks.md)** — the org (accounts, agents,
  channels, groups, subscriptions) is standupable by agents through tools; the
  only human-reserved surface is the security boundary (providing a secret's
  value for a named slot).
- **[Read-only inspection](./read-only-inspection.md)** — agents get a wide,
  shared, read-only window onto external systems (Pulumi Cloud, SaaS
  dashboards); every mutation routes through code and a merge.
- **[Review flow](./review-flow.md)** — one approval clears the review gate by
  default; the human is always the final merge gate. The multi-Manager
  "all-approve vs one-approve" UX is an open product question.
