{
  pkgs,
  lib,
}:
# Bundle the full TypeScript import graph: the Runner invokes this bare command,
# while a single-file bun wrapper would omit relative and workspace imports.
# Keep platform-specific native `.node` siblings beside the binary; the loader
# resolves them there when the container has no node_modules or network.
let
  system = pkgs.stdenv.hostPlatform.system;

  # Per-system pins: the FOD covers the platform-specific optionalDependency,
  # and the addon set differs by arch (x64 two, arm64 one).
  outputHashBySystem = {
    "x86_64-linux" = "sha256-5xK8aKaDAXVu+otqOF3CIvqNuor8KPk3uJDW1m3tFB0=";
    "aarch64-linux" = "sha256-kMha5mgsD9+gZk8F3YgmMUGjVORxPCS229if7Kc5ys8=";
  };
  nativeBySystem = {
    "x86_64-linux" = {
      bunCpu = "x64";
      nativesPkg = "pi-natives-linux-x64";
      addons = [
        "pi_natives.linux-x64-modern.node"
        "pi_natives.linux-x64-baseline.node"
      ];
    };
    "aarch64-linux" = {
      bunCpu = "arm64";
      nativesPkg = "pi-natives-linux-arm64";
      addons = [ "pi_natives.linux-arm64.node" ];
    };
  };
  native =
    nativeBySystem.${system} or (throw "compass-agent entrypoint: unsupported system ${system}");

  # Fetch the locked dependency tree in the only network-enabled derivation.
  # Install and compile with repo-pinned Bun: the binary embeds it, and the SDK
  # needs Bun.Image, which nixpkgs' Bun lacks.
  pinnedBun = (import ../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;

  # Parameterized by TARGET system: `--cpu` selects that arch's optionalDependency,
  # so either pin can be recomputed from any host.
  nodeModulesFor =
    target:
    pkgs.stdenv.mkDerivation {
      pname = "compass-agent-node-modules";
      version = "0.1.0";

      # Include only bun install manifests: it resolves every workspace member before
      # filtering, but source edits should not invalidate this closure.
      # Expand globs from root package.json so new workspace members stay included.
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

      nativeBuildInputs = [ pinnedBun ];
      dontConfigure = true;

      buildPhase = ''
        runHook preBuild
        export HOME=$TMPDIR
        bun install --frozen-lockfile --ignore-scripts --filter '@compass/agent' \
          --cpu=${nativeBySystem.${target}.bunCpu} --os=linux
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

      # The fixed-output hash pins the installed tree, not only versions. Ignore
      # install scripts and strip host-varying `.bin` shims for reproducibility.
      # Refresh with `lib.fakeSha256` after lockfile or manifest changes.
      dontFixup = true;
      outputHashMode = "recursive";
      outputHashAlgo = "sha256";
      outputHash = outputHashBySystem.${target};
    };
  nodeModules = nodeModulesFor system;
  nodeModulesBySystem = lib.genAttrs (builtins.attrNames outputHashBySystem) nodeModulesFor;

  # A denylist (`bun build` only errors on a statically-resolvable missing
  # import, so an allowlist would silently drop a new fixture). Shared with
  # source-fingerprint.nix so the stale-image check hashes exactly these files.
  pkgSrc = lib.fileset.toSource {
    root = ../packages/compass-agent;
    fileset = import ./agent-source-files.nix { inherit lib; };
  };

  # Reconstruct the workspace at the installed depth so member symlinks resolve.
  # Merge copies into existing directories; plain `cp -R src dst` nests
  # `node_modules` when the destination exists.
  bundle = pkgs.runCommand "compass-agent-bundle" { nativeBuildInputs = [ pinnedBun ]; } ''
    export HOME=$TMPDIR
    pkgDir=packages/compass-agent

    mkdir -p $pkgDir
    cp -R ${pkgSrc}/. $pkgDir/
    chmod -R +w $pkgDir

    mkdir -p node_modules $pkgDir/node_modules
    cp -R ${nodeModules}/node_modules/. node_modules/
    cp -R ${nodeModules}/$pkgDir/node_modules/. $pkgDir/node_modules/

    # scripts/compile.ts bakes the SDK's `omp-legacy-pi-modules` registry into the
    # binary through Compass's local copy of the SDK build plugin, resolving roots
    # from this reconstructed workspace's installed tree. The binary uses it to load
    # legacy Pi extensions, so it must not be left external.
    bun $pkgDir/scripts/compile.ts $out/compass-agent
    # The prebuilt addon ships in the per-system optionalDependency
    # `@oh-my-pi/pi-natives-linux-<arch>` (pinned in bun.lock, so in the FOD tree),
    # hoisted into `.bun/node_modules/@oh-my-pi/`. x64 carries two AVX2 variants
    # (modern/baseline); arm64 carries one. `cp` follows the hoist symlink.
    natives=node_modules/.bun/node_modules/@oh-my-pi/${native.nativesPkg}
    ${lib.concatMapStringsSep "\n" (f: "cp $natives/${f} $out/") native.addons}
  '';
in
# The bundled binary runs directly and finds native addons beside it.
# Expose both node-modules derivations so hash checks can realize the FOD alone.
lib.extendDerivation true { inherit nodeModules nodeModulesBySystem; } (
  pkgs.writeShellScriptBin "compass-agent" ''
    exec ${bundle}/compass-agent "$@"
  ''
)
