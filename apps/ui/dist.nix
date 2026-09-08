{
  pkgs,
  lib,
  version,
}:
# The Compass web UI (`apps/ui`) built to a static `dist/` in the nix store —
# the `compass-ui:build` moon task's output (`bunx vite build`, `apps/ui/moon.yml`
# task `build`), realized as a derivation.
#
# WHY THIS EXISTS (RIG-3474). `compass-app` is a gtk4 shell that loads its
# front-end off disk: `distDirForExecutable`
# (go/cmd/compass-app/main.go:353-366) resolves `dist` BESIDE the executable. The
# release tarball satisfies that by staging `bin/dist` next to `bin/compass-app`
# (app-bundle/build.sh:97), but the flake's `compass-app` was a bare
# `buildGoModule` emitting only `bin/compass-app` — so
# `nix profile install .#compass-app` installed a shell with no UI. The flake's
# own contract (flake.nix:3-6) is that a bare checkout packages the native app,
# and a UI-less app does not meet it. This derivation is the missing half; the
# flake stages it into `$out/bin/dist`, mirroring the tarball layout exactly so
# the two distribution channels resolve the UI the same way.
#
# WHY FROM THE REPO ROOT, not `apps/ui` in isolation: `@compass/ui` depends on
# `@compass/client` as `workspace:*` (apps/ui/package.json:13), and the repo is a
# single bun workspace with ONE root lockfile (`/bun.lock`) and a hoisted
# `node_modules` (package.json's `workspaces.packages`). Both derivations below
# therefore build in a workspace reconstructed at the builder's cwd, with the
# members at the paths the root manifest declares — `@compass/client` resolves
# through a `node_modules/@compass/client -> ../../packages/compass-client`
# workspace link, so the member must sit at that path for the link to land
# anywhere.
let
  # Both members whose dependencies the build needs. `@compass/client` is not
  # merely a workspace sibling to resolve: the UI imports it, it is source-only
  # (`"exports": { ".": "./src/index.ts" }`,
  # packages/compass-client/package.json:7-9), so vite compiles its TypeScript
  # in-tree and must resolve ITS dependencies (@bufbuild/protobuf,
  # @connectrpc/connect{,-web}) too. Installing only `@compass/ui` would leave
  # those absent and fail the build at the first client import.
  members = [
    "@compass/ui"
    "@compass/client"
  ];

  # The dependency closure, fetched once as a fixed-output derivation — the only
  # derivation here allowed network access. `--frozen-lockfile` pins the VERSIONS
  # to `/bun.lock`; the `outputHash` pins the installed tree as it lands on this
  # build platform. Same shape and rationale as agent-image/entrypoint.nix:31-135,
  # which pins the sibling `compass-agent` closure — with ONE deliberate
  # difference, the linker strategy, argued at `--linker=hoisted` below.
  nodeModules = pkgs.stdenv.mkDerivation {
    pname = "compass-ui-node-modules";
    # Deliberately NOT the flake's `version`: a FOD's output path is derived from
    # its hash AND its name, so stamping the `+g<shortRev>` suffix here would
    # rename the output on every commit and force a fresh network install per
    # build, for a tree whose content depends only on the lockfile. The build
    # derivation below carries the real stamp.
    version = "0.1.0";

    # Only the files `bun install` reads: the lockfile plus EVERY workspace
    # member's package.json. Bun resolves the whole workspace graph before it
    # will install a single filtered member — omit one and it aborts with
    # `Workspace not found` — so all the manifests are required even though only
    # the two members above are installed. No source files, so an unrelated code
    # edit does not invalidate this closure.
    #
    # The member list is READ FROM the root manifest rather than restated, so
    # adding a workspace package cannot silently break this build. Entries are a
    # mix of literal paths and one-level globs (`packages/*`), so a trailing
    # `/*` is expanded against the directory.
    #
    # `bunfig.toml` is deliberately EXCLUDED. The root config sets
    # `install.minimumReleaseAge` (a 5-day supply-chain soak on freshly-published
    # packages) — a WALL-CLOCK-dependent policy, and bun reads bunfig from the
    # install dir. Admitting it would make this FOD's result a function of the
    # date it is realized: the same lockfile could install cleanly today and
    # refuse tomorrow's re-realise, or vice versa. The soak is a gate on what
    # enters `bun.lock`, and it does its work at `bun install` time on a dev box
    # or in CI; by the time a version is IN the lockfile it is a reviewed pin,
    # which is precisely what `--frozen-lockfile` installs here.
    src =
      let
        repoRoot = ../..;
        entries = (builtins.fromJSON (builtins.readFile ../../package.json)).workspaces.packages;
        expand =
          entry:
          if lib.hasSuffix "/*" entry then
            let
              parent = lib.removeSuffix "/*" entry;
              children = builtins.readDir (repoRoot + "/${parent}");
            in
            lib.mapAttrsToList (name: _: "${parent}/${name}") (
              lib.filterAttrs (
                name: type:
                type == "directory" && builtins.pathExists (repoRoot + "/${parent}/${name}/package.json")
              ) children
            )
          else
            [ entry ];
        manifests = map (p: repoRoot + "/${p}/package.json") (lib.concatMap expand entries);
      in
      lib.fileset.toSource {
        root = repoRoot;
        fileset = lib.fileset.unions (
          [
            ../../bun.lock
            ../../package.json
          ]
          ++ manifests
        );
      };

    nativeBuildInputs = [ pkgs.bun ];
    dontConfigure = true;

    # `--linker=hoisted` IS THE REPRODUCIBILITY FIX, not a style choice, and it
    # is why this FOD does not simply copy entrypoint.nix's invocation.
    #
    # Bun's DEFAULT `isolated` linker installs each package once under
    # `node_modules/.bun/<pkg>@<ver>/` and then publishes a version-INDEPENDENT
    # alias for it at `node_modules/.bun/node_modules/<pkg>`. That alias is
    # single-slot, so when the lockfile resolves the same package NAME at two
    # versions, the slot has no principled winner and bun binds whichever
    # install lands last — a race. This closure has 38 such multi-version names,
    # and it was MEASURED to flip: three clean re-realises of the isolated
    # layout produced two different trees, differing in exactly one link
    # (`.bun/node_modules/web-vitals` -> `web-vitals@6.0.0` vs `@5.3.0`, the two
    # versions `posthog-js` pulls — `^5.3.0` plus a `web-vitals-soft-navs`
    # aliased at 6.0.0, bun.lock:1762,2074,2076). A recursive output hash over a
    # tree with a racing link is a hash that fails on a cache eviction, in CI,
    # on another machine — the FOD would go red with `hash mismatch` on a build
    # that changed nothing. Pinning whichever value one run happened to produce
    # does not fix that; it just picks a coin flip.
    #
    # `hoisted` (npm-style) has no such alias dir: every package resolves at a
    # real path, so a second version nests under its dependent instead of
    # competing for one slot. Three clean installs were byte-identical
    # (0 differing paths), and the vite build over the hoisted tree emits the
    # same bundle — an identical `index-<hash>.js` content hash to the isolated
    # build — so this buys determinism at no cost to what ships.
    #
    # Stripping the alias dir instead was tried and REJECTED: the build genuinely
    # resolves through it (removing it fails the vite build with an
    # unresolved-import error out of rolldown), so it is load-bearing, not
    # incidental. `entrypoint.nix` keeps the isolated layout because its consumer
    # is a `bun build --compile` bundle that reaches into `.bun` paths directly
    # (entrypoint.nix:208-218); this consumer is vite, which needs only ordinary
    # resolution, so it can take the deterministic layout.
    buildPhase = ''
      runHook preBuild
      # bun writes its install cache and metadata under $HOME, which defaults to
      # the read-only store root in a nix builder; both are pointed at the
      # build's own temp dir so the install has somewhere writable.
      export HOME=$TMPDIR
      export BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache
      bun install --frozen-lockfile --ignore-scripts --linker=hoisted \
        ${lib.concatMapStringsSep " " (m: "--filter '${m}'") members}
      runHook postBuild
    '';

    # The hoisted layout puts the whole closure in the ROOT `node_modules` (no
    # per-member `node_modules` is created), so that one tree is the output. The
    # two workspace members appear in it as links out to their source paths
    # (`node_modules/@compass/client -> ../../packages/compass-client`), which
    # dangle here and are re-pointed at real source by the build below.
    installPhase = ''
      runHook preInstall
      mkdir -p $out
      cp -R node_modules $out/node_modules
      # Drop every `node_modules/.bin` before hashing. Its entries are per-CLI
      # symlinks into each package's own `bin/`, so removing a `.bin` dir removes
      # only links, never their targets. The build below invokes vite through its
      # real entry point (`node_modules/vite/bin/vite.js`) rather than a `.bin`
      # shim, so nothing needs them — and in the isolated layout these were a
      # measured source of cross-host drift, so they stay stripped.
      find $out -type d -name .bin -prune -exec rm -rf {} +
      runHook postInstall
    '';

    # Expect this hash to move when `bun.lock` or a workspace manifest changes,
    # and on a devenv-nixpkgs channel bump (which moves `pkgs.bun`, the builder).
    # Refresh it by setting `lib.fakeSha256` and taking the value nix reports.
    #
    # NOTE(RIG-3514): tools/renovate/refresh-fod-hashes.ts's
    # `FOD_ENTRIES` automates exactly that refresh for the repo's two other pins,
    # but its `marker` is a per-file unique substring and its five Renovate task
    # sites each name the pin files they may write (tools/renovate/config.json5),
    # asserted by tools/renovate/config.test.ts. Registering this third pin is a
    # change to that surface, not to this one, so it is filed rather than done
    # here — until it lands, a bun.lock bump reddens `nix build .#compass-ui`
    # with `hash mismatch in fixed-output derivation` and the pin is refreshed by
    # hand.
    dontFixup = true;
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = "sha256-g/TyWGw/D3bLZ9UFELG+1W/KZhoPB/yOZy/ffvx53O0=";
  };

  # The two members' own source. Stated as a DENYLIST, not an allowlist of the
  # files vite reads today: a new build-time input (a public/ asset dir, a
  # postcss config, a new generated-proto tree) must not be able to drop out of
  # this build silently — vite would emit a bundle that is quietly missing it
  # rather than failing. The cost of the inversion is that an untracked
  # working-tree file is admitted too; that is the lesser failure, and the entries
  # that actually matter are named below.
  #
  # `.env*` is the exception the denylist has to name explicitly, and it is a
  # SECRETS measure only — not what keeps the dev door out of the bundle (that
  # is the `unset` at the vite invocation below, because `bun` autoloads
  # `.env*` into `process.env` while node does not). A developer's untracked
  # `apps/ui/.env.local` would otherwise be copied into a world-readable store
  # path on a PUBLIC repo: vite inlines only `VITE_`-prefixed vars into the
  # bundle, so a secret in one does not reach `dist`, but the file itself still
  # lands in the derivation source. Filtered by name rather than by a
  # `.gitignore` read, because `lib.fileset` does not consult gitignore and the
  # risky file is precisely the untracked one.
  #
  # `node_modules` is the defect the filtering exists for — an unfiltered path
  # copy takes a developer's checked-out tree (no `.gitignore` awareness), which
  # would then shadow the pinned FOD tree and resolve imports against whatever
  # the build host happened to have. `dist` is a stale prior output of this very
  # build. Both are wrapped in `maybeMissing` because `lib.fileset` errors on a
  # nonexistent path and a clean checkout has run neither `bun install` nor a
  # build.
  # Matches `.env`, `.env.local`, `.env.development`, … anywhere in the tree.
  dotenvFiles =
    dir: lib.fileset.fileFilter (f: lib.hasPrefix ".env" f.name) dir;

  uiSrc = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.difference ./. (
      lib.fileset.unions [
        (lib.fileset.maybeMissing ./node_modules)
        (lib.fileset.maybeMissing ./dist)
        (dotenvFiles ./.)
      ]
    );
  };

  clientSrc = lib.fileset.toSource {
    root = ../../packages/compass-client;
    fileset = lib.fileset.difference ../../packages/compass-client (
      lib.fileset.unions [
        (lib.fileset.maybeMissing ../../packages/compass-client/node_modules)
        (dotenvFiles ../../packages/compass-client)
      ]
    );
  };
