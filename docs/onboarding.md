# Getting started with Compass

Compass has two front doors, and most people should use the first one.

- **Use the app.** Install the desktop app, sign in with your own model
  subscription, and start working. Nothing to host, no server to run.
- **Self-host the stack.** Run the server, database, and agent runner yourself
  on a machine you control. Choose this when you want agent sessions on your own
  hardware, a shared install for several clients, or your own data boundary.

This guide covers the app, then self-hosting. For operational details — flags,
systemd, database options — see
[Self-hosting the Compass stack](./self-host.md).

## The app

The desktop app is the front door. It carries its own local agent runtime, so a
single machine needs no server. In embedded mode agent sessions still run in
containers, so the app requires **rootless podman** on the host. On macOS podman
runs inside a Linux VM; the app creates and starts the podman machine during
embedded preflight, and the first run downloads a VM image. Client mode needs
no local podman.

On first launch, when `app.toml` is absent and no `--mode` or
`COMPASS_APP_MODE` override is set, the app opens a chooser. Choose **Run
Compass on this computer** to use the local stack, or **Connect to a server**
to enter an HTTPS server origin, an optional CA file, and a bearer token. The
origin is a scheme, host, and optional port; a trailing `/` is removed, and any
other path, query, or fragment is rejected. The app writes `app.toml` once,
after the choice succeeds, and never rewrites an existing file. The embedded
choice runs preflight, saves embedded mode, then asks you to quit and reopen
Compass to start the stack. The server choice saves the origin and an optional
CA copy; tokenstore keeps the bearer in the OS keychain or its 0600-file
fallback.

The app is published as a per-platform release build: a `.dmg` for
Apple-silicon macOS, which you open and drag to Applications, and a `.tar.gz`
for Linux. Extract the Linux tarball and run `bin/compass-app` from inside the
extracted directory — the bundle ships the UI assets and the stack binaries
alongside it, so keep the tree intact rather than copying binaries onto your
`PATH`. Symlinking `bin/compass-app` into a directory on your `PATH` is fine.
Each release also publishes a `SHA256SUMS` file to verify what you downloaded.

On Linux you can also install the app straight from the flake, without
downloading a release:

```console
nix profile install github:RigelBuild/compass#compass-app
```

> **Note:** the first release has not been cut yet, so there is nothing to
> download today, and the flake install above currently gives you the app
> binary without its UI assets or stack binaries, so it will not launch yet.
> Both of those are being fixed. You can still bring a self-hosted stack up
> today — see the entry tier below — but running a session in it needs the app,
> so that waits on the same fix.

In local mode, once the app installs, launch it, sign in with your own model
subscription, and start working. Your subscription is the only credential in
that path; there is no Compass-hosted service.

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
| How you select it | the default | `COMPASS_RUNTIME_BACKEND=microvm` (see the bring-up note) |

**The microVM tier is the recommended shape, including for self-host.** A
microVM gives each session a separate kernel, which is the isolation boundary
Compass is designed around. The entry tier is fully supported and is the right
starting point when you do not have a KVM-capable host yet — it is a permanent
option, not a deprecated one.

Both tiers run the same stack and speak the same TLS door to clients. Moving
between them is a host change, not a data migration.

**The tier is chosen at bring-up by the `COMPASS_RUNTIME_BACKEND` environment
variable, not by the host's capabilities.** A KVM-capable host still runs the
entry tier's containers unless you ask for microVMs. The microVM tier also
needs guest images and a run root; see the
[guest image guide](./self-host-guest-image.md).

## What to run it on

Choose a host by capability, not brand. Both tiers need Linux and rootless
podman. The entry tier needs no KVM. The microVM tier needs `/dev/kvm` openable
by the stack user and the microVM userspace. Most cloud instances do not expose
`/dev/kvm`; check the [self-host prerequisites](./self-host.md#prerequisites)
before choosing a host.

## Deployment shapes

Two shapes are supported:

- **Dedicated Linux box.** The server runs on its own host and serves clients
  over TLS. See [Dedicated KVM machine](./self-host.md#dedicated-kvm-machine).
- **One box, localhost TLS.** The stack and client share a machine, and the
  server serves only the local client. See
  [One-box localhost-TLS](./self-host.md#one-box-localhost-tls).

The self-host guide describes the microVM tier. Its KVM and microVM-userspace
prerequisites are microVM-only; its installation, flags, systemd, and database
instructions apply to both tiers.

## Bringing the stack up

Install the binaries with the [recommended Nix flake](./self-host.md#nix-flake-recommended)
or a [release tarball](./self-host.md#release-tarball), then follow the
[prerequisites](./self-host.md#prerequisites) and the commands for your
[deployment shape](./self-host.md#deployment-shapes). Containers are the
default tier. Select microVMs with `COMPASS_RUNTIME_BACKEND=microvm`; that tier
also needs guest assets and a run root, covered by the
[guest image guide](self-host-guest-image.md).

The stack bundles PostgreSQL by default. To use an existing database, see
[Database](./self-host.md#database). For reboot persistence and readiness
checks, see [Running under systemd](./self-host.md#running-under-systemd).

## On a Mac

The stack itself is Linux-only, because agent sessions need KVM or rootless
podman and neither exists natively on macOS. Two supported paths:

- **Use the app** (the front door above) and let it run sessions locally. The
  app sets up the podman machine on the first embedded launch and runs sessions
  in it. This is the answer for most Mac users.
- **Point the client at a remote Linux stack.** The Mac runs the client only and
  connects over the same TLS door as any other client.

A local Linux VM on the Mac can host the stack, but the details of that path are
being settled and are deliberately not documented here yet.
