# Why a coding agent doesn't split into a brain and hands

This is the long-form rationale behind one rejection in the Compass hosted
agent-platform design ([the elastic session runtime record](design.md), RIG-1717):
we seriously considered splitting the agent into a cheap "brain" tier and a
heavy "hands" tier, and we walked away from it. The design record states the
decision in a paragraph. This document is the full argument, because the split
is an attractive idea that will be proposed again, and the reasons it fails are
worth writing down once, properly.

The short version: a persistent brain/hands split is the right architecture for
a narrow class of agent, and an interactive coding agent is not in that class.
The workloads it does fit already have their split, in a form that predates
agents entirely. And the problems people reach for the split to solve on a
coding agent are solved more cleanly by two mechanisms that have nothing to do
with topology.

## The pattern

The idea is clean, which is why it keeps getting proposed. An agent spends most
of its time reasoning: calling a model, reading code, editing files. That work
is light and I/O-bound. Occasionally it needs to do something heavy, like
compile a project or run a test suite. That work is CPU-bound and wants a big
machine.

So you split the agent in two. The **brain** runs on a small, cheap box: the
model loop, the edit logic, a view of the filesystem. The **hands** run on a big
box, spun up on demand when a heavy operation arrives and torn down after. A
shared virtual filesystem connects them so the hands see the edits the brain
made. You reach for expensive compute only when a task needs it and keep
thousands of idle brains packed cheaply the rest of the time.

The split keeps getting proposed for coding agents specifically, and coding
agents are exactly where it fails.

## The one principle

A persistent split into a cheap tier and a heavy tier pays off only when **both**
of these hold:

1. The cheap tier is genuinely cheap.
2. The heavy work is rare and cleanly isolatable into a separate tier.

The first condition is the load-bearing one. If the cheap tier isn't actually
cheap, you have saved nothing by peeling it off, and the topology is pure
overhead. The second condition decides whether a standing heavy tier earns its
keep or sits idle between bursts of work. A candidate workload has to clear both,
and you check it against them directly. The test is not "coding versus
non-coding" or "our agent versus theirs." It is these two conditions.

## Coding fails both conditions

**The cheap tier isn't cheap enough to be worth peeling off.** The honest version
of this argument has to concede something first: you *can* run a coding agent on
a stripped-down brain. Several of the strongest coding agents shipping today work
mostly from grep and raw file text, with no resident language server, and they
are very good. So "the brain must be a full IDE" is not true, and an essay that
rested on it would fall over in one sentence.

But look at what a "cheap" coding brain still needs. It needs the materialized
source tree, because you cannot grep or edit files you do not have. It needs git.
For anything beyond trivial edits it wants the language toolchain and a language
server, which is the direction we expect the frontier to move and which Compass
specifically depends on. So the cheap tier is "a box with the whole
repo checked out and the model loop running." The heavy tier is "a box with the
whole repo checked out and the toolchain running." The delta you actually peel
off by splitting is the toolchain and some CPU headroom, and to get it you
duplicate the working tree across two boxes or share it over a network volume,
which brings back the tree-transfer problem the split was supposed to avoid.
That is a thin saving bought with a standing topology and a distributed
filesystem. The cheap tier is cheap enough to run; it is not cheap enough that
carving it onto its own box pays for the machinery of carving it.

**The heavy work can be isolated, but not profitably into a standing tier.** It
is tempting to say compiles and tests can't be separated from editing, but that
overstates it, and this design refutes it directly: the elastic session runtime
*does* run a compile in a separate, transient environment that shares the
session's storage. So isolation is
possible. The question condition 2 really asks is whether a *standing* heavy
tier earns its cost, and for coding it does not, because the heavy work is woven
through the inner loop rather than gathered into a phase. The loop is: edit,
compile, read the error, edit, compile, run the one failing test, edit. A
standing hands tier either sits reserved and mostly idle between those compiles,
or runs as a warm pool that pays a dispatch and tree-sync cost on every
iteration of the loop. Either way you pay continuously for topology, and because
condition 1 already failed you did not save enough on the brain to cover it.

The way to get compute-on-demand without that bill is to make the extra capacity
**temporal rather than topological**: grow the session's own environment for the
duration of a heavy op, or burst to a transient environment that shares the
session's storage, then reclaim it. That is a burst, not a split. It differs from
a standing hands tier in two ways that matter: it is transient, so you pay for
big compute only while the op runs, and it presumes the brain is already a
complete environment holding the session's state, so it adds capacity instead of
relocating the workload. The single real asymmetry a coding agent has, that a
heavy op wants more compute than the inner loop, is met by the burst. The split's
standing second tier is cost without a matching benefit.

That is enough to reject the split for Compass. But it is worth following the
idea out to the rest of the agent market, because the pattern of where it fits
turns out to be sharp, and instructive.

## The rest of the market

Take the other big category: a general computer-use or knowledge-work agent that
books travel, fills spreadsheets, drives a CRM. This agent really is mostly
reasoning, so the cheap brain looks like a fit. It fails the split for the
opposite reason coding does. Its heavy compute is not local. When it does
something expensive it asks someone else's servers to do it, a Sheets
recalculation or a Salesforce query over HTTP. There is no heavy *local* tier to
split off; what runs on the agent's own machine is a browser or an API client
plus reasoning, which is one modest environment. You cannot peel off a tier that
lives on a SaaS backend.

So coding has no cheap tier worth peeling, and computer-use has no heavy local
tier to peel. What passes both conditions is the workload in between: an agent
that mostly reasons and occasionally dispatches heavy compute that is
**self-hosted** rather than delegated to a SaaS backend. That workload is real,
and naming it honestly is what makes the argument hold. Media transcoding and
rendering, HPC simulation, genomics pipelines, EDA regression farms, and
self-hosted GPU training all have the shape: a light glue tier that assembles a
job, and heavy compute that is genuinely rare, long-running, batch-isolatable,
and local. Condition 1 holds because assembling an `ffmpeg` command or a Nextflow
run or an `sbatch` script is cheap glue work, not a dev environment. Condition 2
holds because the heavy job is a discrete batch with a clean start and end, not
something woven through a loop.

