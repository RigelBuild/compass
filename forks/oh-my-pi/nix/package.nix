# Sealed omp build recipe (SEA-1242 T1b, Option C). Adapted from
# numtide/llm-agents.nix `packages/omp/package.nix` (locked rev
# 6e9f6664e72433966557ab83a7c3e8c0bbf64a44), with one deliberate change: `src`
# is this fork tree itself (the sibling `flake.nix` passes `src = self`), not a
# `fetchFromGitHub` of upstream `can1357/oh-my-pi`. This builds sealed's fork —
# which carries the auth-broker `/metrics` exposition the `llm_usage_*` dashboard
# scrapes (SEA-1242) plus the composite tool-call-id pairing fix (SEA-1160) — on
# Matt's schedule, decoupled from numtide's release cadence. `hashes.json` pins
# the fork VERSION + `cargoHash`; `bun.nix` is regenerated (bun2nix) for the
# fork's `bun.lock`. Keep this recipe in sync with numtide's build fixes on
# subtree bumps.
{
  lib,
  stdenv,
  src,
  bun2nix,
  bun,
  rustc,
  cargo,
  rustPlatform,
  pkg-config,
  makeWrapper,
  autoPatchelfHook,
  zlib,
  libclang,
  python3,
  zig,
}:

let
  versionData = builtins.fromJSON (builtins.readFile ./hashes.json);
  # `src` is passed by the caller (the in-tree fork subtree); only version +
  # cargoHash come from hashes.json now (the upstream tarball `hash` is unused).
  inherit (versionData) version cargoHash;
  platformsBySystem = {
    aarch64-darwin = {
      bunTarget = "bun-darwin-arm64";
      nativeLib = "libpi_natives.dylib";
      nodeTag = "darwin-arm64";
    };
    aarch64-linux = {
      bunTarget = "bun-linux-arm64";
      nativeLib = "libpi_natives.so";
      nodeTag = "linux-arm64";
    };
    x86_64-darwin = {
      bunTarget = "bun-darwin-x64";
      nativeLib = "libpi_natives.dylib";
      nodeTag = "darwin-x64";
    };
    x86_64-linux = {
      # `bun compile` embeds this runtime into the omp binary, and
      # installCheckPhase smoke-tests that binary on the BUILD host. The `nix`
      # project pins to the CI static hosts, which include mattlinuxpro (2013
      # Mac Pro Xeon, pre-Haswell, no AVX2) — so the `-modern` (AVX2) runtime
      # SIGILLs at smoke-test time. Use `-baseline`: it runs on the non-AVX2
      # build host AND on mattfw (Zen 5, has AVX2) at negligible cost for an
      # I/O-bound coding agent. See the sibling flake.nix note (build-tool bun)
      # and ci/toolchain/ci-toolchain.nix (same constraint, same fix).
      bunTarget = "bun-linux-x64-baseline";
      nativeLib = "libpi_natives.so";
      nodeTag = "linux-x64";
    };
  };
  platform =
    platformsBySystem.${stdenv.hostPlatform.system}
      or (throw "Unsupported platform for omp: ${stdenv.hostPlatform.system}");
  rustTarget = stdenv.hostPlatform.rust.rustcTarget;
  rustTargetEnv = "CARGO_TARGET_${
    lib.toUpper (builtins.replaceStrings [ "-" ] [ "_" ] rustTarget)
  }_RUSTFLAGS";
  glimmerRustFlags = lib.concatStringsSep " " [
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_create"
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_destroy"
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_reset"
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_scan"
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_serialize"
    "-Clink-arg=-Wl,-u,tree_sitter_glimmer_external_scanner_deserialize"
  ];

  # `src` is a function argument (the fork subtree), not fetched from upstream.
