# Getting started with Compass

Compass has two front doors, and most people should use the first one.

- **Use the app.** Install the desktop app, sign in with your own model
  subscription, and start working. Nothing to host, no server to run.
- **Self-host the stack.** Run the server, database, and agent runner yourself
  on a machine you control. Choose this when you want agent sessions on your own
  hardware, a shared install for several clients, or your own data boundary.

This guide walks the first door, then the graduation path to the second. The
operational reference for a self-hosted install — flags, systemd, database
options — is [Self-hosting the Compass stack](./self-host.md); this guide is the
route to it, not a replacement.

## The app

The desktop app is the front door. It carries its own local agent runtime, so a
single machine needs no server and no container runtime.

```console
brew install --cask compass
```

Launch it, sign in with your own model subscription, and the app is ready. Your
subscription is the only credential involved; there is no Compass-hosted service
in this path.

Graduate to a self-hosted stack when you want any of:

- agent sessions running on a bigger machine than your laptop;
- several clients sharing one install;
- sessions that keep running when your laptop sleeps.

## Choosing a self-host tier

Self-hosting comes in two tiers. They differ in one thing: whether the host
gives each agent session a **microVM** or a **container**.

| | Entry tier (containers) | microVM tier (recommended) |
| --- | --- | --- |
| Session isolation | rootless container | hardware-virtualized microVM |
| Host requirement | any Linux box with rootless podman | `/dev/kvm` openable |
| Typical host | a cheap VPS | bare-metal or a nested-virt instance |

**The microVM tier is the recommended shape, including for self-host.** A
microVM gives each session a separate kernel, which is the isolation boundary
Compass is designed around. The entry tier is fully supported and is the right
starting point when you do not have a KVM-capable host yet — it is a permanent
option, not a deprecated one.

Both tiers run the same stack and speak the same TLS door to clients. Moving
between them is a host change, not a migration.

## What to run it on

Pick the host by capability, not by brand. Both tiers need a Linux host with
rootless podman available.

**Entry tier — any Linux box that can run rootless podman.** No `/dev/kvm`
needed. A small VPS is enough to start; give it enough RAM for the server, the
database container, and your concurrent sessions.

**microVM tier — a host where `/dev/kvm` is present and openable.** In practice
that means one of:

- a bare-metal or dedicated-CPU machine;
- a cloud instance type that explicitly advertises **nested virtualization**;
- a Linux workstation where your user is in the `kvm` group.

The trap worth naming: most general-purpose cloud instances do **not** expose
`/dev/kvm`, and nothing tells you until a session fails to boot. Check before you
commit to a provider — on any candidate host, `compass-stack preflight` answers
it directly.

Known to work, in no particular order and with no endorsement implied: bare-metal
and dedicated-CPU offerings from Hetzner, OVH, and Equinix Metal; nested-virt
instance types on Google Compute Engine; `*.metal` instance types on AWS EC2. Any
host meeting the capability bar above works just as well — this list is a
starting point for shopping, not a recommendation between vendors.

## Deployment shapes

Two shapes, both documented in full in [self-host.md](./self-host.md).

**Dedicated Linux box.** The stack runs on its own machine, the server binds a
routable TLS address, and clients connect from elsewhere. This is the shape for a
shared or long-lived install.

**One box, localhost TLS.** The stack and the client live on the same machine and
the server binds the loopback door. This is the evaluation and solo-use shape.
TLS still applies, so the client transport is identical to the dedicated-box
shape — only the reachable surface differs.

## Bringing the stack up

Install the binaries and the microVM userspace, then bring the stack up. The nix
flake is the recommended channel because it carries the pinned microVM userspace
with it:

```console
nix profile install \
    github:RigelBuild/compass#compass-server \
    github:RigelBuild/compass#compass-runner \
    github:RigelBuild/compass#compass-stack \
    github:RigelBuild/compass#compass-stack-env
```

A release tarball is also published per release; it does not carry the microVM
userspace. Both channels are covered in
[self-host.md](./self-host.md#installing-the-binaries).

On a microVM-tier host, check the prerequisites before the first bring-up:

```console
compass-stack preflight
```

Every check must pass on the microVM tier — it verifies `/dev/kvm`, rootless
podman, and the microVM userspace floors. A failing check names the missing
dependency and exits non-zero.

> **Entry tier:** `compass-stack preflight` currently checks the microVM
> prerequisites unconditionally, so it reports failures for `/dev/kvm` and the
> microVM userspace on a container-tier host even though that host is supported.
> Skip the preflight on the entry tier for now; a backend-aware preflight that
> reports the right verdict per tier is in progress.

Then bring it up. The stack provisions its own PostgreSQL by default, so there is
no database to install:

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --listen 0.0.0.0:50052
```

Drop `--listen` for the one-box shape; the default is `127.0.0.1:50052`. Point a
client at the server's TLS door and run a session to confirm the install.

For a stack that survives reboots, run it under systemd —
[self-host.md](./self-host.md#running-under-systemd) carries a working unit. To
use an existing PostgreSQL instead of the bundled one, see
[Database](./self-host.md#database).

## On a Mac

The stack itself is Linux-only: agent sessions need KVM or rootless podman, and
neither exists natively on macOS. Two supported paths:

- **Use the app** (the front door above) and let it run sessions locally. This is
  the answer for most Mac users.
- **Point the client at a remote Linux stack.** The Mac runs the client only and
  connects over the same TLS door as any other client.

A local Linux VM on the Mac can host the stack, but the details of that path are
being settled and are deliberately not documented here yet.