The split fits those workloads. But they already have it. Every one of them runs
behind a job scheduler that predates agents by decades: Slurm and its cousins for
HPC, a render-farm manager for media, Spark for data jobs, a batch queue for GPU
training. The scheduler *is* the brain/hands split, already built, already
operated, already the interface those shops use. An agent working in that world
submits to the scheduler exactly the way a human does. It does not need, and
gets nothing from, a new brain/hands agent runtime underneath it. So the split
does not serve an empty set. It serves the set of workloads that already own
their split, where the right move for an agent is to use the one that exists. For
an interactive coding agent, which is what Compass is, it serves no one.

The one genuine remainder is a local-only application with no cloud backend and
no batch scheduler: legacy desktop software, air-gapped tools, some CAD front
ends. There the answer is still not a split. It is a computer-use agent in one
sandboxed environment with a display, reasoning and application colocated. There
is no heavy isolatable tier to carve out, just an app and an agent driving it.

## The honest steelman, and why it isn't a split

There is one real motivation left, and it deserves a straight answer. You
genuinely do want model-written code to run inside a hardware or OS isolation
boundary. The model writes code, you run it, and you do not fully trust it. That
instinct is correct.

But it argues for a **sandbox, not a split**. Isolation is a per-session
boundary: this session's code runs inside a box that cannot reach other tenants
or the host. It says nothing about whether reasoning and execution live in one
environment or two. A session sandbox delivers the isolation completely.
Splitting reasoning onto a separate box adds nothing on top of the sandbox except
a filesystem-sync problem you did not have before. Conflating "I need isolation"
with "I need a split" is the trap: the two feel related because both draw a
boundary, but they draw different boundaries, and only the sandbox is
load-bearing.

One caveat keeps this honest. A heavy op does run code the agent did not author:
a package's post-install script, a `build.rs`, a test dependency. That is a
distinct untrusted actor inside the session, and it is fair to ask whether a
split contains it. It does not. Whatever that code produces flows back into the
working tree the next reasoning step reads, and the tree is shared across a split
just as it is within a session, so the split does not quarantine the one thing
that actually crosses back. What contains an untrusted build is the session
sandbox plus default-deny egress and credentials scoped down to what the specific
op needs, all of which this design already does, and none of which is a
reasoning/execution topology. The design does draw intra-session asymmetries
where they buy something, giving a burst environment only the just-in-time
credentials its op requires and keeping the sole forge-write credential on the
server. Those are credential-scoping decisions, not a brain/hands split, and they
are the right shape for the supply-chain threat.

## The two rescues collapse into simpler mechanisms

The split gets reached for to solve two real problems on a coding agent. Both
have cleaner answers.

Security isolation, keeping untrusted model-written code away from the reasoning
and the credentials, is delivered by sandboxing the whole session against other
tenants and the host, as above. No intra-session brain/hands boundary is needed,
because inside one session there is nothing to separate that a credential scope
does not already handle.

Fleet density, not holding a big environment open for every idle session, is
delivered by suspending idle sessions. When a session goes quiet, evict its whole
environment and reclaim the resources; the durable state is the session
transcript plus a persistent volume, and on the next activity you relaunch and
resume. An idle session then costs storage rather than a reserved machine. You
get the density without splitting the running session at all.

## And the shared filesystem goes with it

The brain/hands split usually arrives with a companion: a content-addressed
virtual filesystem, with Merkle-root snapshots and a "sync to this hash" transfer
primitive, so the hands box can materialize the tree the brain box has been
editing. It is elegant engineering, and we rejected it too, because it exists to
solve a problem the colocated model does not have. The VFS moves a filesystem
between two machines. Once reasoning and compute share one environment, and a
burst environment shares the session's volume instead of receiving a synced copy,
there is no cross-machine tree to transfer, and the primitive has nothing to do.

The reflexive objection is that giant monorepos need lazy, content-addressed
materialization, and that is true, but it does not put the burden back on us to
build one. Three cases cover the ground. At the largest scale, organizations
already run their own virtual filesystem or use git's sparse-checkout, because
operating at that size forces you to build it long before an agent shows up;
Compass interops with a customer's VFS behind a thin source-of-tree seam. The
asset-heavy middle, a game studio or an LFS-heavy repo that is too big for a
comfortable full checkout but has not built its own EdenFS, is served by
sparse-checkout and git partial clone, which need no filesystem of ours. And the
platform's own concern, thousands of hosted sessions each checking out a copy of
the same mid-size repo, is answered by snapshotting a repo's materialized tree
once and starting later sessions from the snapshot, the Codespaces prebuild
model, behind the same seam. None of the three requires Compass to build a
content-addressed filesystem, and the scale where one would genuinely pay is the
scale that already owns it.

## What we built instead

One environment per session, fusing the agent, the toolchain, the language
servers, and the working tree. It is sized for the inner loop by default and
grows only for the duration of a heavy operation. Its storage lives on a
persistent volume that survives suspend, resume, and eviction. Idle sessions are
suspended rather than carved apart. The isolation boundary is the session
sandbox, a hardware-virtualized microVM in the end state, facing other tenants
and the host rather than walling reasoning off from execution inside a single
tenant's session.

Every real need the split reached for on a coding agent is met without it:
isolation by the session sandbox, density by suspend-idle, and burst headroom by
growing one environment in time. The full design, its seams, and its constraints
are in [the elastic session runtime record](design.md).