in
stdenv.mkDerivation {
  pname = "omp";
  inherit version src;

  cargoDeps = rustPlatform.fetchCargoVendor {
    name = "omp-${version}-cargo-vendor";
    inherit src;
    hash = cargoHash;
  };

  nativeBuildInputs = [
    bun2nix.hook
    bun
    rustc
    cargo
    rustPlatform.cargoSetupHook
    pkg-config
    makeWrapper
    zig
  ]
  ++ lib.optionals stdenv.hostPlatform.isLinux [ autoPatchelfHook ];

  buildInputs = lib.optionals stdenv.hostPlatform.isLinux [
    stdenv.cc.cc.lib
    zlib
  ];

  # smallvec's `specialization` feature requires nightly Rust.
  # RUSTC_BOOTSTRAP=1 enables nightly features on stable rustc.
  env = {
    RUSTC_BOOTSTRAP = 1;
    ${rustTargetEnv} = glimmerRustFlags;
  };

  bunDeps = bun2nix.fetchBunDeps {
    # bun.nix lives at the fork ROOT (not beside this recipe): its
    # `copyPathToStore ./packages/*` entries are path literals resolved relative
    # to bun.nix's own dir, so it must sit at the workspace root where
    # `packages/` lives.
    bunNix = ../bun.nix;
  };

  # Drop robomp-web workspace: its devDependencies aren't needed for the CLI.
  postUnpack = ''
        rm -rf $sourceRoot/python/robomp/web
        ROOT="$sourceRoot" ${lib.getExe python3} -c "
    import json, re, os
    root = os.environ['ROOT']

    with open(f'{root}/package.json') as f:
        pkg = json.load(f)
    ws = pkg.get('workspaces', {})
    if isinstance(ws, dict) and 'packages' in ws:
        ws['packages'] = [w for w in ws['packages'] if 'robomp/web' not in w]
    elif isinstance(ws, list):
        pkg['workspaces'] = [w for w in ws if 'robomp/web' not in w]
    with open(f'{root}/package.json', 'w') as f:
        json.dump(pkg, f, indent=2)
        f.write('\\n')

    # bun.lock uses trailing commas (JSONC), strip them for stdlib json
    with open(f'{root}/bun.lock') as f:
        text = re.sub(r',\s*([}\]])', r'\1', f.read())
    lock = json.loads(text)
    lock.get('workspaces', {}).pop('python/robomp/web', None)
    lock.get('packages', {}).pop('robomp-web', None)
    for k in list(lock.get('packages', {})):
        if k.startswith('robomp-web/'):
            del lock['packages'][k]
    with open(f'{root}/bun.lock', 'w') as f:
        json.dump(lock, f, indent=2)
        f.write('\\n')
    "
  '';

  # We handle build and install ourselves
  dontUseBunBuild = true;
  dontUseBunInstall = true;
  dontRunLifecycleScripts = true;

  # bun compile embeds JS in the binary; stripping would break it
  dontStrip = true;

  postPatch = ''
    # Strip ^ and ~ prefixes: bun resolves range specifiers via the npm
    # registry, which is unreachable in the sandbox.
    for f in package.json packages/*/package.json; do
      if [ -f "$f" ]; then
        sed -i 's/: "\^/: "/g; s/: "~/: "/g' "$f"
      fi
    done
    sed -i 's/: "\^/: "/g; s/: "~/: "/g' bun.lock

    # Relax engines.bun to the bun doing the compile, otherwise omp refuses
    # to start when the embedded runtime is older than upstream's minimum
    # (issue #4996).
    sed -i 's/"bun": ">=[0-9.]*"/"bun": ">='"$(bun --version)"'"/' \
      packages/utils/package.json

    # swarm-extension pins @oh-my-pi/pi-coding-agent to a stale major, which
    # bun can't satisfy locally and would fetch from npm. Use the workspace
    # reference instead.
    sed -i 's|"@oh-my-pi/pi-coding-agent": "[0-9][^"]*"|"@oh-my-pi/pi-coding-agent": "workspace:*"|' \
      packages/swarm-extension/package.json bun.lock

    # Placeholder client bundle avoids building the full React dashboard.
    cat > packages/stats/src/embedded-client.generated.txt <<'PLACEHOLDER'
    export const EMBEDDED_CLIENT_ARCHIVE_TAR_GZ_BASE64 = "";
    PLACEHOLDER
  '';

  buildPhase = ''
    runHook preBuild

    # Native node modules like @napi-rs/cli need libstdc++ at build time
    ${lib.optionalString stdenv.hostPlatform.isLinux ''
      export LD_LIBRARY_PATH="${lib.makeLibraryPath [ stdenv.cc.cc.lib ]}"
    ''}

    # bindgen (used by zlob crate) needs libclang
    export LIBCLANG_PATH="${libclang.lib}/lib"

    # Build the Rust native addon
    echo "Building Rust native addon..."
    cargo build --release -p pi-natives --target ${rustTarget} --target-dir target

    # Install the native addon where the JS code expects it
    mkdir -p packages/natives/native
    cp target/${rustTarget}/release/${platform.nativeLib} \
       packages/natives/native/pi_natives.${platform.nodeTag}.node

    # Generate the napi type definitions and JS loader
    napiBin="$(pwd)/node_modules/.bin/napi"
    if [ -x "$napiBin" ]; then
      "$napiBin" build \
        --manifest-path crates/pi-natives/Cargo.toml \
        --package-json-path packages/natives/package.json \
        --platform \
        --no-js \
        --dts index.d.ts \
        -o packages/natives/native \
        --release \
        || echo "napi CLI post-processing failed; using cargo output directly"
    fi

    # Generate runtime enum exports from const enums in the type definitions
    if [ -f packages/natives/scripts/gen-enums.ts ] && \
       [ -f packages/natives/native/index.d.ts ]; then
      bun packages/natives/scripts/gen-enums.ts || true
    fi

    # --generate embeds the omp:// docs index; without it the script is a no-op
    # and the binary ships no docs, breaking omp:// reads.
    echo "Generating docs index..."
    bun packages/coding-agent/scripts/generate-docs-index.ts --generate

    # Generate the embedded stats dashboard client bundle. Bun.Archive.write
    # stamps each tar header with the current time, so normalize the archive
    # afterwards to keep the compiled binary reproducible (issue #6534).
    echo "Generating embedded stats dashboard..."
    bun --cwd packages/stats scripts/generate-client-bundle.ts --generate
    bun ${./normalize-embedded-client.ts} \
      packages/stats/src/embedded-client.generated.txt

    # Generate the embedded HTML-export tool-views bundle (coding-agent prepack
    # step): export/html/index.ts text-imports ./tool-views.generated.js, which
    # bun compile cannot resolve unless it is generated first.
    echo "Generating embedded HTML-export tool-views..."
    bun --cwd packages/collab-web scripts/build-tool-views.ts

    # Since v16.4.6 mupdf is bundled into the binary and its wasm blob is
    # embedded via a generated helper (upstream gen:mupdf); --external mupdf no
    # longer works because the compiled bunfs cannot resolve it.
    echo "Embedding mupdf wasm..."
    bun packages/coding-agent/scripts/embed-mupdf-wasm.ts --generate

    # Compile the standalone binary. Since v16.4.6 the binary needs the
    # in-memory omp-legacy-pi-modules virtual module, which only the Bun.build()
    # plugin from upstream's compile-binary.ts can provide, so drive that helper
    # (compile-standalone.ts) instead of `bun build --compile`.
    echo "Compiling standalone binary..."
    (cd packages/coding-agent && bun ${./compile-standalone.ts} "${platform.bunTarget}")

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall

    mkdir -p $out/lib/omp $out/bin
    cp dist/omp $out/lib/omp/omp
    # Ship the plain addon name: native.ts probes for it on every arch.
    cp packages/natives/native/pi_natives.${platform.nodeTag}.node $out/lib/omp/

    makeWrapper $out/lib/omp/omp $out/bin/omp \
      --set PI_SKIP_VERSION_CHECK 1 \
    ${lib.optionalString stdenv.hostPlatform.isLinux "--prefix LD_LIBRARY_PATH : ${
      lib.makeLibraryPath [
        zlib
        stdenv.cc.cc.lib
      ]
    }"}

    runHook postInstall
  '';

  # Workers and the stats dashboard only fail at runtime when their bunfs
  # entrypoints are missing; the smoke test catches that at build time.
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    HOME=$TMPDIR $out/bin/omp --smoke-test | grep -q "smoke-test: ok"
    # Guard the sealed feature-on-fork this build exists to ship (SEA-1242): the
    # auth-broker scrape-scoped /metrics token surface. Offline flag check — the
    # live endpoint needs a running broker + bound port (impractical here), so
    # assert the CLI surface so an upstream resync that drops it fails the build.
    HOME=$TMPDIR $out/bin/omp auth-broker token --help 2>&1 | grep -q -- --metrics
    runHook postInstallCheck
  '';

  passthru.category = "AI Coding Agents";

  meta = with lib; {
    description = "A terminal-based coding agent with multi-model support";
    homepage = "https://github.com/can1357/oh-my-pi";
    changelog = "https://github.com/can1357/oh-my-pi/releases/tag/v${version}";
    license = licenses.mit;
    sourceProvenance = with lib.sourceTypes; [ fromSource ];
    maintainers = with maintainers; [ aldoborrero ];
    mainProgram = "omp";
    platforms = builtins.attrNames platformsBySystem;
  };
}
