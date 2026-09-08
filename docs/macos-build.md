# Building the Compass native app on macOS

Notes for building `go/cmd/compass-app` on a Mac, producing the `.app`/`.dmg`,
and knowing what the result can and cannot do. Every command here mirrors a
command the repo already runs; the citation after each one is where to verify
it.

## Status: what works and what does not

The darwin lane is **compile + unit test + ad-hoc sign + bundle**. It is not a
working stack.

| Thing | State | Evidence |
| --- | --- | --- |
| Compiling the shell on darwin | Works. Bare `go build ./cmd/compass-app` selects the real entrypoint on darwin (no build tag). | `go/cmd/compass-app/main.go:1` |
| Producing a signed, mountable `.dmg` | Works. Ad-hoc signature only (`codesign --sign -`), not Developer ID, not notarized. | `tools/macos-bundle/index.ts:273-276` |
| CI coverage on darwin | Compile, ONE unit test, bundle, mount assertion. There is no functional stack test. | `.github/workflows/ci.yml:1469-1471`, `:1639-1645`, `:1661-1701` |
| Embedded mode (the app supervising its own stack) | **Does not work on macOS.** Do not expect a running stack from a Mac build. | see below |
| `nix profile install .#compass-app` on a Mac | **No such output.** The flake's `systems` list is `x86_64-linux` only. | `flake.nix:33`, `flake.nix:124-127` |
| The darwin podman-machine preflight check | **Absent, not failing.** Its `MachineReady` adapter is unwired. | `go/internal/preflight/preflight.go:117`, `go/cmd/compass-app/embedded.go:378-384` |
| x86_64 (Intel) darwin | Not covered. CI runs `macos-14` (arm64) and the bundle is named `darwin-arm64`. | `.github/workflows/ci.yml:1471`, `:1651` |

Why embedded mode does not work on macOS, concretely:

- The darwin preflight adapter set passes only `GOOS`, `PodmanRootless`,
  `PodmanVersion`, and `ImagePresent`
  (`go/cmd/compass-app/embedded.go:379-384`). `MachineReady` is therefore
  `nil`, and the machine check runs only when
  `d.GOOS == "darwin" && d.MachineReady != nil`
  (`go/internal/preflight/preflight.go:117`). So on a Mac with no podman
  machine, preflight reports **clean** and the failure surfaces later, deeper,
  and less legibly.
- The supervised stack cannot start on darwin at all, independently of the
  above. It records a start-time identity token for every child it spawns,
  read from `/proc/<pid>/stat` (`go/internal/stack/pgidfile.go:353-357`),
  which darwin does not have. The comment states the intent plainly: "on a
  non-Linux unix this reader fails and up refuses"
  (`go/internal/stack/pgidfile.go:337-339`), and the read is unconditional on
  the child-spawn path (`go/internal/stack/stack.go:451-454`, reached for
  postgres, server, and runner at `:361`, `:298`, `:326`). A correctly
  provisioned, running podman machine does not fix this.

These are two separate blockers, and the second is the harder one. The macOS
podman-machine work (spike, then the `MachineReady` adapter and the init/start
ensure step) is task T-6 of the embedded-revival record, still open
(`docs/designs/ui/compass-native-embedded-revival/design.md:661-684`). T-6 does
**not** cover the `/proc` reader: its scope is machine provisioning only. That
fix is a separate two-site swap, already chosen in
`docs/designs/platform/compass-stack-supervision/design.md:304-316` — give the
identity token a darwin reader via `sysctl KERN_PROC`, at both the spawn site
(`go/internal/stack/pgidfile.go:340`) and the teardown site
(`go/internal/stack/adapters/groupsignal.go:99`), sharing one `uint64`
encoding. Both blockers must land before a Mac runs an embedded stack.

So: build on a Mac to check the compile and the bundle. Use a Linux box, or
client mode against a remote `compass-server`, to actually run Compass.

## Prerequisites

