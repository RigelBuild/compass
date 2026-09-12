# Self-hosting the Compass stack

Compass ships as a small set of binaries you run yourself on a Linux host. This
guide covers the two supported deployment shapes, how to install the binaries,
how the bundled database works (and how to bring your own), and how to run the
stack under systemd.

The stack is `compass-stack up`: one command that supervises the server, a
database, and the agent runner as child processes on a KVM-capable Linux host.
The per-agent sandboxes are microVMs, so the host must expose hardware
virtualization — `compass-stack preflight` checks that before you commit to a
bring-up.

## Prerequisites

The stack runs the agent runner's sessions in microVMs and the database in a
rootless container, so the host needs:

- **A KVM-capable Linux machine.** `/dev/kvm` must be present and openable by
  the user running the stack. On a cloud VM this means a bare-metal or
  nested-virt-enabled instance type; on a workstation it means the invoking user
  is in the `kvm` group.
- **Rootless podman.** The bundled database runs as a rootless container.
- **The microVM userspace trio** at or above the pinned floors:
  cloud-hypervisor, virtiofsd, and passt. The nix flake channel provides these
  at the sanctioned pin; the release tarball assumes you supply them (they are
  packaged in most distributions).
- **The `secretspec` CLI**, at or above 0.20.0. The secrets *write* path (the
  admin `SetSecret` RPC) spawns it by name to write values into
  your configured provider. Reading secrets at boot does not need it — the
  server links the SecretSpec library directly — so a missing CLI does not stop
  the stack starting; it fails the first secret write instead. Unlike the
  microVM trio, the nix flake channel does NOT carry it — the flake's nixpkgs
  pin resolves 0.14.0, below the floor above — so install it on every shape:

  ```sh
  brew install secretspec
  ```

  It is in `homebrew/core` with bottles for macOS arm64 and Linux (x86_64 and
  arm64); on Linux the bottle pulls `dbus`, which the keyring provider needs.
  Prebuilt tarballs per platform are also published on each
  [upstream release](https://github.com/cachix/secretspec/releases). The floor
  tracks the library version the server links, so the two halves of the secrets
  path cannot drift.

Run the preflight check before your first bring-up to surface any missing
prerequisite at install time rather than mid-`up`:

```console
$ compass-stack preflight
[PASS] kvm              /dev/kvm present and openable
[PASS] podman           podman present and rootless-capable
[PASS] cloud-hypervisor reported "cloud-hypervisor v53.0.0" at/above floor 53.0.0
[PASS] virtiofsd        reported "virtiofsd 1.14.0" at/above floor 1.14.0
[PASS] passt            reported "passt 2025_09_19.623dbf6" at/above floor 2025_09_19
[PASS] secretspec       reported "secretspec 0.20.0" at/above floor 0.20.0
```

A failing check prints a `[FAIL]` line naming the missing or below-floor
dependency and exits non-zero, so it is safe to gate an install script on.

> **Note:** `compass-stack preflight` ships its own minimal checks. Once the
> runtime lane's microVM support gate lands, these host-level checks defer to it;
> until then they are the install-time prerequisite surface.

## Deployment shapes

### Dedicated KVM machine

The recommended shape for a shared or production install: a dedicated
KVM-capable Linux host runs `compass-stack up`, the server binds a TLS door, and
clients connect from other machines over that door.

- The host exposes `/dev/kvm` and runs the full stack.
- The server listens on a routable address with a TLS certificate.
- Clients elsewhere connect over TLS and run agent sessions in the host's
  microVMs.

Point the listen address at the host's routable interface when bringing the
stack up:

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --listen 0.0.0.0:50052
```

### One-box localhost-TLS

The single-machine shape for evaluation or solo use: the stack and the client
live on the same box, and the server binds the loopback TLS door.

- Everything runs on one KVM-capable machine.
- The server binds `127.0.0.1:50052` (the default listen address), reachable
  only from the same host.
- TLS still applies on loopback, so the client's transport is identical to the
  dedicated-machine shape — only the reachable surface differs.

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest
```

No `--listen` flag is needed; the default `127.0.0.1:50052` is the one-box door.

## Installing the binaries

The stack is a set of binaries — `compass-stack`, `compass-server`, and
`compass-runner` — plus the microVM userspace trio, shipped through two
channels. On the default database path postgres runs as a pinned container, so
no `compass-postgres` binary is installed; it is only needed on the dev-path
(`--postgres-image ""`), which runs the `compass-postgres` wrapper on `PATH`
instead of a container.

### Nix flake (recommended)

The flake tracks `main` and builds each binary as its own package, plus
`compass-stack-env` (the cloud-hypervisor/virtiofsd/passt trio at the sanctioned
pin). Install the stack binaries and the trio together so `compass-stack up`
resolves `compass-server` and `compass-runner` on `PATH` and the trio is present
for the microVM boot:

```console
nix profile install \
    github:RigelBuild/compass#compass-server \
    github:RigelBuild/compass#compass-runner \
    github:RigelBuild/compass#compass-stack \
    github:RigelBuild/compass#compass-stack-env
```

### Release tarball

Each release publishes a platform tarball of the same binaries. Download,
extract, and place them on `PATH`:

```console
$ curl -fsSL https://github.com/RigelBuild/compass/releases/latest/download/compass-stack-linux-amd64.tar.gz \
    | tar -xz -C /usr/local/bin
```

The tarball does not carry the microVM userspace trio; install cloud-hypervisor,
virtiofsd, and passt from your distribution and confirm the floors with
`compass-stack preflight`.

## Database

By default the stack provisions its own PostgreSQL as a bundled rootless
container — you do not install or manage a database yourself. `compass-stack up`
starts it, `compass-stack down` stops it, and its data lives under the stack's
state directory. This is the zero-configuration path, matching the dominant
self-host convention (a bundled database out of the box, a documented opt-out for
operators who run their own).

To use an existing PostgreSQL instead, pass its DSN and the stack skips the
bundled container entirely:

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --database-external \
    --database 'postgres://user:pass@db.internal:5432/compass'
```

With `--database-external` the stack only connects to the `--database` DSN you
name; it never starts, stops, or owns that instance's lifecycle. The flag is the
opt-out switch and `--database` (or `$COMPASS_DATABASE_DSN`) carries the DSN.

## Secrets

Compass keeps its secret *values* in your configured `secretspec` provider, not
in its own database. At boot the server declares the secret names it needs for
the features you enabled and resolves each one from the provider. A declared
name that resolves with no value fails startup rather than running degraded, so
enabling a feature and forgetting its secret is a boot failure you see at once,
not a silent gap. The name declaration and the resolve error live in
`declareServerSecretNames` (`go/server/serve.go`) and `SpecResolver.Resolve`
(`go/internal/secrets/resolver.go`).

One prefixing trap is worth stating plainly. The forge secret names you
configure are resolved with `SERVER_` prepended (`serverSecretName`,
`go/server/serve.go`). A name you configure as `FOO` is stored in the provider
as `SERVER_FOO`. Set the provider value under the prefixed name.

### Master key

`COMPASS_MASTER_KEY` encrypts every user secret at rest. You must provision it
before the first boot; compass never generates it, and boot fails closed when it
is absent, empty, the wrong length, or not hex.

It is a 32-byte key, written as 64 hex characters. Generate one with:

```bash
openssl rand -hex 32
```

Set that value under the name `COMPASS_MASTER_KEY` in your provider. Unlike the
forge secrets it already carries the reserved `COMPASS_` prefix, so it is
fully qualified and is not re-prefixed with `SERVER_`.

The first boot records a salted, non-secret fingerprint of the key. A later boot
with a different key fails startup before it touches any stored data, because
proceeding would make every existing secret undecryptable. Rotation is versioned
re-encrypt machinery, never a raw overwrite of this value.

Losing the master key loses every stored secret. There is no recovery path.
Store it where you will not lose it and where a changed value cannot be
overwritten by accident.

### Forge secrets

The forge integration adds up to six more secrets. Each row below is a *name*
you choose (via the flag or its environment variable), whose *value* you then
set in the provider under the `SERVER_`-prefixed spelling. Compass declares a
name only when the feature that needs it is configured, so you only provision
the rows for the lanes you run.

| Flag | Holds | Required when |
| --- | --- | --- |
| `--forge-app-key-secret` | Primary GitHub App PEM private key | GitHub App is configured (`--forge-app-id` set) |
| `--forge-app-webhook-secret` | Primary App webhook signing secret | GitHub App is configured |
| `--forge-reviewer-app-key-secret` | Reviewer GitHub App PEM private key | Reviewer App is configured (`--forge-reviewer-app-id` set) |
| `--forge-linear-client-id` | Linear OAuth client id | Set, together with the client secret |
| `--forge-linear-client-secret` | Linear OAuth client secret | Set, together with the client id |
| `--forge-linear-webhook-secret` | Linear webhook signing secret | Set |

Each flag also reads an environment variable when the flag is unset:
`$COMPASS_FORGE_APP_KEY_SECRET` and so on, one per row.

Set every name you intend to use. The two Linear rows have built-in default
names in the code, but those apply only when compass looks a value *up* --
they do not switch the Linear lane on, so do not rely on them. Compass
declares the pair only when both names are set explicitly, by flag or by
`$COMPASS_FORGE_LINEAR_CLIENT_ID` / `$COMPASS_FORGE_LINEAR_CLIENT_SECRET`
(`declareServerSecretNames` gates on the raw config, `go/server/serve.go`).
Set a provider value and no flag and Linear stays off silently:
`buildLinearTokenSource` returns no token source, and its half-configured
warning needs exactly one of the two to resolve, so neither resolving logs
nothing at all.

### Choosing a provider

The right `secretspec` provider depends on your deployment shape. On a box an
operator uses directly, a keychain-backed provider keeps the values in the
platform keyring. On a server or cloud deployment, point `secretspec` at your
cloud secret manager so the values live in managed storage rather than on the
host.

## Running under systemd

Wrap `compass-stack up` in a systemd unit so the stack starts on boot and
restarts on failure. `up` brings the stack to ready and returns without
blocking; `--linger` leaves the children running after the `up` process exits,
so the unit is a `Type=oneshot` with `RemainAfterExit=yes` and a matching
`down` on stop:

```ini
[Unit]
Description=Compass self-host stack
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --listen 0.0.0.0:50052 \
    --linger
ExecStop=/usr/local/bin/compass-stack down \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest
Restart=on-failure
RestartSec=5
# The stack needs KVM and rootless podman; run it as a dedicated,
# non-root user that is a member of the kvm group.
User=compass

[Install]
WantedBy=multi-user.target
```

Install and start it:

```console
sudo systemctl daemon-reload
sudo systemctl enable --now compass-stack
```

Check readiness with the stack's own status command:

```console
compass-stack status \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest
```
