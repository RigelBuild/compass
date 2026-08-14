# Exact bun/node/moon toolchains vendored as Nix derivations. Both the dev
# shell (devenv.nix) and the CI toolchain gate (tools/toolchain/gate-tools.nix)
# import this one module, so the two cannot drift: on Linux both sides resolve
# byte-identical store paths, and the aarch64-darwin legs give macOS developers
# the same pinned versions from the per-platform Apple-Silicon assets.
#
# Versions come from tools/toolchain/versions/<lang>.nix (the single source of
# truth); bump the hash there when a version changes — the fetch fails loudly
# on the resulting mismatch, so nothing silently drifts.
{ pkgs }:
let
  inherit (pkgs) lib stdenv;

  # Per-arch toolchain selection: each tool's upstream asset is picked by the
  # build host's arch (`hostPlatform`). Every leg pins its own hash; `fetchurl`
  # fails loudly on a mismatch, so a wrong-arch asset can't silently ship.
  bunPin = import ./versions/bun.nix;
  nodePin = import ./versions/node.nix;
  moonPin = import ./versions/moon.nix;
in
{
  bun = pkgs.stdenv.mkDerivation {
    pname = "bun";
    version = bunPin.version;
    # bun's `-baseline` (no-AVX2) x64 build runs on pre-Haswell hosts where the
    # AVX2-only standard x64 build SIGILLs. ARM has no AVX2, so the aarch64 leg
    # uses the plain build.
    src = pkgs.fetchurl bunPin.srcs.${pkgs.stdenv.hostPlatform.system};
    # autoPatchelf is linux-ELF mechanics; darwin ships a self-contained Mach-O.
    # unzip is needed on both (the asset is a .zip on every platform).
    nativeBuildInputs = [
      pkgs.unzip
    ]
    ++ lib.optionals stdenv.isLinux [ pkgs.autoPatchelfHook ];
    buildInputs = lib.optionals stdenv.isLinux [ pkgs.stdenv.cc.cc.lib ];
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      runHook preInstall
      install -Dm755 "$(find . -name bun -type f | head -1)" "$out/bin/bun"
      # bunx is argv[0]-dispatched to `bun x`; the .moon bun lint tasks call it.
      ln -s bun "$out/bin/bunx"
      runHook postInstall
    '';
  };

  node = pkgs.stdenv.mkDerivation {
    pname = "node";
    version = nodePin.version;
    # darwin ships a .tar.gz (not the linux .tar.xz); stdenv unpacks both.
    src = pkgs.fetchurl nodePin.srcs.${pkgs.stdenv.hostPlatform.system};
    # autoPatchelf + the C++ runtime are linux-ELF mechanics; the darwin build is
    # a self-contained Mach-O that needs neither.
    nativeBuildInputs = lib.optionals stdenv.isLinux [ pkgs.autoPatchelfHook ];
    buildInputs = lib.optionals stdenv.isLinux [ pkgs.stdenv.cc.cc.lib ];
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      runHook preInstall
      mkdir -p "$out"
      for d in bin lib include share; do
        [ -d "$d" ] && cp -r "$d" "$out/"
      done
      runHook postInstall
    '';
  };

  moon = pkgs.stdenv.mkDerivation {
    pname = "moon";
    version = moonPin.version;
    src = pkgs.fetchurl moonPin.srcs.${pkgs.stdenv.hostPlatform.system};
    # Statically linked (musl on linux, self-contained Mach-O on darwin) — no
    # autoPatchelf needed on either platform.
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      runHook preInstall
      install -Dm755 "$(find . -name moon -type f | head -1)" "$out/bin/moon"
      runHook postInstall
    '';
  };
}
