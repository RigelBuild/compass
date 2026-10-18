{
  pkgs,
  lib,
  config,
  inputs,
  ...
}:
# Compass dev shell — the single source of the dev + CI toolchain. nix/devenv
# owns the language/runtime toolchains, the contract codegen tools, the Go
# analysis battery, and the linters the moon gate runs. Everything here is a
# system task's dependency: moon execs what this shell puts on PATH.
let
  # Postgres DSN shared by compass-server and dogfood:mint-runner-token. mint
  # MUST open the same store the server migrated, so both reference one binding —
  # a lone edit to either copy would silently point mint at a different database.
  dogfoodDSN = "host=${config.env.PGHOST} port=${toString config.env.PGPORT} dbname=compass sslmode=disable";

  # Runner id (enrollment subject) shared by dogfood:mint-runner-token and the
  # compass-runner process. Enroll cross-checks the token subject against
  # --runner-id, so mint and runner MUST agree or enroll is rejected — both
  # reference one binding so an overlay override moves them in lockstep.
  dogfoodRunnerID = "dogfood";

  # The go pin (tools/toolchain/versions/go.nix), version-selected from the
  # go-overlay input, which exposes each version as `go_<major>_<minor>_<patch>`
  # built over the devenv.lock-pinned nixpkgs. The parity gate selects the same
  # version through go-overlay's overlay route, so both resolve one derivation.
  goPin = import ./tools/toolchain/versions/go.nix;
  goToolchain = inputs.go-overlay.packages.${pkgs.stdenv.system}."go_${lib.replaceStrings [ "." ] [ "_" ] goPin.version}";

  # bun/node/moon vendored as nix derivations from tools/toolchain/versions/*.nix.
  # The dev shell and the parity gate (tools/toolchain/gate-tools.nix) import this
  # one module, so the two sides cannot drift.
  toolchainTools = import ./tools/toolchain/toolchain-tools.nix { inherit pkgs; };

  # Meissa's Go module adds analyzers rebuilt with this shell's Go toolchain.
  # The module adds them to packages; the toolchain itself stays explicit below.
  # Build connect-go's protoc plugin from the same Go graph as Compass. Its
  # vendorHash mirrors guest-image/default.nix and flake.nix.
  protocGenConnectGo = import ./tools/toolchain/protoc-gen-connect-go.nix { inherit pkgs goToolchain; };

  meissaToolchain = inputs.meissa.packages.${pkgs.stdenv.hostPlatform.system};
