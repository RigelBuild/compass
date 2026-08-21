{
  pkgs,
  lib,
}:
# `compass-agent` as a real command on PATH — a BUNDLED entrypoint.
#
# The Runner execs a bare `compass-agent` argv with no flags
# (go/internal/runner/relay.go `agentCommand`), so the TypeScript entrypoint has
# to resolve and run like any other binary.
#
# WHY A BUNDLE, and not the `writeShellScriptBin` + `bun run ${./src/cli.ts}`
# shape the repo's other bun CLIs use: interpolating a single .ts file copies
# exactly that ONE FILE into the store, with no siblings and no node_modules
# beside it. That works for `tools/wait-for-reviews` and `tools/get-pr-diff`
# because each is genuinely self-contained — their only import is `bun`'s own
# `$` builtin, which needs no resolution. `cli.ts` is not: it has five relative
# imports (./agent, ./transport/*) and three workspace-package imports
# (@oh-my-pi/pi-coding-agent, @oh-my-pi/pi-ai, and their transitive closure).
# Pointed at a lone store file, bun fails at the FIRST import — `ENOENT while
# resolving package '@oh-my-pi/pi-coding-agent'` — so the container's headline
# entrypoint could not start at all.
#
# `bun build --compile` resolves the whole graph ahead of time and emits a
# STANDALONE executable (bun runtime + bundled graph). It is NOT a lone file:
# the native addon loader needs its prebuilt `.node` at runtime, so the bundle
# derivation ships the compiled binary WITH its `pi_natives.*.node` siblings in
# the same store dir. In compiled mode the loader resolves the addon from
# execDir = `dirname(process.execPath)`, which is exactly that dir — so a cold
# container with no node_modules and no network still loads the native addon.
let
  # The package's dependency closure, fetched once as a fixed-output derivation
  # (the only derivation here allowed network access). `--frozen-lockfile` pins
  # the VERSIONS to `bun.lock`; the output hash below pins the installed tree as
  # it lands on THIS build platform.
  nodeModules = pkgs.stdenv.mkDerivation {
    pname = "compass-agent-node-modules";
    version = "0.1.0";

    # Only the files `bun install` reads: the lockfile plus EVERY workspace
    # member's package.json. Bun resolves the whole workspace graph before it
    # will install a single filtered package — omit one member and it aborts
    # with `Workspace not found` — so the manifests are all required even though
    # only `@compass/agent`'s dependencies are installed. Manifests and the
    # lockfile only: no source files, so an unrelated code edit does not
    # invalidate this closure.
    #
    # The member list is READ FROM the root manifest rather than restated here,
    # so adding a workspace package cannot silently break this build. Entries
    # are a mix of literal paths and one-level globs (`packages/*`), so a
    # trailing `/*` is expanded against the directory.
    src =
      let
        repoRoot = ../.;
        entries = (builtins.fromJSON (builtins.readFile ../package.json)).workspaces.packages;
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
            ../bun.lock
            ../package.json
          ]
          ++ manifests
        );
      };

    nativeBuildInputs = [ pkgs.bun ];
    dontConfigure = true;

    buildPhase = ''
      runHook preBuild
      export HOME=$TMPDIR
      bun install --frozen-lockfile --ignore-scripts --filter '@compass/agent'
      runHook postBuild
    '';

    # Bun's isolated layout is WORKSPACE-RELATIVE: the real packages live once
    # in the root store (`node_modules/.bun/<pkg>@<ver>/...`) and the member's
    # own `node_modules` is a tree of symlinks pointing back up at it
    # (`../../../../node_modules/.bun/...`). Both trees are kept at their
    # original depths so those relative links still resolve — flattening or
    # splitting them breaks every dependency.
    installPhase = ''
      runHook preInstall
      mkdir -p $out/packages/compass-agent
      cp -R node_modules $out/node_modules
      cp -R packages/compass-agent/node_modules \
            $out/packages/compass-agent/node_modules
      # Drop every `node_modules/.bin` directory before hashing. Its entries are
      # per-CLI symlinks bun points at each package's own `bin/`, not generated
      # wrapper scripts — so `rm -rf` on a `.bin` dir removes only the links,
      # never their targets, and no dependency file is collaterally deleted. The
      # bundle below resolves imports through the `.bun` package trees, never
      # through `.bin`, so the runtime does not need them. They are ALSO the sole
      # source of this FOD's cross-environment non-determinism: bun's
      # nested-`.bin` symlink set is not
      # stable across build hosts (e.g. a `browserslist` shim inside
      # `update-browserslist-db/node_modules/.bin` is emitted on some hosts and
      # not others), which desynchronizes an otherwise byte-identical tree.
      # Removing them makes the recursive output hash reproducible.
      find $out -type d -name .bin -prune -exec rm -rf {} +
      runHook postInstall
    '';

    # A fixed-output derivation: `bun install` is the one step here that needs
    # the network, and pinning the output hash is what keeps the rest of the
    # build pure. What the hash covers is the INSTALLED TREE, not just the
    # lockfile's version set: `bun install` writes a `.bun` cache layout and
    # platform-specific optional dependencies. `--ignore-scripts` keeps
    # postinstall from injecting host-varying content, and the installPhase
    # strips the non-deterministic `.bin` shims above, so the tree is
    # reproducible across build hosts. Expect the hash to move when `bun.lock` or
    # a workspace manifest does — refresh it by setting `lib.fakeSha256` and
    # taking the value nix reports.
    dontFixup = true;
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = "sha256-KCjicPjnY2WRbgxNHdlqAjY1bGTOEx9nGPvzZzjcxOA=";
  };

  # The package's own source. A BARE path here (`${../packages/…}`) would copy
  # the directory wholesale, and a developer's checked-out `node_modules` sits in
  # it — an unfiltered path copy takes it (no `lib.fileset`, so no `.gitignore`),
  # which would then shadow the pinned FOD tree and resolve imports against
  # whatever the build machine happened to have.
  #
  # Stated as a DENYLIST, not a hand-maintained allowlist of the files `bun
  # build` reads today, and for the same reason the `nodeModules` src above reads
  # its member list out of the root manifest: a new bundle-time input must not be
  # able to break this build silently. `bun build` only errors on a
  # statically-resolvable missing import, so an allowlist would answer a new JSON
  # fixture, a generated-proto dir outside `src`, or a `bunfig.toml` with a
  # bundle that still builds and is quietly missing it — the same latent shape as
  # the defect the filtering was added for. The cost of the inversion is that an
  # untracked working-tree file is admitted too; that is the lesser failure, and
  # the two entries that actually matter are named below.
  #
  # `moon.yml` is excluded deliberately: it drives moon's typecheck/test tasks,
  # not this bundle. `node_modules` is the defect itself, and is wrapped in
  # `maybeMissing` because `lib.fileset` errors on a nonexistent path and a clean
  # checkout has not run `bun install`.
  pkgSrc = lib.fileset.toSource {
    root = ../packages/compass-agent;
    fileset = lib.fileset.difference ../packages/compass-agent (
      lib.fileset.unions [
        (lib.fileset.maybeMissing ../packages/compass-agent/node_modules)
        ../packages/compass-agent/moon.yml
      ]
    );
  };

  # Bundle inside a RECONSTRUCTED workspace: the package source at the same depth
  # bun installed it (`packages/compass-agent`) with both node_modules trees
  # restored around it, so the member's relative symlinks resolve back to the
  # root store exactly as they did at install time.
  #
  # Every copy MERGES into its destination (`cp -R <src>/. <dst>/` against a
  # pre-made dir) rather than relying on the destination being absent: plain
  # `cp -R src dst` NESTS when `dst` already exists, which would bury the pinned
  # tree at `node_modules/node_modules` where nothing resolves it.
  bundle = pkgs.runCommand "compass-agent-bundle" { nativeBuildInputs = [ pkgs.bun ]; } ''
    export HOME=$TMPDIR
    pkgDir=packages/compass-agent

    mkdir -p $pkgDir
    cp -R ${pkgSrc}/. $pkgDir/
    chmod -R +w $pkgDir

    mkdir -p node_modules $pkgDir/node_modules
    cp -R ${nodeModules}/node_modules/. node_modules/
    cp -R ${nodeModules}/$pkgDir/node_modules/. $pkgDir/node_modules/

    mkdir -p $out
    # `omp-legacy-pi-modules` is an OPTIONAL dynamic import inside the SDK's
    # legacy-compat shim (pi-coding-agent legacy-pi-compat.ts:50), guarded at
    # runtime and absent from our dependency closure. Left external so the
    # bundler does not fail resolving a module the code already tolerates
    # missing.
    #
    # `--compile` emits a STANDALONE executable (bun runtime + the whole
    # resolved graph baked in), not an interpreted `cli.js`. This is what lets
    # the runtime native-addon loader find its `.node` beside the binary: in
    # compiled mode the loader's candidate list includes execDir =
    # `dirname(process.execPath)` (pi-natives native/loader-state.js), so a
    # `.node` copied next to the binary resolves cold, with no node_modules,
    # no network, and from any cwd.
    bun build $pkgDir/src/cli.ts \
      --compile \
      --external omp-legacy-pi-modules \
      --outfile=$out/compass-agent

    # Ship the prebuilt native addon BESIDE the compiled binary. It is not
    # inside `@oh-my-pi/pi-natives`; it ships in the platform optionalDependency
    # `@oh-my-pi/pi-natives-linux-x64` (pinned in bun.lock, so present in the
    # FOD tree). bun's isolated install keeps the platform package in its `.bun`
    # virtual store and hoists it through the version-independent symlink
    # `node_modules/.bun/node_modules/@oh-my-pi/pi-natives-linux-x64` (it is NOT
    # hoisted to the plain top-level `node_modules/@oh-my-pi/`). It carries two
    # CPU variants; the loader picks `modern` when the host has AVX2 else
    # `baseline` (loader-state.js), so BOTH must be present for either host to
    # resolve. `cp` follows the hoist symlink to copy the real files.
    natives=node_modules/.bun/node_modules/@oh-my-pi/pi-natives-linux-x64
    cp $natives/pi_natives.linux-x64-modern.node $out/
    cp $natives/pi_natives.linux-x64-baseline.node $out/
  '';
in
# The bundle is now a STANDALONE compiled binary, not an interpreted `cli.js`,
# so the wrapper execs it directly — no `bun run` at runtime. The compiled
# binary carries its own bun runtime, and finds its native addon from the
# `.node` siblings copied beside it in the same store dir.
pkgs.writeShellScriptBin "compass-agent" ''
  exec ${bundle}/compass-agent "$@"
''
