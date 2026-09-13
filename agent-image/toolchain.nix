# The in-image toolchain for the Compass agent base image.
#
# A *runtime* toolchain for a coding agent, not a build image: it needs Nix and a
# shell, not the project's compile-time closure. It shares one thing with the rest
# of the repo — the pinned bun derivation the dev shell and CI gate build
# (tools/toolchain/toolchain-tools.nix) — rather than re-pinning.
#
# What the agent needs in-image, and why each is here:
#   * nix — the agent rebuilds its own devenv in-container as itself; devenv's
#     containers module ships no `nix` (it assumed a CI host that has it).
#   * devenv + direnv — the activation path, driven by the agent. Nothing wraps
#     the entrypoint in `direnv exec`, so the agent activates a checkout itself.
#     The image also bakes a direnvrc loading devenv's `use devenv` stdlib and
#     points DIRENV_CONFIG at it (`direnvConfig` below).
#   * bun — runs `compass-agent`, a TypeScript entrypoint.
#   * git + gh — the agent clones its own repos and drives forge work.
#   * nftables/getent/gawk — required in-image by the egress arm. getent is its
#     own nixpkgs package, NOT part of glibc/glibc.bin (neither ships bin/getent).
#   * coreutils/bash/cacert — a usable shell and the CA bundle every HTTPS clone +
#     nix substitution needs (without it the first substitution fails on TLS).
{
  pkgs,
  compassAgent,
}:
let
  # The repo's pinned bun — the exact vendored derivation the dev shell and CI
  # gate import, so the image IS the pin byte for byte, not a nixpkgs bun that
  # merely matches.
  bun = (import ../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;

  # Single-user Nix, set up for the agent uid with `/nix` owned by the agent.
  # Each setting is load-bearing:
  #   build-users-group = (empty) — the single-user switch; a non-empty group with
  #     no daemon aborts every build, and there is no daemon here by design.
  #   sandbox = false — the build sandbox needs nested userns, unreliable inside a
  #     rootless podman userns; the agent is trusted, so it gains nothing.
  #   experimental-features — devenv is flake-based; without this it fails.
  #   substituters — cold-realizing from the public cache is accepted cost.
  #   ssl-cert-file — pinned to the bundle's STORE path: nix's compiled-in default
  #     names a filename `pkgs.cacert` does not ship, so without this every
  #     substitution dies with an opaque SSL CA error. A store path is immune to
  #     however /etc ends up laid out.
  nixConf = pkgs.writeTextDir "etc/nix/nix.conf" ''
    experimental-features = nix-command flakes
    build-users-group =
    sandbox = false
    substituters = https://cache.nixos.org
    trusted-public-keys = cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=
    ssl-cert-file = ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
  '';

  # The same bundle under the filename everything OTHER than nix looks for.
  # `pkgs.cacert` ships only `ca-bundle.crt`, but openssl (curl/git/gh/ssh)
  # defaults to `ca-certificates.crt`. `ssl-cert-file` covers nix alone, so
  # without this the agent's own HTTPS clones still fail. Same bundle, second name.
  caCertificates = pkgs.runCommand "ca-certificates" { } ''
    mkdir -p $out/etc/ssl/certs
    ln -s ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
          $out/etc/ssl/certs/ca-certificates.crt
  '';

  # The devenv stdlib for direnv, so `use devenv` resolves in-image. Shipping the
  # binaries is not enough: direnv's stdlib has no `use_devenv` — devenv supplies
  # it as a fragment printed by `devenv direnvrc` that an `.envrc` is expected to
  # `eval`. A bare `use devenv` .envrc (what a trimmed repo ends up with) fails
  # `use_devenv: command not found` and direnv silently degrades to this image's
  # tools. Loading the fragment from direnv's own rc makes every `.envrc` shape
  # work without the agent editing cloned repos.
  #
  # It lands in /etc, not $HOME: the container module stages a real `/home/agent`
  # that wins over anything this buildEnv symlinks there, so DIRENV_CONFIG points
  # direnv at this store-backed rc instead, leaving the agent's ~/.config
  # untouched. A COLD activation prints a few `command not found` lines — devenv's
  # own `_nix_import_env` evals the printed environment, whose progress lines bash
  # tries to run; the environment is fully activated regardless, so an agent
  # reading its own output should not read these as a broken toolchain.
  direnvConfig = pkgs.writeTextDir "etc/direnv/direnvrc" ''
    eval "$(${pkgs.devenv}/bin/devenv direnvrc)"
  '';
in
pkgs.buildEnv {
  name = "compass-agent-toolchain";
  paths = [
    # The agent's own Nix: the whole point of the self-contained image.
    pkgs.nix
    pkgs.devenv
    pkgs.direnv

    # The entrypoint and its interpreter.
    bun
    compassAgent

    # The agent clones and drives its own repos.
    pkgs.git
    pkgs.gh
    pkgs.openssh

    # Egress arm step's in-image requirements (egress.go:76-77).
    pkgs.nftables
    pkgs.getent
    pkgs.gawk

    # A usable base environment.
    pkgs.bashInteractive
    pkgs.coreutils-full
    pkgs.cacert

    nixConf
    caCertificates
    direnvConfig
  ];
  pathsToLink = [
    "/bin"
    "/etc"
  ];
}