in
pkgs.runCommand "compass-ui-${version}"
  {
    nativeBuildInputs = [ pkgs.bun ];
  }
  ''
    export HOME=$TMPDIR
    export BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache

    # Reconstruct the workspace at the paths the root manifest declares, so the
    # pinned tree's workspace links (`node_modules/@compass/client ->
    # ../../packages/compass-client`) resolve onto real source.
    #
    # Every copy MERGES into a pre-made destination (`cp -R <src>/. <dst>/`)
    # rather than relying on the destination being absent: plain `cp -R src dst`
    # NESTS when `dst` already exists, which would bury the pinned tree at
    # `node_modules/node_modules` where nothing resolves it.
    mkdir -p apps/ui packages/compass-client
    cp -R ${uiSrc}/. apps/ui/
    cp -R ${clientSrc}/. packages/compass-client/
    chmod -R +w apps packages

    # The workspace root's own manifest and the shared tsconfig
    # `apps/ui/tsconfig.json` extends (`../../tsconfig.base.json`). Without the
    # latter, tsconfig resolution fails and vite's transform silently falls back
    # to defaults.
    cp ${../../package.json} package.json
    cp ${../../tsconfig.base.json} tsconfig.base.json

    mkdir -p node_modules
    cp -R ${nodeModules}/node_modules/. node_modules/
    chmod -R +w node_modules

    # `bunx vite build` (the moon task's command) would consult the network for a
    # missing binary; vite is already in the pinned tree, so invoke its real
    # entry point directly. In the hoisted layout that path is plain and
    # version-independent — unlike the isolated layout's
    # `node_modules/.bun/vite@<version>+<hash>/`, which a dependency bump
    # renames — and it does not go through a `.bin` shim, which the FOD strips.
    #
    # cwd is apps/ui because that is where `vite.config.ts` and `index.html`
    # live, matching the moon task's project cwd. Output lands in `apps/ui/dist`
    # (moon.yml `outputs: ['dist']`), vite's default.
    cd apps/ui
    # THE RUNTIME IS THE HAZARD, not the mode and not which files exist. `bun`
    # autoloads `.env*` into `process.env`; node does not. Measured in this
    # directory: `bun -e 'console.log(process.env.VITE_COMPASS_BASE_URL)'`
    # prints `http://127.0.0.1:50051`, the same expression under `node` prints
    # `undefined`. Vite then inlines whatever `VITE_*` it finds in `process.env`
    # regardless of `--mode`. The moon task's `bunx vite build` runs vite under
    # NODE (`vite/8.2.0 linux-x64 node-v24.20.0`), which is why the tarball lane
    # never had this bug; invoking the entry point with `bun` above — done for
    # the resolution reasons argued there — silently changed that env semantic
    # and baked the dev door into the bundle (`127.0.0.1:50051`, twice in
    # `index-<hash>.js`), so a flake-installed app dialled localhost.
    #
    # BOTH defenses are required; neither is sufficient alone, and that is
    # measured, not assumed. Re-admitting `.env.development` to the derivation
    # source with this `unset` still in place STILL bakes the door — bun loads
    # the file when it starts vite, which is after this shell line runs — and
    # the assertion below catches it. Conversely the `unset` is not redundant
    # with the filter: it clears a `VITE_COMPASS_*` inherited from the ambient
    # environment, which the source filter cannot reach. A real build's
    # door+bearer come from the build-time environment; unset, the app resolves
    # them at boot instead of baking a wrong one. `--mode production` states the
    # intent explicitly since this lane does not go through `bunx`.
    unset VITE_COMPASS_BASE_URL VITE_COMPASS_TOKEN
    bun ../../node_modules/vite/bin/vite.js build --mode production
    cd ../..

    # Sanity assertion, mirroring app-bundle/build.sh:93 — the presence check the
    # tarball lane already makes. A green build must mean a COMPLETE dist: vite
    # exits 0 having emitted nothing if `index.html` (its build entry point) went
    # missing from the source filtering above, and the gtk4 shell would then load
    # a blank window at runtime with nothing anywhere reporting a failure. Fail
    # here instead.
    if [ ! -f apps/ui/dist/index.html ]; then
      echo "ERROR: vite build produced no apps/ui/dist/index.html" >&2
      exit 1
    fi

    # Second assertion: the dev door must not be baked in. This lane really did
    # ship `127.0.0.1:50051` inlined in the bundle, and neither a comment nor
    # `--mode production` prevents a future edit from re-admitting
    # `.env.development` to the derivation source. The sentinel is the same one
    # `src/preview-build.test.ts` pins as `DEV_DEFAULT_BASE_URL`; that gate
    # drives `bunx vite build` in a checkout and cannot observe this build, so
    # the check is repeated here where it can fail.
    if grep -rq '127\.0\.0\.1:50051' apps/ui/dist; then
      echo "ERROR: the .env.development dev door is baked into the bundle" >&2
      exit 1
    fi

    # The CONTENTS of apps/ui/dist become $out, so a consumer stages the store
    # path itself as its `dist` directory (flake.nix's compass-app postInstall)
    # and `$out/index.html` is the bundle's entry point.
    mkdir -p $out
    cp -R apps/ui/dist/. $out/
  ''