| Need | Why | Symptom if missing |
| --- | --- | --- |
| Xcode command line tools (clang + the system SDK) | The shell is a cgo build; CI sets `CGO_ENABLED: '1'` and the darwin shell links the SYSTEM WebKit framework rather than a gtk closure (`.github/workflows/ci.yml:1559`, `:1474-1476`; `flake.nix:124-127`). The design record names Xcode CLT as the requirement for the Cocoa/cgo shell (`docs/designs/infra/ci/compass-local-dev/design.md:323-324`). | `go build` fails in the cgo/link step, not the Go compile step. |
| Go at the repo pin | The pin is `1.27.1` (`tools/toolchain/versions/go.nix:10`). The module's own floor is `go 1.26.0` (`go/go.mod:15`) and tracks the pin minus at most one minor (`tools/toolchain/versions/go.nix:8-9`). | An older toolchain fails with `file requires newer Go version`. |
| bun at the repo pin | `1.4.0` (`tools/toolchain/versions/bun.nix:3`); it runs the bundler (`.github/workflows/ci.yml:1652`) and, through moon, the UI build (`apps/ui/moon.yml:24-26`). The pin ships an `aarch64-darwin` asset (`tools/toolchain/versions/bun.nix:13-16`). | `bun: command not found`, or a version-skewed UI dist. |
| nix (recommended) | It is how the pinned toolchain is resolved. CI installs nix on `macos-14` and puts the `gate-tools.nix` `langs` store paths on `PATH` (`.github/workflows/ci.yml:1505-1537`); those attrs are bun/node/moon/go plus the Go analysis battery (`tools/toolchain/gate-tools.nix:100-109`). | Nothing breaks structurally, but you are building with an unpinned toolchain. |
| direnv + devenv (the dev shell) | `.envrc` auto-loads the devenv shell and watches the four version pins (`.envrc:6`, `.envrc:11-14`, `.envrc:16`). Run `direnv allow` once. The shell's `packages` list guards only the Linux-specific entries behind `pkgs.stdenv.isLinux` (`devenv.nix:190`, `:199`, `:217`, `:237`), and `toolchain-tools.nix` carries `aarch64-darwin` legs "so macOS developers get the same pinned versions" (`tools/toolchain/toolchain-tools.nix:4-5`). | `go`/`nix` not on `PATH`; the dev loop names that case explicitly (`tools/compass-app-dev/index.ts:54-56`). |
| podman + a podman machine | Only if you attempt to RUN embedded mode. Preflight probes `podman info` and `podman image exists` (`go/cmd/compass-app/preflight_adapters.go:24-25`, `:51-53`), and enforces a podman ≥ 4.3 floor (`go/internal/runtime/podman.go:493-494`, `:504-505`). On macOS podman runs inside a Linux VM (`go/internal/preflight/preflight.go:29-31`). | See "Running it" below — this is where the honest answer is "not yet". |

The dev shell is **not** darwin-complete for everything: `xvfb-run` and
`chromium` are Linux-only by construction (`devenv.nix:190`, `:199`), so the
GTK4 e2e and the UI dev-smoke gate are Linux lanes, not Mac ones.

## Build the app locally

These mirror the `darwin` CI job step for step
(`.github/workflows/ci.yml:1539-1645`). Run them from the repo root with the
dev shell active.

Version stamp, the same dev shape CI uses (`.github/workflows/ci.yml:1607-1610`):

```bash
dev="$(cat version.txt)+g$(git rev-parse --short HEAD)"
echo "$dev"
```

`version.txt` is `0.1.0` today (`version.txt:1`). CI fails loud when it is
missing or empty (`.github/workflows/ci.yml:1608`); this is the same guard
`app-bundle/build.sh:57-58` carries.

Compile the shell. No build tag: darwin selects the real entrypoint via
`//go:build (linux && gtk4) || darwin` (`go/cmd/compass-app/main.go:1`), and the
CI comment says the same (`.github/workflows/ci.yml:1612-1614`):

```bash
CGO_ENABLED=1 go -C go build -trimpath \
  -ldflags "-X main.version=$dev" \
  -o /tmp/compass-app ./cmd/compass-app
```

