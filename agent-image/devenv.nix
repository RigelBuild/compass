{ pkgs, lib, config, ... }:
# The Compass agent base image — the single self-contained OCI artifact every
# per-agent container starts from.
#
# Its own devenv, not a `containers.agent` attr on a dev shell: devenv's container
# module serializes the ENCLOSING devenv's merged `config.env` into the image, so
# a dev-shell build would ship that shell's whole build environment — none of
# which belongs in an agent runtime, and an env var pointing at a store path the
# image lacks fails at use, not at build. A separate devenv keeps those vars out.
#
# Still a devenv container build, deliberately: the module wires the one primitive
# the writable-store model needs (`initializeNixDatabase`, nixUid=nixGid=1000),
# shipping a valid nix DB and chowning the nix state to the agent uid.
# Hand-rolling nix2container would re-implement that for no gain.
#
# The agent owns /nix as itself (single-user Nix), so it clones repos, edits
# devenv.nix, and rebuilds mid-session as a developer does. No host /nix mount and
# no assumption the host has Nix; Compass must run on hosts without it.
let
  # `compass-agent` as a real command on PATH — a bundled single-file entrypoint.
  # The Runner execs a bare `compass-agent` argv and cli.ts has a real import
  # graph, so it is bundled rather than store-copied; entrypoint.nix carries why.
  compassAgent = import ./entrypoint.nix { inherit pkgs lib; };

  toolchain = import ./toolchain.nix { inherit pkgs compassAgent; };

in
{
  # No dev-shell packages: nothing enters a shell here. Listing the toolchain
  # would build the same closure for a shell no one opens. (skopeo lives in the
  # ROOT dev shell the publish job enters — a package here would bake into the
  # image via the container entrypoint's `source ${shell.envScript}`.)
  packages = [ ];

  # One entry, and only because it must be here: direnv resolves its rc as
  # `$DIRENV_CONFIG/direnvrc` with no system-wide path, so the baked devenv stdlib
  # in `/etc/direnv/direnvrc` is only reachable if the image env points at it.
  # Everything session-shaped stays out — the Runner supplies it per-exec.
  #
  # devenv merges its own `DEVENV_*` vars into `config.env`, serialized verbatim
  # into the image. Most are harmless (the fork forces ROOT/STATE/RUNTIME to the
  # container home during a build), but DEVENV_PROFILE (the dev profile) and
  # DEVENV_TASK_FILE name absolute /nix/store paths, and nix2container makes
  # config.json a closure root — so a store path in the env drags its whole
  # closure into the image. Force both to a placeholder while building
  # (isBuilding). The agent-image-env-gate is the regression backstop.
  env = {
    DIRENV_CONFIG = "/etc/direnv";
  }
  // lib.optionalAttrs config.container.isBuilding {
    DEVENV_PROFILE = lib.mkForce "/home/agent/.devenv/profile-not-in-image";
    DEVENV_TASK_FILE = lib.mkForce "/home/agent/.devenv/tasks-not-in-image.json";
  };

  containers = lib.optionalAttrs pkgs.stdenv.isLinux {
    agent = {
      name = "compass-agent";

      # Identity, matched to the Go runtime rather than devenv's default. The
      # runtime runs the agent as uid 1000 with $HOME=/home/agent, and launches
      # containers with `--userns=keep-id:uid=<agent-uid>` so an arbitrary host uid
      # still yields an agent that owns /nix (a podman-version preflight guards the
      # remap). The uid is what /nix ownership keys on, but the passwd row and
      # $HOME must match what the Runner execs with, or nix/direnv hit "$HOME is
      # not owned by you". The vendored fork exposes the identity per-container.
      user = "agent";
      group = "agent";
      homeDir = "/home/agent";

      # The image carries no repo. devenv's default copyToRoot is the project root,
      # which would bake this build directory into every container for nothing —
      # the agent clones the repos it works on itself.
      copyToRoot = [ ];

      # Rootless podman reads from containers-storage directly, and the Runner
      # resolves its image ref out of exactly that store when it creates a
      # container — no pull step. devenv's default `docker-daemon:` a rootless
      # podman does not read.
      registry = "containers-storage:";

      # The agent's store is large and grows (nix, devenv's closure, bun, the
      # git/gh/nftables set). One layer would re-transfer whole on any change;
      # spreading it lets podman share unchanged layers across containers. A
      # ceiling, not a count — the real layer count de-duplicates below it.
      maxLayers = 60;

      layers = [
        {
          copyToRoot = [ toolchain ];
          maxLayers = 60;
        }
      ];
    };
  };
}