in
{
  imports = [ inputs.meissa.devenvModules.go ];
  meissa.go.toolchain = goToolchain;

  packages = (with pkgs; [
    # Store paths keep the generator toolchain stable during code generation.
    # A nixpkgs plugin bump requires regenerating headers with `moon run compass-proto:gen`.
    buf
    protobuf # protoc
    protoc-gen-go
    protoc-gen-es

    # sqlc: generates typed Go from the .sql query files in
    # go/internal/store/queries, compiled against the migrations schema. Bare
    # nixpkgs attr so the toolchain-parity gate resolves it in this literal.
    sqlc

    # Lint gate: biome + rumdl are NOT here — they come from Meissa, appended
    # below. `@biomejs/biome` stays a package.json devDep for the LSP.

    # actionlint: static checker for the .github/workflows/ files. A bare nixpkgs
    # attr in this parsed literal so the toolchain-parity gate resolves it,
    # giving the same pinned binary everywhere.
    actionlint

    # SQL migration lint battery: squawk (migration-safety) + sqruff (SQL
    # style/lint). Bare nixpkgs attrs in this parsed literal so the parity gate
    # resolves them, one pin giving CI and every dev box the identical binaries.
    squawk
    sqruff

    # curl: the compass-server readiness probe POSTs to GetServerInfo over the
    # loopback dev-http door. Pinned here (via `lib.getExe`) rather than an
    # ambient system curl, so one-command bring-up works on any dev box —
    # including a NixOS box without curl in current-system.
    curl

    # pkg-config: the Wails v3 cgo link needs it to discover the GTK4/WebKitGTK
    # `.pc` files (via PKG_CONFIG_PATH, set in `env` below). In this parsed list,
    # not the Linux env block, because it has a gate-resolved bin and is
    # cross-platform — harmless on macOS where the app links system WebKit.
    pkg-config

    # CI builds PATH from `packages`, not services, so its e2e gate needs postgres here.
    # Bare `postgresql` matches the default `services.postgres` derivation, keeping
    # CI and development on the same version.
    postgresql
  ])
  # The language toolchains, Go toolchain and Connect plugin are dotted references,
  # so they stay outside the parsed pkgs list, which the parity gate requires.
  ++ [
    toolchainTools.bun
    toolchainTools.node
    toolchainTools.moon
    goToolchain
    protocGenConnectGo
  ]
  # hk (jdx/hk): the jj/git pre-push hook runner. A dotted input reference,
  # appended outside the parsed literal (parity gate throws on non-bare tokens).
  # Cross-platform, so no isLinux guard. `enterShell` runs `hk install` against
  # the repo-root hk.pkl; pinned to v1.48.0 via the `hk` input in devenv.yaml.
  ++ [
    inputs.hk.packages.${pkgs.stdenv.system}.default
  ]
  # biome + rumdl: Meissa's lint toolchain at the devenv.lock-pinned `meissa`
  # rev, one derivation for every consumer. Dotted refs, so outside the parsed
  # literal; the parity gate covers them via gate-tools.nix's `meissa` output.
  ++ [
    meissaToolchain.biome
    meissaToolchain.rumdl
  ]
  # xvfb-run: virtual framebuffer wrapper for the multi-window gtk4 e2e — the
  # real GTK4/WebKit shell needs a display and dev boxes + CI runners are
  # headless. Linux-only, appended outside the parsed literal (the parity gate
  # resolves every bare attr on macOS too, where xvfb-run does not exist).
  ++ lib.optionals pkgs.stdenv.isLinux [ pkgs.xvfb-run ]
  # chromium: the browser the dev-boot smoke gate drives (apps/ui:dev-smoke).
  # Playwright points at it via PLAYWRIGHT_CHROMIUM_PATH. Linux-only (no darwin
  # build), appended outside the parsed literal for the same macOS reason as
  # xvfb-run. CI's dev-smoke provisions it via chromium-e2e-env.nix, not here.
  ++ lib.optionals pkgs.stdenv.isLinux [ pkgs.chromium ]
  # The publish lane needs nix2container's patched skopeo for the `nix:` transport.
  # Keep it out of the published agent image to avoid its unused 168 MB closure.
  # Linux-only and resolved from the pinned input.
  ++ lib.optionals pkgs.stdenv.isLinux [
    inputs.nix2container.packages.${pkgs.stdenv.system}.skopeo-nix2container
  ]
  # The elastic-session KVM backend needs cloud-hypervisor, virtiofsd, and passt.
  # Host `/dev/kvm` enablement is external; runtime preflight enforces the version floor.
  # Linux-only, outside the parsed package list because macOS lacks these tools.
  ++ lib.optionals pkgs.stdenv.isLinux [
    pkgs.cloud-hypervisor
    pkgs.virtiofsd
    pkgs.passt
  ]
  # The Go server resolves `secretspec` by name; this input includes the age provider.
  # Its version matches `hostcheck.SecretSpecFloor`.
  # Dotted references stay outside the parsed package list.
  ++ [
    inputs.secretspec-nixpkgs.legacyPackages.${pkgs.stdenv.system}.secretspec
  ];

  env = {
    # moon execs the vendored nix toolchain on PATH instead of resolving
    # go/gofmt through the ~/.proto shims, which back an absent `proto` binary
    # and otherwise kill every `compass-go:*` moon task with a shim exec error.
    # Platform-independent, so in the base set, not the Linux merge below.
    MOON_TOOLCHAIN_FORCE_GLOBALS = "true";
    # Same per-clone cache as `compass-go:lint` (go/moon.yml), so a bare shell
    # `golangci-lint` never shares ~/.cache/golangci-lint across clones.
    GOLANGCI_LINT_CACHE = "${config.devenv.root}/go/.golangci-cache";
    # The shared rumdl base policy; .rumdl.toml's `extends` expands this var.
    RUMDL_BASE_CONFIG = "${meissaToolchain."rumdl-base-config"}/rumdl/base.toml";
  }
  # GTK `.pc` files need the shared propagated closure for transitive `Requires:` lookup.
  # Search both `lib` and `share` pkgconfig; zlib installs its `.pc` under `share`.
  # Keep this Linux-only env out of packages so macOS uses system WebKit and the
  # parity gate avoids the GTK closure.
  // lib.optionalAttrs pkgs.stdenv.isLinux {
    PKG_CONFIG_PATH =
      let
        # The GTK4/WebKitGTK set, from the shared module so the dev shell and the
        # gtk4 e2e CI helper resolve one closure and cannot drift.
        pcClosure = lib.closePropagation (import ./tools/toolchain/gtk-closure.nix pkgs);
      in
      lib.concatStringsSep ":" [
        (lib.makeSearchPathOutput "dev" "lib/pkgconfig" pcClosure)
        (lib.makeSearchPathOutput "dev" "share/pkgconfig" pcClosure)
      ];

    # Visual-regression output depends on pinned fonts and hinting/AA, not host fonts.
    # Limit fontconfig to Chromium; process-wide `FONTCONFIG_FILE` would also change
    # fonts in dev-shell GUIs.
    # Linux-only because the gate's Chromium is Linux-only.
    E2E_FONTCONFIG_FILE = (import tools/toolchain/chromium-e2e-env.nix).fontconfig;

    # The gate's baselines are captured against this exact Chromium, so a dev
    # shell that leaves it unset falls through playwright.config.ts's default to
    # the host browser and reds every shot on version skew alone.
    PLAYWRIGHT_CHROMIUM_PATH =
      "${(import tools/toolchain/chromium-e2e-env.nix).chromium}/bin/chromium";
  };

  enterShell = ''
    # Bootstrap the workspace on shell entry so a fresh clone is ready right
    # after `direnv allow` — no separate install step. --frozen-lockfile is a
    # fast no-op once node_modules is current.
    bun install --frozen-lockfile
    # Pre-push git hook (a thin shell over `moon ci`). hk install is idempotent,
    # so re-run it on every entry — a guard on the hook file being absent would
    # never pick up `hk.pkl` changes in an already-cloned checkout.
    if command -v hk >/dev/null 2>&1; then
      hk install >/dev/null 2>&1 || echo "devenv: hk install failed; run 'hk install' to enable the pre-push gate"
    fi
  '';

  # `devenv up` orders store/cert/token dependencies; `build-cli` uses `before`
  # to join its upstream-only closure, so CLI compile failures fail `up`.
  # Image/clean stay opt-in: they need rootless Linux podman and image rebuilds
  # are heavy. `gen-cert` skips existing files; force regeneration after expiry.
  services.postgres = {
    enable = true;
    # The single dogfood database compass-server opens. Owned by $USER over the
    # local Unix socket (peer auth) — no password on the loopback dev path.
    initialDatabases = [ { name = "compass"; } ];
  };

  processes = {
    # nats: the event-fabric broker. compass-server publishes comms events to it
    # and delivery consumes them, so the server refuses to boot without it.
    # JetStream stores under the state dir; loopback-only, like the stack's.
    nats = {
      exec = ''
        exec ${lib.getExe pkgs.nats-server} -js -a 127.0.0.1 \
          -p ${toString config.processes.nats.ports.client.value} \
          -m ${toString config.processes.nats.ports.monitor.value} \
          -sd "${config.devenv.state}/nats"
      '';
      ports.client.allocate = 4222;
      ports.monitor.allocate = 8222;
      # /healthz answers 200 only once JetStream is enabled, the same readiness
      # verdict the self-host stack probes.
      ready.exec = ''
        ${lib.getExe pkgs.curl} -fsS --max-time 2 \
          "http://127.0.0.1:${toString config.processes.nats.ports.monitor.value}/healthz" >/dev/null
      '';
      restart.on = "on_failure";
    };

    # `devenv-tasks` signals the process group; `exec` keeps the server directly in it.
    # Its 5s stop grace matches the drain deadline, so a full drain may race SIGKILL.
    # Failure restarts recover store hiccups; devenv caps attempts at five.
    compass-server = {
      exec = ''
        # Build once into the state dir, then exec the binary so no shell/`go
        # run` parent lingers between devenv-tasks and compass-server — the
        # binary lands directly in the process group devenv-tasks signals on
        # stop (see the process comment above for the full signal path).
        bin="${config.devenv.state}/compass/compass-server"
        # Stamp the same scheme the release and CI lanes stamp: the semver base
        # from version.txt (the single source all build paths read), plus a
        # suffix marking this artifact as a dev-shell build. The suffix is a
        # literal `+dev` rather than `+g<shortrev>`: the repo is worked through
        # jj workspaces, where `.git` may be a file or absent, so shelling out
        # to `git rev-parse` inside the process script would be a build that
        # fails depending on which working copy it runs in.
        version_file="${config.devenv.root}/version.txt"
        # $(cat) drops NUL bytes with only a warning, splicing 1.2.3\0999 into a
        # stamped 1.2.3999 the flake lane (readFile hard-errors) refuses. UTF-16
        # ASCII text is every other byte NUL, so this refuses it too.
        if IFS= read -r -d "" _ <"$version_file"; then
          printf 'version.txt contains a NUL byte (UTF-16 or corrupt): %s\n' \
            "$version_file" >&2
          exit 1
        fi
        version_base="$(cat "$version_file")"
        # Trim the same four bytes nixpkgs `lib.strings.trim` strips on the
        # flake side (space, tab, CR, LF — both ends), THEN apply the same
        # character class the flake applies to its trimmed value. Both steps
        # are needed for the two lanes to accept exactly the same file: the
        # flake trims before matching, so checking the untrimmed bytes here
        # would reject a CRLF or tab-padded file the flake happily stamps —
        # `nix build` green while `devenv up` hard-fails on one tree. Trimming
        # only the surrounding whitespace (never inner) keeps the guard from
        # turning a malformed value into a plausible-looking stamp: `0.1.0 rc1`
        # is rejected, not silently stamped as `0.1.0rc1`. The class is a shape
        # filter, not a full semver parse. The two lanes are held in agreement
        # by the flake-gate:version-guard parity gate, which runs both guards
        # over a shared candidate table.
        while :; do
          case "$version_base" in
            [$' \t\r\n']*) version_base="''${version_base#?}" ;;
            *[$' \t\r\n']) version_base="''${version_base%?}" ;;
            *) break ;;
          esac
        done
        case "$version_base" in
          "" | *[!0-9A-Za-z.+-]*)
            printf 'version.txt missing or not a version string: %q\n' \
              "$version_base" >&2
            exit 1
            ;;
        esac
        # Explicit `|| exit 1` rather than leaning on the `set -e` devenv
        # injects into the rendered task script (its src/modules/tasks.nix
        # emits it for every non-status bash task): a failed build must not
        # fall through to the `exec` below and silently run the previously-built
        # binary at a stale version, and that must hold whether or not the
        # pinned devenv keeps rendering the wrapper.
        # The commit GetServerInfo reports as `rev`, so a redeploy can confirm the
        # running binary. Best-effort for the reason above: a working copy with no
        # git checkout stamps an empty rev; any tracked or untracked change adds "-dirty".
        rev="$(git -C "${config.devenv.root}" rev-parse HEAD 2>/dev/null)" || rev=""
        if [ -n "$rev" ] && [ -n "$(git -C "${config.devenv.root}" status --porcelain 2>/dev/null)" ]; then
          rev="$rev-dirty"
        fi
        go build -ldflags "-X main.version=$version_base+dev -X main.rev=$rev" \
          -o "$bin" ./cmd/compass-server || exit 1
        exec "$bin" \
          --socket "$COMPASS_SOCKET" \
          --dev-http "127.0.0.1:${toString config.processes.compass-server.ports.devhttp.value}" \
          --database "$COMPASS_DATABASE_DSN" \
          --listen "127.0.0.1:${toString config.processes.compass-server.ports.network.value}" \
          --tls-cert "${config.devenv.state}/compass/tls.crt" \
          --tls-key "${config.devenv.state}/compass/tls.key"
      '';
      cwd = "${config.devenv.root}/go";
      ports.devhttp.allocate = 50051;
      # The authenticated TLS network door. RunnerService lives only on this
      # door, so the runner (and any remote client) dials it over TLS with the
      # gen-cert trust anchor; the loopback socket/dev-http doors stay for the
      # co-located UI/shell.
      ports.network.allocate = 50052;
      env = {
        # Unix socket under the devenv state dir so the path is stable across
        # restarts and a co-located client (the UI/shell) resolves the same one.
        COMPASS_SOCKET = "${config.devenv.state}/compass/server.sock";
        # pgx keyword/value DSN over the Postgres Unix socket. No user= : pgx
        # defaults to the OS user, the peer-auth identity that owns `compass`.
        COMPASS_DATABASE_DSN = dogfoodDSN;
        # Dogfood is single-trust-domain and has no scope grant seed.
        COMPASS_FORGE_DISABLE_SCOPE_ENFORCEMENT = "true";
        # The event fabric; compass-server reads it when --nats-url is absent.
        COMPASS_NATS_URL = "nats://127.0.0.1:${toString config.processes.nats.ports.client.value}";
      };
      # Wait for Postgres and NATS before starting: the server opens the store
      # and connects the event fabric at boot, and fails closed on either.
      # (dogfood:gen-cert orders `before` this so the TLS cert/key exist when
      # the network door opens.)
      after = [ "devenv:processes:postgres" "devenv:processes:nats" ];
      # Ready only once the server answers, not merely once the socket exists:
      # compass-server binds the socket BEFORE migrating and accepting, so
      # `test -S` would flip ready true while a dial still blocks. Probe
      # GetServerInfo, which succeeds only after the store is migrated.
      ready.exec = ''
        ${lib.getExe pkgs.curl} -fsS --max-time 2 \
          -H 'Content-Type: application/json' \
          -X POST \
          "http://127.0.0.1:${toString config.processes.compass-server.ports.devhttp.value}/compass.v1.CompassService/GetServerInfo" \
          -d '{}' >/dev/null
      '';
      restart.on = "on_failure";
    };

    # The browser direct-dials the server's loopback dev-http door; wildcard CORS
    # permits it. Wait for readiness so the first load cannot race store migrations.
    compass-ui = {
      exec = ''
        exec bunx vite
      '';
      cwd = "${config.devenv.root}/apps/ui";
      env = {
        # Direct-dial: the browser dials the dev door directly, so the base URL
        # is the server's loopback dev-http port.
        VITE_COMPASS_BASE_URL =
          "http://127.0.0.1:${toString config.processes.compass-server.ports.devhttp.value}";
      };
      after = [ "devenv:processes:compass-server" ];
    };
  }
  // lib.optionalAttrs pkgs.stdenv.isLinux {
    # Remote runners use the authenticated TLS door and trust only gen-cert's CA.
    # Keep the enrollment token in env, not argv, to avoid process-table exposure.
    # Agent sockets must fit the 107-byte AF_UNIX limit, so `$XDG_RUNTIME_DIR` must
    # stay short (runner startup rejects a dir over 38 bytes). Enrollment failures restart.
    compass-runner = {
      exec = ''
        set -euo pipefail
        : "''${XDG_RUNTIME_DIR:?XDG_RUNTIME_DIR must be set; the compass-runner per-container sockets live under it}"
        bin="${config.devenv.state}/compass/compass-runner"
        go build -o "$bin" ./cmd/compass-runner
        # Assign, then export. `export VAR="$(cmd)"` is itself a command whose
        # own exit status (0) masks the substitution's, so `set -e` never sees a
        # failed read: an empty token misreports as unset, a truncated one
        # fails Enroll as `unauthenticated`.
        COMPASS_RUNNER_TOKEN="$(cat "${config.devenv.state}/compass/runner.token")"
        export COMPASS_RUNNER_TOKEN
        exec "$bin" \
          --runner-id ${dogfoodRunnerID} \
          --server "https://127.0.0.1:${toString config.processes.compass-server.ports.network.value}" \
          --ca "${config.devenv.state}/compass/tls.crt" \
          --image compass-agent:latest \
          --runtime-dir "$XDG_RUNTIME_DIR/compass-runner"
      '';
      cwd = "${config.devenv.root}/go";
      # Enroll only once the token exists (mint runs after the server is ready,
      # which is after the store is migrated — so the whole chain is ordered).
      after = [ "dogfood:mint-runner-token" ];
      restart.on = "on_failure";
    };
  };

  # Dogfood loop tasks. gen-cert, build-cli and mint-runner-token run
  # cross-platform (backing the macOS-native dogfood); agent-image and clean stay
  # Linux-only, as the whole podman-backed loop targets the Linux dev box.
  tasks = {
    # gen-cert: mint the self-signed TLS trust anchor the network door serves and
    # the runner trusts. Skip-if-present, so a restart never swaps the cert out
    # from under a live server/runner pair. Ordered `before` compass-server so
    # the cert/key exist when the network door opens.
    "dogfood:gen-cert" = {
      exec = ''
        set -euo pipefail
        bin="${config.devenv.state}/compass/compass-gen-cert"
        go build -o "$bin" ./cmd/compass-gen-cert
        exec "$bin" \
          --cert-out "${config.devenv.state}/compass/tls.crt" \
          --key-out "${config.devenv.state}/compass/tls.key"
      '';
      cwd = "${config.devenv.root}/go";
      before = [ "devenv:processes:compass-server" ];
    };

    # `build-cli` uses `before` to enter `up`'s upstream-only closure.
    # Remove the old binary first because `go build -o` leaves it on failure; otherwise
    # a failed compile could run stale code.
    # A compile failure fails `up` so the broken CLI cannot be ignored.
    "dogfood:build-cli" = {
      exec = ''
        set -euo pipefail
        bin="${config.devenv.state}/compass/compass"
        rm -f "$bin"
        go build -o "$bin" ./cmd/compass
      '';
      cwd = "${config.devenv.root}/go";
      before = [ "devenv:processes:compass-server" ];
    };

    # Assert the built artifact, not just the task graph: a missing binary means
    # the build produced nothing — skipped, dropped from the closure, or failed.
    "dogfood:check-cli" = {
      exec = ''
        bun "${config.devenv.root}/tools/operator-cli-check/index.ts"
      '';
      cwd = config.devenv.root;
      env = {
        DEVENV_STATE = config.devenv.state;
      };
      after = [ "dogfood:build-cli" ];
      before = [ "devenv:processes:compass-server" ];
    };

    # Use the server's DSN so store precedence matches; wait for its readiness probe
    # to ensure migrations are complete.
    # Writes the token 0600. Idempotent: keeps this runner's registration, restores
    # a lost one, and rotates a token held by another subject.
    "dogfood:mint-runner-token" = {
      exec = ''
        set -euo pipefail
        bin="${config.devenv.state}/compass/compass-mint-runner-token"
        go build -o "$bin" ./cmd/compass-mint-runner-token
        exec "$bin" \
          --runner-id ${dogfoodRunnerID} \
          --token-out "${config.devenv.state}/compass/runner.token"
      '';
      cwd = "${config.devenv.root}/go";
      env = {
        COMPASS_DATABASE_DSN = dogfoodDSN;
      };
      after = [ "devenv:processes:compass-server" ];
    };
  }
  // lib.optionalAttrs pkgs.stdenv.isLinux {
    # The runner resolves this image from containers-storage without pulling it.
    # Use agent-image's devenv.lock rev to match the module source.
    # Keep this opt-in: the image is large, and rebuilding it on `up` is too heavy.
    "dogfood:agent-image" = {
      exec = ''
        set -euo pipefail
        # Drop any stale localhost/ tag BEFORE the copy. `container copy` writes
        # the fresh image to docker.io/library/compass-agent:latest, but podman
        # resolves the BARE ref `compass-agent:latest` (what the runner and the
        # e2e fixture use) against localhost/ FIRST — so a leftover
        # localhost/compass-agent:latest from an older build would SHADOW this
        # fresh copy and the runner would silently launch stale agent code.
        # Removing it first makes the bare ref resolve to the image we just built.
        # `podman rmi` exits non-zero when the tag is absent, so tolerate that.
        podman rmi -f localhost/compass-agent:latest 2>/dev/null || true
        src=$(bun "${config.devenv.root}/tools/toolchain/devenv-cli/index.ts" --lock "${config.devenv.root}/agent-image/devenv.lock" --mode flakeref)
        nix run "$src" -- container copy agent
      '';
      cwd = "${config.devenv.root}/agent-image";
    };

    # Runner startup sweeps only containers labelled with its own id. Unlabeled
    # pre-label containers still need dogfood:clean; on a shared podman user this
    # task removes every environment's agent containers.
    "dogfood:clean" = {
      exec = ''
        set -euo pipefail
        # Same guard the compass-runner process uses, and it runs FIRST: under
        # `set -u` an unset XDG_RUNTIME_DIR otherwise aborts the task after the
        # containers are already removed, so it half-succeeds and reports failure.
        : "''${XDG_RUNTIME_DIR:?XDG_RUNTIME_DIR must be set; the compass-runner per-container sockets live under it}"
        # Uses the host's rootless podman — the same binary/storage the runner
        # execs to create these containers, so it sees them (a nix-pinned podman
        # could resolve a different containers-storage config).
        ids="$(podman ps -a --filter name=compass-agent- --format '{{.ID}}')"
        if [ -n "$ids" ]; then
          # shellcheck disable=SC2086
          podman rm -f $ids
        fi
        rm -rf "$XDG_RUNTIME_DIR/compass-runner/containers/"
        # Drop the stale localhost/ tag of the agent image. `container copy`
        # writes the fresh image to docker.io/library/compass-agent:latest, but
        # podman resolves the BARE ref `compass-agent:latest` (what the runner and
        # the e2e fixture use) against localhost/ FIRST — so a leftover
        # localhost/compass-agent:latest from an older build SHADOWS every fresh
        # copy, and the runner silently launches stale agent code. `podman rmi`
        # exits non-zero when the tag is absent, so tolerate that: the goal is a
        # clean slate, not a guaranteed prior existence.
        podman rmi -f localhost/compass-agent:latest 2>/dev/null || true
      '';
    };
  };
}
