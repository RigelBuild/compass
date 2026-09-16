{
  pkgs,
  lib,
}:
# `compass-agent` as a real command on PATH — a BUNDLED entrypoint. The Runner
# execs a bare `compass-agent` argv with no flags, so the TypeScript entrypoint
# has to resolve and run like any other binary.
#
# Why a bundle, not the `writeShellScriptBin` + `bun run ${./src/cli.ts}` shape
# the repo's other bun CLIs use: interpolating a single .ts file copies only that
# ONE file, no siblings, no node_modules. That works for genuinely self-contained
# tools, but `cli.ts` has relative and workspace-package imports, so bun fails at
# the FIRST import and the headline entrypoint could not start.
#
# `bun build --compile` resolves the whole graph ahead of time into a STANDALONE
# executable. It is NOT a lone file: the native addon loader needs its prebuilt
# `.node` at runtime, so the derivation ships the binary WITH its
# `pi_natives.*.node` siblings in the same store dir, resolved from execDir — so
# a cold container with no node_modules and no network still loads the addon.
let
  system = pkgs.stdenv.hostPlatform.system;

  # Per-system native pins: the FOD hash covers the platform-specific
  # optionalDependency, and the addon set differs by arch (x64 two, arm64 one).
  nativeBySystem = {
    "x86_64-linux" = {
      outputHash = "sha256-JbgM44AwH7/b3Y/2T44+eBXwyvMi8owXToGVspEeCk4=";
      nativesPkg = "pi-natives-linux-x64";
      addons = [
        "pi_natives.linux-x64-modern.node"
        "pi_natives.linux-x64-baseline.node"
      ];
    };
    "aarch64-linux" = {
      outputHash = "sha256-asK46RRcPuByIjHMJUYL/UCp4f0UJBvPvZehUiyeW0I=";
      nativesPkg = "pi-natives-linux-arm64";
      addons = [ "pi_natives.linux-arm64.node" ];
    };
  };
  native =
    nativeBySystem.${system}
      or (throw "compass-agent entrypoint: unsupported system ${system}");

  # The package's dependency closure, fetched once as a fixed-output derivation
  # (the only derivation here allowed network access). `--frozen-lockfile` pins
  # versions to `bun.lock`; the output hash pins the installed tree on THIS host.
  nodeModules = pkgs.stdenv.mkDerivation {
    pname = "compass-agent-node-modules";
    version = "0.1.0";

    # Only the files `bun install` reads: the lockfile plus EVERY workspace
    # member's package.json (bun resolves the whole graph before installing one
    # filtered package). No source files, so a code edit does not invalidate this
    # closure. The member list is READ FROM the root manifest rather than restated,
    # so adding a workspace package cannot silently break this build; entries mix
    # literal paths and one-level globs (`packages/*`).
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

    # Bun's isolated layout is WORKSPACE-RELATIVE: the real packages live once in
    # the root store and the member's own `node_modules` is a tree of symlinks
    # pointing back up. Both trees are kept at their original depths so the
    # relative links resolve — flattening breaks every dependency.
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

    # A fixed-output derivation: `bun install` is the one step needing network,
    # and pinning the output hash keeps the rest pure. The hash covers the
    # INSTALLED TREE, not just the version set. `--ignore-scripts` keeps
    # postinstall from injecting host-varying content, and the installPhase strips
    # non-deterministic `.bin` shims, so the tree is reproducible across hosts.
    # Refresh with `lib.fakeSha256` on a bun.lock/manifest move.
    dontFixup = true;
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = native.outputHash;
  };

  # The package's own source, filtered as a DENYLIST. A bare path would copy the
  # directory wholesale, taking a developer's checked-out `node_modules` (no
  # `.gitignore` filtering), which would shadow the pinned FOD tree. It is a
  # denylist, not an allowlist, for the same reason the `nodeModules` src reads
  # its member list from the manifest: `bun build` only errors on a
  # statically-resolvable missing import, so an allowlist would silently drop a
  # new JSON fixture or proto dir. `moon.yml` is excluded (it drives moon tasks,
  # not this bundle); `node_modules` (the defect) is wrapped in `maybeMissing`
  # because `lib.fileset` errors on a path a clean checkout lacks.
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
  # bun installed it, with both node_modules trees restored around it, so the
  # member's relative symlinks resolve back to the root store as at install time.
  # Every copy MERGES into its destination (`cp -R <src>/. <dst>/`) rather than
  # relying on absence: plain `cp -R src dst` NESTS when dst exists, burying the
  # pinned tree at `node_modules/node_modules`.
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
    # legacy-compat shim (pi-coding-agent
    # src/extensibility/plugins/legacy-pi-compat.ts:751), guarded at runtime and
    # absent from our dependency closure. Left external so the bundler does not
    # fail resolving a module the code already tolerates missing.
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

    # The prebuilt addon ships in the per-system optionalDependency
    # `@oh-my-pi/pi-natives-linux-<arch>` (pinned in bun.lock, so in the FOD tree),
    # hoisted into `.bun/node_modules/@oh-my-pi/`. x64 carries two AVX2 variants
    # (modern/baseline); arm64 carries one. `cp` follows the hoist symlink.
    natives=node_modules/.bun/node_modules/@oh-my-pi/${native.nativesPkg}
    ${lib.concatMapStringsSep "\n" (f: "cp $natives/${f} $out/") native.addons}
  '';
in
# The bundle is a STANDALONE compiled binary, so the wrapper execs it directly —
# no `bun run` at runtime. It carries its own bun runtime and finds its native
# addon from the `.node` siblings beside it in the same store dir.
pkgs.writeShellScriptBin "compass-agent" ''
  exec ${bundle}/compass-agent "$@"
''
