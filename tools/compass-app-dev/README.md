# compass-app-dev

Build (and launch) the Compass native shell straight from the checkout, for
local dogfooding against a remote `compass-server`. No `.app`/`.dmg`/tarball
staging: the binary compiles beside this tool and the shell serves
`apps/ui/dist` in place (via `COMPASS_ASSETS_DIR`).

Runs on **macOS** and **Linux** — the go build branches per OS. darwin links the
system WebKit framework with the platform clang (no build tag, nothing to
realize). Linux uses `-tags gtk4` and realizes the pinned GTK4/WebKitGTK
pkg-config closure + nixpkgs cc-wrapper from `tools/toolchain/gtk-e2e-env.nix`
(the same closure the gtk4 e2e CI lane and `app-bundle/build.sh` link against),
so it needs `nix` on `PATH`. Both need the repo checkout with its devenv/direnv
dev shell active.

## Tasks

```sh
moon run compass-app-dev:build   # compile the dev binary
moon run compass-app-dev:run     # build, then launch it
```

The dev loop is: `git pull` → `moon run compass-app-dev:run`.

## Pointing the app at a server

The **server connection** (URL, CA, token) is not a flag or env var — the shell
reads it from **one config file**:

- `$XDG_CONFIG_HOME/compass/app.toml`, else
- `~/.config/compass/app.toml`

Copy the template and edit it once:

```sh
mkdir -p ~/.config/compass
cp tools/compass-app-dev/app.toml.example ~/.config/compass/app.toml
$EDITOR ~/.config/compass/app.toml
```

`server_url` is required and must be an absolute `https` URL. `ca_cert` is an
optional PEM trust anchor for a server whose cert chains to a private CA (omit to
trust system roots). Unknown keys are rejected.

The **bearer token is not in the config** — you paste it once on the connect
screen and it persists in the OS keychain.

(The binary does take unrelated flags — `-assets` / `-state-dir`, and their
`$COMPASS_ASSETS_DIR` / `$COMPASS_STATE_DIR` env forms — for the UI dist and
state directory; the `run` task sets `COMPASS_ASSETS_DIR` for you. Those govern
asset/state paths, never the server connection.)

### Dogfooding an internal environment

To point at an internal dogfood `compass-server` (a `main`/`preview` env stood
up on a developer box), you need three things, all specific to that environment
and therefore kept in its own runbook, not this public repo:

- `server_url` — the env's authenticated network-door address (an `https` URL on
  the private network).
- `ca_cert` — a copy of the env's TLS trust anchor, since the door cert is minted
  from the env's own CA rather than a public root.
- the bearer token — minted by the env, pasted on the connect screen (stored in
  the keychain, never in the config file).

Concrete values for the current dogfood environments live in the tracking issue
`RIG-3103` and the platform repo's `compass-dogfood-operations` design record.
Reachability is private-network-ACL gated: an untagged personal device reaches
the door, while a tagged box needs an explicit ACL grant (managed
declaratively in the platform infrastructure repo).
