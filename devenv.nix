{
  pkgs,
  lib,
  inputs,
  ...
}:
# Compass dev shell — the single source of the dev + CI toolchain. Compass is
# OSS and self-contained (it does not extend another repo's shell). The split,
# per docs/architecture/build-and-ci.md:
#
#   proto  — the bun/node/moon runtimes, pinned in .prototools. Activated on
#            shell entry below.
#   fenix  — the exact Rust toolchain from rust-toolchain.toml, shared by the
#            dev shell and the CI image so both run one pinned Rust.
#   devenv — proto + fenix plus everything non-language: the protobuf/contract
#            toolchain (buf, protoc, the Rust prost/tonic codegen plugins), the
#            Rust dev tools that aren't the compiler (cargo-deny, cargo-nextest,
#            sccache), and the lint tools the pre-push + CI gates run.
#
# proto bootstraps bun/node/moon standalone; Rust comes from fenix (nix) here,
# or from rustup reading rust-toolchain.toml on the no-nix path.
let
  # Exact Rust toolchain (channel + components) from rust-toolchain.toml, built
  # by fenix. The dev shell and the CI image (ci/ci-toolchain.nix) share this
  # one derivation, so rust-toolchain.toml is the single source of truth.
  rustToolchain = inputs.fenix.packages.${pkgs.stdenv.system}.fromToolchainFile {
    file = ./rust-toolchain.toml;
    sha256 = "sha256-gh/xTkxKHL4eiRXzWv8KP7vfjSk61Iq48x47BEDFgfk=";
  };
in
{
  packages =
    with pkgs;
    [
      # Runtime manager: installs bun/node/moon per .prototools on shell entry.
      proto

      # Contract toolchain: protobuf schema lint/breaking + the single
      # buf codegen pipeline (Rust messages + gRPC services).
      buf
      protobuf # protoc
      protoc-gen-prost # Rust message codegen plugin
      protoc-gen-tonic # Rust gRPC service codegen plugin

      # Rust: the exact rust-toolchain.toml toolchain (fenix) + a C linker so
      # cargo can link test/bin crates, then the cargo dev tools.
      rustToolchain
      stdenv.cc # cc/ld for cargo's link step
      cargo-deny # license + contract dep-ban fence
      cargo-nextest # test runner (JUnit `ci` profile)
      sccache # Rust compile cache

      # Lint/format for the non-Rust, non-TS surfaces the gates cover.
      actionlint
      nixfmt-rfc-style
      shellcheck
      shfmt
      taplo
    ]
    # Linux-only: the Woodpecker step backend + `devenv container build`.
    ++ lib.optionals pkgs.stdenv.isLinux [ podman ];

  # sccache as the Rust compiler wrapper (the per-compilation-unit cache that
  # sits inside the cargo task, independent of moon's task-output cache).
  env.RUSTC_WRAPPER = "${pkgs.sccache}/bin/sccache";

  enterShell = ''
    # Activate the proto-managed runtimes (bun/node/moon) — Rust is from fenix
    # (packages). The shims dir is ahead on PATH so a bare `bun`/`moon`
    # resolves these pins, not a host install.
    export PROTO_HOME="''${PROTO_HOME:-$HOME/.proto}"
    export PATH="$PROTO_HOME/shims:$PROTO_HOME/bin:$PATH"
    ${pkgs.proto}/bin/proto install
    # Append the bun workspace bin dir so `buf generate` finds the node-based
    # protoc-gen-es plugin (and biome) after `bun install`. Appended, not
    # prepended, so the devenv/proto toolchains above still win.
    export PATH="$PATH:''${DEVENV_ROOT:-$PWD}/node_modules/.bin"
  '';

  # CI step image: the OCI image the Woodpecker agents run `moon run :ci` in, so
  # CI and local share one toolchain. Woodpecker command steps run `/bin/sh -c`
  # and bypass the entrypoint, so the toolchain can't be activated by enterShell
  # — it's baked onto /bin via a container layer (ci/ci-toolchain.nix). copyToRoot
  # = [ ] keeps the repo out of the image (Woodpecker checks it out per job), so
  # no sources leak into a pullable image. Build + push with ci/publish-ci-image.sh.
  #
  # Linux-only: the layer vendors linux-x64 bun/node/moon, so guard the whole
  # container off Darwin dev shells / direnv evaluation.
  containers = lib.optionalAttrs pkgs.stdenv.isLinux {
    ci = {
      name = "compass-ci";
      copyToRoot = [ ];
      # A container layer's copyToRoot relocates store paths to the image root,
      # landing the toolchain on /bin where a command step's default PATH finds it.
      layers = [
        { copyToRoot = [ (import ./ci/ci-toolchain.nix { inherit pkgs rustToolchain; }) ]; }
      ];
    };
  };
}