Grounded at `.github/workflows/ci.yml:1615-1617`. Failure path: with no Xcode
CLT this dies in the cgo toolchain, not the Go front end. With `-tags gtk4`
added by mistake it would try to link the GTK closure, which is Linux's alone
(`flake.nix:124-127`).

Compile the three sidecars — pure Go, cgo off
(`.github/workflows/ci.yml:1619-1626`):

```bash
for b in compass-stack compass-server compass-runner; do
  CGO_ENABLED=0 go -C go build -trimpath \
    -ldflags "-X main.version=$dev" \
    -o "/tmp/$b" "./cmd/$b"
done
```

Run the darwin-tagged unit test. This is the only CI lane that executes it: the
moon `compass-go:test` lane runs untagged (`go test -race ./...`,
`go/moon.yml:163`) and on CI's Linux runners untagged selects the
`linux && !gtk4` stub (`go/cmd/compass-app/main_nogtk4.go:1`) instead of
`main_test.go` (`//go:build (linux && gtk4) || darwin`), while the gtk4 e2e
lane `-run E2E` filters it out (`.github/workflows/ci.yml:1628-1636`, `:1455`).
On your Mac the stub does not apply: `GOOS=darwin` satisfies that tag with no
tags set, so a plain `moon run compass-go:test` does run this test locally. The
explicit command below is what CI runs, and what to use when scripting:

```bash
CGO_ENABLED=1 go -C go test -trimpath \
  -run 'TestDistDirForExecutable' -count=1 -v \
  ./cmd/compass-app/
```

Grounded at `.github/workflows/ci.yml:1639-1641`. It defends the `.app`
dist-resolution contract: under a `Contents/MacOS` executable the resolver
returns `Contents/Resources/dist`, else `dist` beside the binary
(`.github/workflows/ci.yml:1633-1635`; the test is
`go/cmd/compass-app/main_test.go:18`). Failure path worth knowing: `-run` alone
exits 0 when it matches nothing, so CI additionally greps for the PASS line and
reds on a rename or skip (`.github/workflows/ci.yml:1637-1645`). Do the same if
you script this.

Build the UI dist the bundle stages (`.github/workflows/ci.yml:1648`):

```bash
moon run compass-ui:build
```

That runs `bunx vite build` and outputs `dist` (`apps/ui/moon.yml:24-26`).

### Arch reality

- CI builds **arm64 only**: `runs-on: macos-14`
  (`.github/workflows/ci.yml:1471`), and the artifact is named
  `/tmp/compass-app-darwin-arm64.dmg` (`.github/workflows/ci.yml:1651`).
- The `.app` targets macOS 11.0 as its `LSMinimumSystemVersion`, described in
  the bundler as "the minimum macOS the arm64 shell targets"
  (`tools/macos-bundle/index.ts:69-70`).
- **x86_64 darwin is not covered** — not by the runner, and not by nix
  (`flake.nix:33` lists `x86_64-linux` alone). Nothing in this repo builds or
  tests an Intel Mac shell. UNVERIFIED whether the compile happens to work
  there; nothing exercises it.
- There is no cross-compile path. The shell links system frameworks via cgo, so
  it "cannot cross-compile from ubuntu"
  (`.github/workflows/ci.yml:1473-1476`).

### The convenience dev loop

For local dogfooding against a **remote** server there is a registered moon
project instead of the raw commands above (`tools/compass-app-dev/moon.yml:31-47`):

```bash
moon run compass-app-dev:build
moon run compass-app-dev:run
```

It branches per OS: darwin gets no build tag and realizes no nix closure
(`tools/compass-app-dev/dev-core.ts:14-24`, `:60-65`), Linux gets `-tags gtk4`
plus the pinned GTK closure. It builds with `-trimpath` and no release
`-ldflags`, so the binary keeps `main.go`'s default version
(`tools/compass-app-dev/dev-core.ts:28-47`). It sets `COMPASS_ASSETS_DIR` to
`apps/ui/dist` for you (`tools/compass-app-dev/dev-core.ts:119-124`). Both
tasks are `runInCI: false` (`tools/compass-app-dev/moon.yml:34-37`, `:44-47`).

