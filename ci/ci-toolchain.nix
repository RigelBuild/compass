# The compass CI step image's toolchain, assembled as a /bin tree.
#
# Woodpecker command steps run `/bin/sh -c` and bypass the image entrypoint, so
# the toolchain can't be activated by devenv's enterShell — it's baked onto /bin
# via a container layer (devenv.nix: containers.ci) using this derivation.
#
# bun/node/moon are the exact .prototools pins (single source of truth with the
# dev shell), fetched and autoPatchelf'd because proto can't run during a
# hermetic build. Bump the hashes here when .prototools changes — the fetch
# fails loudly on the mismatch, so the image can't silently drift from local
# dev. The Rust toolchain comes from nixpkgs; pinning it to rust-toolchain.toml
# exactly (e.g. via fenix) is deferred to CI activation, when version parity
# starts to matter — the contract drift gate is buf-driven and
# Rust-version-independent.
#
# protoc-gen-es, biome, tsc and vite are NOT baked in: they install with the
# rest of the bun workspace deps (`bun install`) at pipeline time.
{ pkgs }:
let
  protoTools = builtins.fromTOML (builtins.readFile ../.prototools);

  bun = pkgs.stdenv.mkDerivation {
    pname = "bun";
    version = protoTools.bun;
    src = pkgs.fetchurl {
      url = "https://github.com/oven-sh/bun/releases/download/bun-v${protoTools.bun}/bun-linux-x64.zip";
      hash = "sha256-ecB3H6i5LDOq5B4VoODTB+qZ0OLwAxfHHGxTI3p44lo=";
    };
    nativeBuildInputs = [
      pkgs.unzip
      pkgs.autoPatchelfHook
    ];
    buildInputs = [ pkgs.stdenv.cc.cc.lib ];
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      runHook preInstall
      install -Dm755 "$(find . -name bun -type f | head -1)" "$out/bin/bun"
      # bunx is argv[0]-dispatched to `bun x`; the bun moon tasks call it.
      ln -s bun "$out/bin/bunx"
      runHook postInstall
    '';
  };

  nodejs = pkgs.stdenv.mkDerivation {
    pname = "nodejs";
    version = protoTools.node;
    src = pkgs.fetchurl {
      url = "https://nodejs.org/dist/v${protoTools.node}/node-v${protoTools.node}-linux-x64.tar.xz";
      hash = "sha256-l0npiPQ3NDt/qDLGne2CoxLkGgMRbXZnl6wU9vnu5Xg=";
    };
    nativeBuildInputs = [ pkgs.autoPatchelfHook ];
    buildInputs = [ pkgs.stdenv.cc.cc.lib ];
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
    version = protoTools.moon;
    src = pkgs.fetchurl {
      url = "https://github.com/moonrepo/moon/releases/download/v${protoTools.moon}/moon_cli-x86_64-unknown-linux-musl.tar.xz";
      hash = "sha256-JmQJiKi7Whm2JGk8/bcaAyid2sr5C1aLBG/TEXjHApE=";
    };
    # Statically linked (musl) — no autoPatchelf needed.
    dontConfigure = true;
    dontBuild = true;
    installPhase = ''
      runHook preInstall
      install -Dm755 "$(find . -name moon -type f | head -1)" "$out/bin/moon"
      runHook postInstall
    '';
  };

  # Woodpecker command steps + moon `script` tasks run via /bin/sh, and node
  # tools (e.g. protoc-gen-es) use `#!/usr/bin/env node` shebangs. A buildEnv of
  # bash gives /bin/bash, not /bin/sh, and no /usr/bin/env — symlink both.
  links = pkgs.runCommand "compass-ci-links" { } ''
    mkdir -p "$out/bin" "$out/usr/bin"
    ln -s ${pkgs.bashInteractive}/bin/bash "$out/bin/sh"
    ln -s ${pkgs.coreutils}/bin/env "$out/usr/bin/env"
  '';
in
pkgs.buildEnv {
  name = "compass-ci-toolchain";
  paths = [
    bun
    nodejs
    moon
    # Rust toolchain (compiler + the lint/format/test extensions the moon tasks
    # invoke) and the cargo plugins the contract + crate gates use.
    pkgs.cargo
    pkgs.rustc
    pkgs.clippy
    pkgs.rustfmt
    pkgs.cargo-nextest
    pkgs.cargo-deny
    pkgs.sccache
    # Contract toolchain: buf + protoc + the Rust codegen plugins (buf resolves
    # protoc-gen-es from node_modules at pipeline time).
    pkgs.buf
    pkgs.protobuf
    pkgs.protoc-gen-prost
    pkgs.protoc-gen-tonic
    # git for moon's VCS/affected detection + the contract drift task's git diff.
    pkgs.git
    # A POSIX shell + core utilities: Woodpecker command steps and the moon
    # `script` tasks (e.g. contract drift) run through /bin/sh and shell out to
    # mktemp/diff/find/grep. buildEnv links only each package's /bin, so list them.
    links
    pkgs.bashInteractive
    pkgs.coreutils
    pkgs.diffutils
    pkgs.findutils
    pkgs.gnugrep
    # The nix/shell/toml/yaml lint tasks moon run :ci invokes.
    pkgs.nixfmt-rfc-style
    pkgs.shellcheck
    pkgs.shfmt
    pkgs.taplo
    pkgs.actionlint
  ];
  # /bin only — the image just needs the toolchain on PATH for command steps.
  pathsToLink = [
    "/bin"
    "/usr/bin"
  ];
}
