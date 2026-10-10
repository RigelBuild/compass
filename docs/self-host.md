# Self-hosting the Compass stack

Compass ships as a small set of binaries you run yourself on a Linux host. This
guide covers the two supported deployment shapes, how to install the binaries,
how the bundled database works (and how to bring your own), and how to run the
stack under systemd.

The stack is `compass-stack up`: one command that supervises the server, a
database, and the agent runner as child processes on a Linux host. Agent
sessions run in containers by default or in microVMs when selected. The
[prerequisites](#prerequisites) describe shared and microVM-only requirements.

## Prerequisites

Both tiers need Linux and rootless podman for the bundled database. They also
need the `secretspec` CLI, at or above 0.20.0.

- **Rootless podman.** The bundled database runs as a rootless container.
- **MicroVM tier only:** `/dev/kvm` must be present and openable by the stack
  user, and the host needs the microVM userspace trio at or above its pinned
  floors: cloud-hypervisor, virtiofsd, and passt. The nix flake supplies the
  trio; the release tarball expects you to install it from your distribution.
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
dependency and exits non-zero. On the microVM tier, this is a useful install
gate before `up`.

The check includes microVM-specific probes even on the container tier. Missing
`/dev/kvm` or microVM userspace is expected there; rootless podman and
`secretspec` are still required for either tier.

> **Note:** `compass-stack preflight` ships its own minimal checks. Once the
> runtime lane's microVM support gate lands, these host-level checks defer to it;
> until then they are the install-time prerequisite surface.

## Deployment shapes

### Dedicated KVM machine

The recommended shape for a shared or production install: a dedicated
KVM-capable Linux host runs `compass-stack up`, the server binds a TLS door, and
clients connect from other machines over that door.

- The microVM tier needs `/dev/kvm`; entry-tier containers do not.
- The server listens on a routable address with a TLS certificate.
- Clients elsewhere connect over TLS and run agent sessions in the host's
  container or microVM, depending on the selected tier.

Point the listen address at the host's routable interface when bringing the
stack up. The LLM gateway has no default image, so supply
`--gateway-image <image>@sha256:<hex>` or `--gateway-external`.

The example below selects the entry tier's default containers. To use microVMs
instead, pass `--runtime-backend microvm` and configure guest assets and a run
root as described in the [guest image guide](self-host-guest-image.md).

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --gateway-image <gateway-image>@sha256:<hex> \
    --listen 0.0.0.0:50052
```

### One-box localhost-TLS

The single-machine shape for evaluation or solo use: the stack and the client
live on the same box, and the server binds the loopback TLS door.

- The microVM tier needs KVM-capable hardware; the entry tier does not.
- The server binds `127.0.0.1:50052` (the default listen address), reachable
  only from the same host.
- TLS still applies on loopback, so the client's transport is identical to the
  dedicated-machine shape — only the reachable surface differs.

```console
$ compass-stack up \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --gateway-image <gateway-image>@sha256:<hex>
```

No `--listen` flag is needed; the default `127.0.0.1:50052` is the one-box door.
The LLM gateway has no default image, so supply `--gateway-image` or
`--gateway-external` for either deployment shape.

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

### Release binaries

Each release attaches the linux-amd64 binaries and a `SHA256SUMS` file.
Download the three stack binaries for a tag, check them, and place them on
`PATH`:

```console
$ tag=vX.Y.Z   # a release that ships compass-stack
$ base=https://github.com/RigelBuild/compass/releases/download/$tag
$ for b in compass-stack compass-server compass-runner; do
    curl -fsSLO "$base/${b}_${tag}_linux-amd64"
  done
$ curl -fsSL "$base/SHA256SUMS" | sha256sum --check --ignore-missing
$ for b in compass-stack compass-server compass-runner; do
    sudo install -m 0755 "${b}_${tag}_linux-amd64" "/usr/local/bin/$b"
  done
```

The release does not carry the microVM userspace trio; install cloud-hypervisor,
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
then exits non-zero. A cold `up` waits up to 15 seconds for the Runner to enroll
and fails if it does not; `up` that attaches to a live server skips that wait.
After `up` returns, `compass-stack` does not watch the Runner. Under devenv,
`restart.on = "on_failure"` restarts the Runner. To restart it manually, run
`compass-stack down`, which stops every child including the Runner, and then the
same `compass-stack up` command.

### Kubernetes projected-token enrollment

Configure the Server with `--runner-clusters` or `$COMPASS_RUNNER_CLUSTERS` and
point the Runner at its projected token with `--token-file` or
`$COMPASS_RUNNER_TOKEN_FILE`. A cluster entry uses the following schema:

```yaml
clusters:
  - name: prod-eks
    issuer: https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE
    audience: compass-runner
    namespace: compass-runners
    serviceAccount: compass-runner
    maxTokenLifetime: 600s
```

The rendered Runner's namespace, `serviceAccount: compass-runner`, audience, and
`tokenExpirationSeconds` / `maxTokenLifetimeSeconds` must match the Server
cluster entry. A token lifetime above the Server's `maxTokenLifetime` fails
authentication. The admission policy denies exec, attach, and ephemeral
containers on Runner pods except to a break-glass group (`admission.breakGlassGroups`,
default `system:masters`, the cluster-admin group).

`audience` and `maxTokenLifetime` show their defaults. By default the Server
finds keys through OIDC discovery on `issuer`. Set at most one of `jwksURI` or
`jwksFile` to override that, and set `caFile` for an issuer with a private CA.

Every cluster must have a unique issuer. The Server selects a cluster by the
token's `iss`; sharing an issuer across clusters is rejected. Reusing one
signing key does not stop the Server starting, but every cluster sharing it
fails closed when tokens are verified.

The default Kubernetes configuration binds `system:service-account-issuer-discovery`
to `system:serviceaccounts`, so the cluster's discovery document and JWKS
require authenticated access. An out-of-cluster Server can reach the keys by
choosing one of three options: bind that role to `system:unauthenticated` with
anonymous authentication enabled; publish JWKS at a reachable
`--service-account-jwks-uri`; or configure a local `jwksFile`, which requires
a Server rollout for every key rotation.

During a live issuer change, keep the old value as a second
`--service-account-issuer` until tokens from both issuers expire. Publish a new
signing key at least one hour before using it. Set `deployers` to the identity
that applies the Runner manifests: Flux's kustomize-controller, or the
ServiceAccount a Kustomization impersonates. Narrow `controllers` to the single
username used by the controller manager when it is known.

The projected-token DaemonSet currently supports one node only. Rollout beyond
one node waits for the multi-Runner hub.

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

Agent forge writes require a scope grant by default. The opt-out is for a single-trust-domain Dogfood deployment only:

| Flag | Environment variable | Meaning |
| --- | --- | --- |
| `--forge-disable-scope-enforcement` | `$COMPASS_FORGE_DISABLE_SCOPE_ENFORCEMENT` | `true` disables enforcement. Default `false` enforces grants; disable it only for single-trust-domain Dogfood. |
| `--forge-scope-grants` | `$COMPASS_FORGE_SCOPE_GRANTS` | Comma-separated `account:provider:host:repo` grants seeded at boot. `provider` is `github` or `linear`; `repo` is `owner/name`, a Linear team key, or `*` for every repo on the host. |

Grants name a user account. That user's agents inherit them. A rejected write looks the same as a write to a missing repo. Reads are not gated.

#### Agent workstream repositories

Use `compass agent repo add <agent> <org/name>`, `remove <agent> <org/name>` and `list <agent>` to manage an agent's GitHub repositories. The target is a bare agent handle or `owner/agent`. Writes require the owning user's own token; tenant administrators cannot change another user's agent grants. Each repository must be an exact `org/name` in the agent's credential org. The GitHub App must be configured.

Agents created later by an agent inherit its repository rows. A grant or remove signals the affected live agent to refresh its credential. After remove, the old token may still reach the removed repository until it expires, up to one hour.

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
    --gateway-image <gateway-image>@sha256:<hex> \
    --listen 0.0.0.0:50052 \
    --linger
ExecStop=/usr/local/bin/compass-stack down \
    --state-dir /var/lib/compass \
    --image ghcr.io/rigelbuild/compass-agent:latest
Restart=on-failure
RestartSec=5
# The microVM tier needs KVM and a dedicated user in the kvm group.
# For the entry tier, rootless podman is enough.
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
    --image ghcr.io/rigelbuild/compass-agent:latest \
    --gateway-image <gateway-image>@sha256:<hex> \
    --listen 0.0.0.0:50052
```

This reports the server, not Runner health. It is not a read-only probe: if the
stack is not running, `status` starts it instead of reporting it down. See
[Runner enrollment](#runner-enrollment) for Runner startup and recovery.

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
compass agent spawn --handle lead --role owner --parent ops
```

`--role` is required and must be `supervisor`, `owner`, or `manager`; it selects
the agent's block-0 prompt. Use `--persona-file <path>` to add an optional
free-text identity overlay that is baked into the agent at provision.

It creates the agent account under the caller, then provisions its container and
starts its session. It prints the agent's `owner/handle`, the session id, and
the container name. `--display-name` defaults to the handle, and `--parent`
places the agent under an existing agent in the tree.

If the handle already exists for the caller, `spawn` starts that agent as it
is; `--display-name`, `--parent`, `--role`, and `--persona-file` are not applied
to it.

When a spawn fails with a timeout or an unavailable server, the error prints a
`--request-id`. Rerun with it to rejoin that spawn. A rerun without it fails
once the agent's container exists, because each agent has one container. If
the error says the agent already has a session or container, check it with
`compass agent status`.

Every `compass` command needs `--server-addr` (or `$COMPASS_SERVER_ADDR`) and
the admin bearer token in `$COMPASS_ADMIN_TOKEN` or a `--token-file`. The token
is never a flag. For the self-signed one-box door, pass its certificate with
`--ca`.

## Graduating from the local app

The desktop app's embedded mode runs a local stack for onboarding and local
development. A laptop is not an always-on host, so for a stack that keeps
running, move to client mode: run `compass-stack up` on an always-on box (one
of the [deployment shapes](#deployment-shapes)) and point the app at it.

Graduation is a config edit, not an in-app flow. The app never rewrites an
existing `app.toml`, so edit it yourself. The file is
`$XDG_CONFIG_HOME/compass/app.toml`, or `~/.config/compass/app.toml` when
`XDG_CONFIG_HOME` is unset. Quit the app, then replace its contents with:

```toml
mode = "client"
server_url = "https://compass.example.com"
# Only for a private-CA or self-signed door; an absolute path to the PEM.
ca_cert = "/home/you/.config/compass/server-ca.pem"
```

`server_url` must be an `https` origin. Omit `ca_cert` when the server's
certificate chains to the system roots. Unknown keys are rejected, so a typo
fails at launch with a config error. Reopen the app: with no stored bearer for
that server, it shows the token screen, and the accepted token is kept in the
OS keychain (or a 0600-file fallback).

The local stack's sessions and database stay on the laptop and are not
migrated to the server. To return to embedded mode, set `mode = "embedded"` and
remove `server_url` and `ca_cert`, since embedded mode rejects both. Once the
file is in client mode, `--mode embedded` or `COMPASS_APP_MODE=embedded` runs
the local stack for one launch without editing it again.

The embedded-to-client end-to-end test that drives the app against a live
stack is still pending, so this path is documented but not yet covered by the
app's e2e suite.