This lane is client-mode dogfooding, not embedded mode. It does not change
anything in the status table.

## Produce the `.dmg`

The bundler is `tools/macos-bundle/index.ts`. Its flag grammar is exactly five
flags (`tools/macos-bundle/index.ts:139-145`):

| Flag | Required | Meaning |
| --- | --- | --- |
| `--binary <path>` | yes (`:173`) | The built darwin `compass-app` binary (`tools/macos-bundle/index.ts:47-48`). |
| `--dist <dir>` | yes (`:177`) | The UI dist directory, `apps/ui/dist` (`tools/macos-bundle/index.ts:49-50`). |
| `--version <semver>` | yes (`:178`) | Stamped into `Info.plist` (`tools/macos-bundle/index.ts:51-52`). |
| `--out <dmg>` | yes (`:179`) | Path the `.dmg` is written to (`tools/macos-bundle/index.ts:53-54`). |
| `--sidecar <path>` | no, repeatable | Binaries staged beside the shell in `Contents/MacOS`, in flag order; zero is legal and yields a shell-only `.app` (`tools/macos-bundle/index.ts:55-64`, `:156-157`). |

There are no other flags. Anything else is rejected as
`unknown or misplaced argument` (`tools/macos-bundle/index.ts:149-151`).

The CI invocation (`.github/workflows/ci.yml:1651-1659`):

```bash
out=/tmp/compass-app-darwin-arm64.dmg
bun run tools/macos-bundle/index.ts \
  --binary /tmp/compass-app \
  --dist apps/ui/dist \
  --version "$dev" \
  --sidecar /tmp/compass-stack \
  --sidecar /tmp/compass-server \
  --sidecar /tmp/compass-runner \
  --out "$out"
```

What it does, in order (`tools/macos-bundle/index.ts:221-284`):

1. Asserts the binary, the dist's `index.html`, and every sidecar exist, before
   staging anything (`:226-230`).
2. Stages `Compass.app/Contents/{MacOS,Resources}` beside the requested `.dmg`
   (`:234-240`).
3. Copies the shell to `Contents/MacOS/compass-app` and `chmod +x` (`:243-247`);
   each sidecar to `Contents/MacOS/<basename>` (`:252-256`); the dist to
   `Contents/Resources/dist` (`:260`).
4. Writes `Contents/Info.plist` with name `Compass`, identifier
   `build.rigel.compass` (`:263-271`).
5. `codesign --sign - --force --deep` — ad-hoc, mandatory on Apple Silicon;
   real Developer-ID signing and notarization are not done here (`:273-276`).
6. `hdiutil create -volname Compass -srcfolder <stage> -ov -format UDZO <out>`
   (`:281`).

Failure paths that are already handled loudly, so you can trust the exit code:

- A flag with no value, a duplicate single-valued flag, or a missing required
  flag throws naming the flag (`tools/macos-bundle/index.ts:152-172`).
- A sidecar whose basename is `compass-app`, or two sidecars with the same
  basename, are rejected — they would silently clobber each other in
  `Contents/MacOS` (`tools/macos-bundle/index.ts:193-207`).
- A missing input path throws `... not found at <path>` before any staging
  (`tools/macos-bundle/index.ts:212-219`).

### Verify the `.dmg`

Mirror the CI assertion (`.github/workflows/ci.yml:1661-1701`):

```bash
mnt="$(mktemp -d)"
hdiutil attach "$out" -mountpoint "$mnt" -nobrowse -readonly
for want in compass-app compass-stack compass-server compass-runner; do
  "$mnt/Compass.app/Contents/MacOS/$want" --version
done
ls "$mnt/Compass.app/Contents/Info.plist" \
   "$mnt/Compass.app/Contents/Resources/dist/index.html"
hdiutil detach "$mnt"
```

