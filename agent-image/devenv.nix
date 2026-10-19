{ pkgs, lib, config, inputs, ... }:
# Base OCI image for agents; its own devenv keeps dev-shell variables and
# unavailable store paths out of the runtime image.
# devenv provides the writable single-user Nix database and ownership setup.
# The agent owns `/nix` to rebuild in-container, without a host Nix mount.
let
  # `compass-agent` as a real command on PATH — a bundled single-file entrypoint.
  # The Runner execs a bare `compass-agent` argv and cli.ts has a real import
  # graph, so it is bundled rather than store-copied; entrypoint.nix carries why.
  compassAgent = import ./entrypoint.nix { inherit pkgs lib; };
  compassForgeCredential = import ../tools/toolchain/compass-forge-credential.nix {
    inherit pkgs;
    src = builtins.path { path = ../go; name = "compass-go-src"; };
  };

  toolchain = import ./toolchain.nix {
    inherit pkgs lib compassAgent compassForgeCredential;
    jjVineSrc = inputs.jj-vine-src;
    sourceFingerprint = import ./source-fingerprint.nix { inherit pkgs lib; };
  };

in
{
  # No dev-shell packages: nothing enters a shell here. Listing the toolchain
  # would build the same closure for a shell no one opens. (skopeo lives in the
  # ROOT dev shell the publish job enters — a package here would bake into the
  # image via the container entrypoint's `source ${shell.envScript}`.)
  packages = [ ];

  # direnv needs DIRENV_CONFIG to find its baked stdlib; Runner supplies
  # session-specific variables at exec time.
  # config.env is serialized into the image, so replace DEVENV_* store paths
  # during builds to avoid pulling their closures into the image.
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

      # Match the Go runtime's agent uid and home. The userns remap lets an arbitrary
      # host uid own `/nix`, so passwd and HOME must match or nix/direnv reject it.
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
