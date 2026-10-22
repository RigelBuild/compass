{
  pkgs,
  lib,
  version,
  # A deployment that serves the UI on a different origin from the door bakes
  # the door here; null keeps the default of resolving it at boot.
  baseUrl ? null,
}:
# Package the UI with the executable, using repo-root workspace paths.
let
  # Pin the same Bun version used by development and CI.
  pinnedBun = (import ../../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;

  # Install the UI and source-only client workspaces.
  members = [
    "@compass/ui"
    "@compass/client"
  ];

  # Fetch the locked dependency closure in the only networked derivation.
  nodeModules = pkgs.stdenv.mkDerivation {
    pname = "compass-ui-node-modules";
    # The lockfile hash defines this FOD's contents; keep its output name stable.
    version = "0.1.0";

    # Bun resolves all workspace manifests before filtering.
    # Exclude bunfig.toml so minimum-release-age policy stays deterministic.
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

    nativeBuildInputs = [ pinnedBun ];
    dontConfigure = true;

    # Hoisted linking avoids unstable aliases and preserves bundle resolution.
    buildPhase = ''
      runHook preBuild
      # Keep Bun's cache and metadata writable inside the build.
      export HOME=$TMPDIR
      export BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache
      bun install --frozen-lockfile --ignore-scripts --linker=hoisted \
        ${lib.concatMapStringsSep " " (m: "--filter '${m}'") members}
      runHook postBuild
    '';

    # Workspace links in the hoisted node_modules tree must point to source during the build.
    installPhase = ''
      runHook preInstall
      mkdir -p $out
      cp -R node_modules $out/node_modules
      # Remove .bin links: Vite is invoked through its entry point below.
      find $out -type d -name .bin -prune -exec rm -rf {} +
      runHook postInstall
    '';

    # Refresh this hash when the lock, manifests, or Bun pin changes.
    dontFixup = true;
    outputHashMode = "recursive";
    outputHashAlgo = "sha256";
    outputHash = "sha256-u6eh2EDQoFVa9A6XZ9wsPFFBbz1FrT2otR8cOJxEqcE=";
  };

  # Denylist source exclusions; dotenv filtering is needed because filesets ignore gitignore.
  dotenvFiles = dir: lib.fileset.fileFilter (f: lib.hasPrefix ".env" f.name) dir;

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
    nativeBuildInputs = [ pinnedBun ];
  }
  ''
    export HOME=$TMPDIR
    export BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache

    # Merge workspace contents into pre-made directories to avoid nested node_modules.
    mkdir -p apps/ui packages/compass-client
    cp -R ${uiSrc}/. apps/ui/
    cp -R ${clientSrc}/. packages/compass-client/
    chmod -R +w apps packages

    # Copy the workspace manifest and shared tsconfig required by apps/ui.
    cp ${../../package.json} package.json
    cp ${../../tsconfig.base.json} tsconfig.base.json

    mkdir -p node_modules
    cp -R ${nodeModules}/node_modules/. node_modules/
    chmod -R +w node_modules

    # Invoke pinned Vite directly to avoid bunx network access and .bin shims.
    # Build from apps/ui, where Vite config and index.html live.
    cd apps/ui
    # Node does not autoload .env, but Bun does; unset both build-time values before Vite starts.
    # The source filter blocks file values, and the unset clears inherited environment values.
    unset VITE_COMPASS_BASE_URL VITE_COMPASS_TOKEN${
      lib.optionalString (baseUrl != null) "\nexport VITE_COMPASS_BASE_URL=${lib.escapeShellArg baseUrl}"
    }
    bun ../../node_modules/vite/bin/vite.js build --mode production
    cd ../..

    # Fail if Vite silently emits no bundle because index.html was excluded.
    if [ ! -f apps/ui/dist/index.html ]; then
      echo "ERROR: vite build produced no apps/ui/dist/index.html" >&2
      exit 1
    fi

    # Ensure .env.development's loopback URL is not baked into the bundle.
    if grep -rq '127\.0\.0\.1:50051' apps/ui/dist; then
      echo "ERROR: the .env.development dev door is baked into the bundle" >&2
      exit 1
    fi

    # Stage the built dist contents as the package output.
    mkdir -p $out
    cp -R apps/ui/dist/. $out/
  ''