CI checks each of those four binaries is executable, exits 0 on `--version`,
and prints a string containing the dev version
(`.github/workflows/ci.yml:1669-1686`), then that `Info.plist` and
`Resources/dist/index.html` exist (`:1687-1696`). `--version` is a real
pre-flag-parse path in the shell (`go/cmd/compass-app/version.go:9-11`,
`:25-31`), so it works with no display.

The Linux side has no equivalent `.dmg` path: `app-bundle/build.sh` produces
`compass-app-<version>-linux-amd64.tar.gz` and nothing else
(`app-bundle/build.sh:2-3`, `:61`, `:145`), driven by
`moon run compass-app-bundle:build` (`app-bundle/moon.yml:34`, `:40`).

## Config and env

Flags on `compass-app`, with their env fallbacks. Resolution order is flag, then
env, then the listed default.

| Flag | Env | Default | Cited |
| --- | --- | --- | --- |
| `--socket` | `$COMPASS_SOCKET` | `$XDG_RUNTIME_DIR/compass/server.sock`, else `$HOME/.compass/server.sock` | `go/cmd/compass-app/main.go:61-65`, `:344-350` |
| `--assets` | `$COMPASS_ASSETS_DIR` | a `.app`'s `Contents/Resources/dist`, else `dist` beside the executable | `go/cmd/compass-app/main.go:66-69`, `:314-317` |
| `--state-dir` | `$COMPASS_STATE_DIR` | `$XDG_STATE_HOME/compass`, else `$HOME/.compass` | `go/cmd/compass-app/main.go:70-73`, `go/cmd/compass-app/client.go:60-66` |
| `--mode` | `$COMPASS_APP_MODE` | `app.toml`, else `embedded` | `go/cmd/compass-app/main.go:74-76`, `:330` |
| `--compass-stack` | `$COMPASS_STACK_BIN` | a `compass-stack` sibling of the executable, else `compass-stack` on `$PATH` | `go/cmd/compass-app/main.go:77-80`, `go/cmd/compass-app/embedded.go:305-322` |
| `--image` | `$COMPASS_AGENT_IMAGE` | `ghcr.io/rigelbuild/compass-agent:latest` | `go/cmd/compass-app/main.go:81-83`, `go/cmd/compass-app/embedded.go:43`, `:361-368` |
| `--version` / `-version` | — | prints the stamped version and exits | `go/cmd/compass-app/version.go:9-11`, `:25-31` |

The agent image default is locked at
`go/cmd/compass-app/embedded.go:43`; the app does not bundle it, and
`compass-stack` pulls it from GHCR at first run
(`go/cmd/compass-app/embedded.go:40-42`).

The **server connection is not a flag or an env var.** It comes from one file:
`$XDG_CONFIG_HOME/compass/app.toml`, else `~/.config/compass/app.toml`
(`go/internal/appconfig/appconfig.go:233-238`). An absent file is not an error;
it resolves to embedded mode (`go/internal/appconfig/appconfig.go:177-179`).
Unknown keys are rejected (`:96-98`). `mode = "client"` requires an absolute
`https` `server_url` (`:133-135`); `ca_cert` is an optional PEM trust anchor.
A template lives at `tools/compass-app-dev/app.toml.example`. The bearer token
is never in the file — it is pasted on the connect screen and lives in the OS
keychain (`tools/compass-app-dev/app.toml.example:18-19`).

On a Mac, `$XDG_STATE_HOME` and `$XDG_RUNTIME_DIR` are usually unset, so the
defaults land at `$HOME/.compass` and `$HOME/.compass/server.sock`
(`go/cmd/compass-app/client.go:66`, `go/cmd/compass-app/main.go:350`).

### microVM knobs: not applicable on darwin

