# The in-image toolchain for the Compass agent base image.
#
# This is a *runtime* toolchain for a coding agent, not a build image: it needs
# Nix and a shell, not the project's compile-time dependency closure. What it
# does share with the rest of the repo is the `.prototools` bun version, which is
# read here and asserted against nixpkgs' bun rather than re-pinned — see `bun`
# below.
#
# What the agent needs in-image, and why each is here rather than assumed:
#
#   * nix — the agent rebuilds its own devenv in-container as itself, so the
#     `nix` CLI is the load-bearing tool, not an extra. devenv's containers
#     module ships no `nix` (it was built for "CI runs nix builds", where the
#     host has it).
#   * devenv + direnv — the activation path, driven by the agent rather than by
#     the Runner. The Runner execs a bare `compass-agent` argv
#     (go/internal/runner/relay.go:37,65-67), so nothing wraps the entrypoint in
#     `direnv exec`; the agent activates a checkout itself when it needs the
#     repo's toolchain. The two binaries alone do not make that path work:
#     `use devenv` is devenv's own direnv stdlib, not direnv's, so the image
#     also bakes a direnvrc that loads it and points `DIRENV_CONFIG` at it
#     (`direnvConfig` below, and devenv.nix's `env`).
#   * bun — runs `compass-agent`, which is a TypeScript entrypoint.
#   * git + gh — the agent clones its own repos rather than being handed a
#     checkout, and drives forge work.
#   * nftables/getent/gawk — required IN-IMAGE by the root egress arm step:
#     "Requires nft, getent, and awk in the image"
#     (go/internal/runtime/egress.go:76-77). getent is its own nixpkgs package,
#     NOT part of `glibc`/`glibc.bin` — neither of those ships
#     a `bin/getent`, so listing glibc here yields an image where the egress arm
#     step fails at provision with a missing binary.
#   * coreutils/bash/cacert — a usable shell environment, and the CA bundle the
#     substituter + every HTTPS clone needs. A container with no CA bundle fails
#     at the first `nix` substitution with an opaque TLS error.
{
  pkgs,
  lib,
  compassAgent,
}:
let
  # The repo's pinned bun version. This image takes bun from nixpkgs rather than
  # vendoring the upstream release tarball, which would mean carrying per-arch
  # hashes for an image whose only bun caller is the `compass-agent` entrypoint,
  # ordinary TypeScript with no version floor. But "close enough" is not a pin:
  # reading the pin and asserting on it turns a silent drift into a build failure
  # naming both versions, so a nixpkgs roll that moves bun off the repo's version
  # cannot ship unnoticed.
  protoTools = builtins.fromTOML (builtins.readFile ../.prototools);

  bun =
    assert lib.assertMsg (pkgs.bun.version == protoTools.bun) ''
      agent image bun (nixpkgs ${pkgs.bun.version}) has drifted from the
      .prototools pin (${protoTools.bun}). Either pin nixpkgs' bun to the
      .prototools version, or vendor that release directly.
    '';
    pkgs.bun;

  # Single-user Nix: in-container Nix is set up for the agent uid, with `/nix`
  # owned by the agent user. Each setting below is load-bearing:
  #
  #   build-users-group = (empty) — THE single-user switch. A non-empty group with
  #     no nix-daemon running aborts every build. There is no daemon here by
  #     design — one uid does all Nix work, and the container runs the agent as
  #     PID 1 with nothing to supervise a daemon.
  #   sandbox = false — the build sandbox needs nested user namespaces, which are
  #     unreliable inside an already-rootless podman userns. The threat model
  #     treats the agent as trusted, so it gains nothing from sandboxing a build
  #     the agent could run unsandboxed a moment later by hand.
  #   experimental-features — devenv is flake-based; without this every devenv
  #     invocation fails.
  #   substituters — cold-realizing closures from the public cache is accepted
  #     cost, so the cache and its key must be configured or every activation
  #     builds from source.
  #   ssl-cert-file — pinned to the bundle's STORE path, not a /etc filename.
  #     `pkgs.cacert` installs `etc/ssl/certs/ca-bundle.crt`, but nix's compiled-in
  #     default is `/etc/ssl/certs/ca-certificates.crt` and openssl's OPENSSLDIR
  #     ships no bundle at all, so without this every substitution and flake fetch
  #     dies with an opaque "Problem with the SSL CA cert (77)" — the exact failure
  #     this file's header says the CA bundle prevents. A store path is immune to
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
  # `pkgs.cacert` ships only `ca-bundle.crt`, but openssl (hence curl, git, gh,
  # ssh) defaults to `/etc/ssl/certs/ca-certificates.crt`. `ssl-cert-file` above
  # covers nix alone, so without this the agent's own `git clone`/`gh` over HTTPS
  # still fail even once substitution works. Same store bundle, second name.
  caCertificates = pkgs.runCommand "ca-certificates" { } ''
    mkdir -p $out/etc/ssl/certs
    ln -s ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
          $out/etc/ssl/certs/ca-certificates.crt
  '';

  # The devenv stdlib for direnv, so `use devenv` resolves in-image.
  #
  # Shipping the `devenv` and `direnv` binaries is not enough to make the
  # activation path work. direnv's stdlib has no `use_devenv`; devenv supplies it
  # as a shell fragment printed by `devenv direnvrc`, which an `.envrc` is
  # expected to `eval` itself (devenv's own `devenv init` template does exactly
  # that — forks/devenv/devenv/init/envrc:3). An `.envrc` that is a bare
  # `use devenv` — the shape a repo ends up with once anyone trims the boilerplate
  # — fails with `use_devenv: command not found`, and direnv still runs the
  # command afterwards, so the agent silently degrades to this image's tools
  # instead of the repo's toolchain. Loading the fragment from direnv's own rc
  # makes every `.envrc` shape work without the agent editing repos it clones.
  #
  # It lands in /etc, not under $HOME, even though direnv looks for the rc at
  # `$DIRENV_CONFIG/direnvrc` (default `~/.config/direnv/direnvrc`) and has no
  # system-wide path. The container module stages a real `/home/agent` directory
  # of its own (containers.nix `mkEtc`), which wins over anything this buildEnv
  # symlinks there — a `home/agent/...` entry here simply vanishes from the image.
  # `DIRENV_CONFIG` in the image env points direnv at this store-backed directory
  # instead, which also leaves the agent's writable `~/.config` untouched.
  #
  # Activation through this rc prints a short run of
  # `/etc/direnv/direnvrc:<n>: <name>: command not found` lines, on a COLD load
  # only — a warm re-entry into an already-built environment prints none
  # (measured 2, 0, 0 across three consecutive `direnv exec` runs in the built
  # image). The exact count and line numbers are deliberately not quoted here:
  # they track the rc devenv emits and shift with the pin.
  #
  # They are expected upstream noise, not a failed activation: `devenv direnvrc`'s
  # own `_nix_import_env` (fork `devenv/direnvrc`, the `eval "$env"` it performs)
  # evals the printed environment, whose non-assignment progress lines bash then
  # tries to run as commands. The environment is fully activated regardless — an
  # agent reading its own tool output should not read these as a broken toolchain.
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
