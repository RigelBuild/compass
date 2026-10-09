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
- **The `secretspec` CLI**, at or above 0.20.0. The server spawns it by name to
  read its secrets at boot, the master key included, so the server will not
  start without it. The nix flake's `compass-server` and `compass-stack-env`
  and the app bundles carry the pinned CLI already. The bare `compass-server`
  release binary does not, so on that shape install it yourself:

  ```sh
  brew install secretspec
  ```

  It is in `homebrew/core` with bottles for macOS arm64 and Linux (x86_64 and
  arm64). Prebuilt tarballs per platform are also published on each
  [upstream release](https://github.com/cachix/secretspec/releases).

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
pin, plus the pinned `secretspec`). Install the stack binaries and the trio together so `compass-stack up`
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

## Guest image

MicroVM agent sessions boot a guest kernel, rootfs, and initrd. With
`--runtime-backend microvm`, `compass-stack up` pulls them by digest
(`--guest-artifact`) or uses a directory you staged (`--guest-dir`, the
air-gapped path). The Runner container image carries its own baked copy. The
sources, the air-gapped runbook, and the agent-image bump flow are in
[the guest image guide](self-host-guest-image.md).

## Runner enrollment

The Runner makes up to five enrollment attempts with 1s, 2s, 4s, and 8s backoffs,
then exits non-zero. Under devenv, `restart.on = "on_failure"` restarts the
Runner. `compass-stack` does not watch the Runner after `up` returns, and `up`
attaches to a live server without starting one, so run `compass-stack down` and
then the same `compass-stack up` command to restart it.

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

The server keeps each raw token-usage event (one row per upstream model call)
for 90 days by default, and deletes older events once a day. Set the window with
`--usage-event-retention` (or `$COMPASS_USAGE_EVENT_RETENTION`) as a Go duration
such as `720h`; `0` keeps every event. The hourly and daily usage totals built
from those events are always kept.

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

When the Linear client-credentials pair and `--forge-linear-webhook-secret` are
both configured, `--public-url` (or `$COMPASS_PUBLIC_URL`) is required: it is
the public base URL of the "Open in Compass" deep links the Linear responder
posts, and the server fails to boot without it. Register the Linear webhook at
`<public-url>/webhooks/linear`. A deployment already running the Linear lane
must set it before upgrading.

### Forge write scopes

By default an agent can write to any repo the forge credential reaches. Two flags limit agent writes to granted repos:

| Flag | Environment variable | Meaning |
| --- | --- | --- |
| `--forge-enforce-scopes` | `$COMPASS_FORGE_ENFORCE_SCOPES` | `true` rejects agent forge writes outside the agent owner's grants. Default off. |
| `--forge-scope-grants` | `$COMPASS_FORGE_SCOPE_GRANTS` | Comma-separated `account:provider:host:repo` grants seeded at boot. `provider` is `github` or `linear`; `repo` is `owner/name`, a Linear team key, or `*` for every repo on the host. |

Grants name a user account. That user's agents inherit them. A rejected write looks the same as a write to a missing repo. Reads are not gated.

Seeding only inserts. Removing a grant from the flag does not revoke it. Boot seeding runs in the bootstrap tenant, so every account in the flag must be a user there; any other account fails startup.

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

## Model registry

Profiles name models by stable name, and the Server maps each name to a
provider chain. Seed the registry once after first boot, and update it later
with the same operator RPC write. No release is needed. The day-1 defaults,
the recommended model per role, and the commands are in
[the model registry guide](model-registry/README.md).

## Dashboards

The repo ships Grafana dashboard JSON for the agent's metrics and traces under
[`dashboards/`](../dashboards/README.md). Import is manual and needs your own
Prometheus and Tempo datasources; that README covers the import steps and which
signals each dashboard binds.

## Spawning an agent

The `compass` operator CLI (`github:RigelBuild/compass#compass`) brings an agent
online with one command:

```console
compass agent spawn --handle lead --parent ops
```

It creates the agent account under the caller, then provisions its container and
starts its session. It prints the agent's `owner/handle`, the session id, and
the container name. `--display-name` defaults to the handle, and `--parent`
places the agent under an existing agent in the tree.

If the handle already exists for the caller, `spawn` starts that agent as it
is; `--display-name` and `--parent` are not applied to it.

When a spawn fails with a timeout or an unavailable server, the error prints a
`--request-id`. Rerun with it to rejoin that spawn. A rerun without it fails
once the agent's container exists, because each agent has one container. If
the error says the agent already has a session or container, check it with
`compass agent status`.

Every `compass` command needs `--server-addr` (or `$COMPASS_SERVER_ADDR`) and
the admin bearer token in `$COMPASS_ADMIN_TOKEN` or a `--token-file`. The token
is never a flag. For the self-signed one-box door, pass its certificate with
`--ca`.