`compass-runner`'s `--microvm-*` flags and their `$COMPASS_MICROVM_*` env forms
(`go/cmd/compass-runner/main.go:296-317`) belong to the `microvm` backend, which
is opt-in: `SelectBackend` defaults an empty or `"podman"` backend to the podman
CLI (`go/internal/runtime/microvm.go:117-125`). The microVM preflight requires
`/dev/kvm` to be openable (`go/internal/runtime/microvm_preflight.go:86-89`),
which a Mac does not have. The embedded pipeline never passes a backend either:
`stackUpArgs` sends only `--state-dir`, `--image`, `--socket`
(`go/cmd/compass-app/embedded.go:142-150`), and `runnerSpec` passes no backend
flag (`go/internal/stack/spec.go:48-54`). **Ignore the microVM knobs on macOS.**

## Running it, and what will happen

`--mode` resolution defaults to **embedded** when `app.toml` is absent
(`go/cmd/compass-app/main.go:74-76`), and embedded launch runs
preflight → `compass-stack up` → `WhoAmI` in that order, short-circuiting on
the first failure (`go/cmd/compass-app/embedded.go:111-131`), all before the
window opens (`go/cmd/compass-app/main.go:255-262`) under a 180 s bring-up
budget (`go/cmd/compass-app/main.go:47`).

Preflight, in order, on a Mac (`go/internal/preflight/preflight.go:83-138`):

| # | Check | On darwin | Severity |
| --- | --- | --- | --- |
| 1 | `os` — must be `linux` or `darwin` | Passes. Detail on failure: `embedded mode runs on linux or darwin, this host is <goos>` (`:87-90`). | fatal |
| 2 | `podman` — `podman info` answers | Fails if podman is absent or the machine is down; detail is `rootless podman is required: <err>` (`:95-98`, adapter `go/cmd/compass-app/preflight_adapters.go:24-32`). | fatal |
| 3 | `podman-version` — ≥ 4.3 | Uses the runner's own copy verbatim: `podman N.N or newer is required …` (`:106-110`, `go/internal/runtime/podman.go:515-518`). | fatal |
| 4 | `machine` — podman machine ready | **Never runs.** Gated on `d.GOOS == "darwin" && d.MachineReady != nil` (`:117`), and `realPreflight` supplies no `MachineReady` (`go/cmd/compass-app/embedded.go:379-384`). | would be fatal |
| 5 | `image` — agent image in the local store | Usually fails on a fresh box, by design; logged at Warn, never fatal (`:129-135`, `go/cmd/compass-app/embedded.go:415-417`). | advisory |

The consequence is the thing to internalize: **with no podman machine, check 4
is absent rather than red.** If podman's CLI answers, checks 1-3 pass, the
advisory image warning is logged, preflight returns clean
(`go/cmd/compass-app/embedded.go:408-422`), and the launch proceeds to
`compass-stack up` — where it fails downstream and opaquely. The start-time
identity read (`/proc/<pid>/stat`,
`go/internal/stack/pgidfile.go:353-357`) is not merely one such downstream
failure — it is an independent refusal. `up` records that token for every
spawned child (`go/internal/stack/stack.go:451-454`), so it fails on darwin
even with a healthy, running podman machine.

### Manual podman machine steps — manual until T-6

If you want to try anyway, provision the machine by hand first:

```bash
podman machine init
podman machine start
podman machine ls
```

These are the commands T-6 will script and then productize behind the
`MachineReady` adapter and an init/start ensure step
(`docs/designs/ui/compass-native-embedded-revival/design.md:663-680`). The
onboarding doc already tells users to do this by hand and says provisioning it
from the app "is not yet implemented" (`docs/onboarding.md:20-23`). Until T-6
lands, the app will neither check nor create the machine for you.

Provisioning the machine will still not give you a running stack. The `/proc`
identity read above is unaffected by it, and T-6 does not cover that fix.

`podman machine init` also has no sanctioned memory/disk floor recorded yet —
setting one is T-6's job
(`docs/designs/ui/compass-native-embedded-revival/design.md:399-402`). Treat the
defaults as UNVERIFIED for a three-image cold start.

To avoid embedded mode entirely, write `mode = "client"` into `app.toml` with a
`server_url`, per `tools/compass-app-dev/app.toml.example`. Client mode opens
the window immediately with no pre-window probe
(`go/cmd/compass-app/main.go:271-278`).

