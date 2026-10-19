# Runtime toolchain (not project builds): Nix/devenv/direnv and pinned Bun
# support activation and compass-agent; Git/gh/jj/jj-vine support repo work.
# nftables/getent/gawk support egress; getent is its own package, not in glibc.
# coreutils/bash/cacert provide a usable shell and HTTPS trust.
{
  pkgs,
  lib,
  compassAgent,
  compassForgeCredential,
  jjVineSrc,
  sourceFingerprint,
}:
let
  # The repo's pinned bun — the exact vendored derivation the dev shell and CI
  # gate import, so the image IS the pin byte for byte, not a nixpkgs bun that
  # merely matches.
  bun = (import ../tools/toolchain/toolchain-tools.nix { inherit pkgs; }).bun;

  # The fork ships no flake, so build it here. The version comes from the fork's
  # own Cargo.toml, so a lock bump can never mislabel the binary.
  jjVine = pkgs.rustPlatform.buildRustPackage {
    pname = "jj-vine";
    version = (lib.importTOML (jjVineSrc + "/Cargo.toml")).package.version;
    src = jjVineSrc;
    cargoLock.lockFile = jjVineSrc + "/Cargo.lock";
    # Upstream tests need a live forge (forgejo compose); the image check runs the binary.
    doCheck = false;
  };

  # Single-user Nix: no daemon, so build-users-group is empty; no sandbox, since
  # nested userns is unreliable under rootless podman (agent is trusted). devenv
  # needs flakes. ssl-cert-file is a store path: nix's compiled-in default names
  # a file `pkgs.cacert` does not ship, so substitution would fail TLS.
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

  # Load devenv's `use devenv` fragment globally; direnv's stdlib lacks it, so
  # cloned repos need no edits. Store the rc outside `/home/agent`, which the
  # container stages over; DIRENV_CONFIG points to it.
  # Cold activation may print `command not found`; devenv still activates fully.
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
    compassForgeCredential

    # The agent clones and drives its own repos.
    pkgs.git
    pkgs.gh
    pkgs.openssh
    pkgs.jujutsu
    jjVine

    # Egress arm step's in-image requirements (egress.go:76-77).
    pkgs.nftables
    pkgs.getent
    pkgs.gawk

    # The resume path expands a streamed archive into the private blob directory.
    pkgs.gnutar
    # A usable base environment.
    pkgs.bashInteractive
    pkgs.coreutils-full
    pkgs.cacert

    nixConf
    # jj-vine asks gh for the resolved host's token; a host gh lacks fails closed.
    (pkgs.writeTextDir "etc/jj/conf.d/compass-agent.toml" ''
      [jj-vine]
      forge = "github"
      openAsDraft = true
      [jj-vine.github]
      host = "https://api.github.com"
      tokenCommand = ["${pkgs.gh}/bin/gh", "auth", "token", "--hostname", "{host}"]
    '')
    # Owner-aware credential lookup runs before gh, the default-installation fallback.
    (pkgs.writeTextDir "etc/gitconfig" ''
      [credential]
          useHttpPath = true
          helper =
          helper = !${compassForgeCredential}/bin/compass-forge-credential git-credential
          helper = !${pkgs.gh}/bin/gh auth git-credential
    '')
    caCertificates
    direnvConfig
    # /etc/compass-agent/source-fingerprint: the e2e fixture's stale-image check.
    sourceFingerprint
  ];
  pathsToLink = [
    "/bin"
    "/etc"
  ];
}