The bearer token is stored in the macOS Keychain through go-keyring, with a
file fallback when no backend answers
(`go/internal/tokenstore/keyring_integration_test.go:3-5`). That darwin leg is
behind the `keyring_integration` build tag and no CI lane sets it, so treat it
as supported but unexercised.

### There is no macOS smoke runbook

`app-bundle/SMOKE.md` is the packaged-bundle acceptance runbook, and it is
Linux-only: it is scoped to the client tarball on "one dev box"
(`app-bundle/SMOKE.md:1-8`, `:16-20`) and contains no macOS or darwin section
at all. The embedded-revival record expects that to change only "after T-6"
(`docs/designs/ui/compass-native-embedded-revival/design.md:659`).

## Known blockers

| Blocker | Evidence | What unblocks it |
| --- | --- | --- |
| Darwin `MachineReady` preflight adapter is unwired, so the podman-machine check is absent rather than failing | `go/internal/preflight/preflight.go:29-34`, `:117`; `go/cmd/compass-app/embedded.go:376-384` | T-6: the darwin adapter plus the init/start ensure step (`docs/designs/ui/compass-native-embedded-revival/design.md:672-676`) |
| No nix flake output for darwin; `nix profile install .#compass-app` does not work on a Mac | `flake.nix:33`; `flake.nix:124-127` | Grow `systems` and add a darwin branch that links system WebKit via frameworks instead of the gtk closure (`flake.nix:124-127`) |
| No functional darwin CI test — the lane compiles, runs one unit test, and bundles | `.github/workflows/ci.yml:1473-1479`, `:1639-1645`, `:1661-1701` | A live-stack darwin lane; today's `macos-14` runner is a compile+bundle sweep only (`.github/workflows/ci.yml:1477-1479`) |
| Embedded stack cannot start on darwin: the process identity token reads `/proc/<pid>/stat` | `go/internal/stack/pgidfile.go:337-339`, `:353-357`; `go/internal/stack/stack.go:451-454` | A darwin `sysctl KERN_PROC` reader at both identity sites — spawn (`go/internal/stack/pgidfile.go:340`) and teardown (`go/internal/stack/adapters/groupsignal.go:99`) — sharing one `uint64` encoding (`docs/designs/platform/compass-stack-supervision/design.md:304-316`). Not in T-6's scope |
| No macOS section in `app-bundle/SMOKE.md` | `app-bundle/SMOKE.md:1-8`, `:16-20` (no macos/darwin/dmg match anywhere in the file) | T-6, which owns "SMOKE.md part (a) executed on a Mac" (`docs/designs/ui/compass-native-embedded-revival/design.md:683-684`) |
| arm64 only; Intel macOS is not built or tested | `.github/workflows/ci.yml:1471`, `:1651`; `flake.nix:33` | An `x86_64-darwin` runner and bundle target; none exists today |
| `.dmg` is ad-hoc signed, not Developer-ID signed or notarized | `tools/macos-bundle/index.ts:273-276` | The real signing task named there as T4 |

## Open / unverified

- Whether the darwin compile succeeds on **x86_64** macOS. Nothing in the repo
  builds it (`.github/workflows/ci.yml:1471`, `flake.nix:33`).
- The `podman machine init` memory and disk floors a Compass cold start needs.
  Recorded as T-6's job, not yet set
  (`docs/designs/ui/compass-native-embedded-revival/design.md:399-402`).
- Whether the stack's own AF_UNIX sockets survive the host↔VM boundary on
  macOS. Carried as an open question in the same record
  (`docs/designs/ui/compass-native-embedded-revival/design.md:390-396`).
- The exact macOS versions the built `.app` runs on. The bundle declares an
  `LSMinimumSystemVersion` of `11.0`
  (`tools/macos-bundle/index.ts:69-70`), but CI only ever launches the binaries
  with `--version` on `macos-14`
  (`.github/workflows/ci.yml:1471`, `:1679`), so nothing verifies the floor.
