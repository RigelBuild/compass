{ pkgs, lib, config, ... }:
# The Compass agent base image — the single self-contained OCI artifact every
# per-agent container starts from.
#
# WHY THIS IS ITS OWN devenv, and not a `containers.agent` attr on a project dev
# shell: devenv's container module serializes the ENCLOSING devenv's merged
# `config.env` into the image config (the RigelBuild/devenv fork's src/modules/containers.nix,
# the `config.Env` binding read by `mkDerivation`). Built from a dev
# shell, this image would therefore ship that shell's build environment — its
# compiler wrappers, its browser and library paths, its whole pkg-config dev
# closure. None of that belongs in a general-purpose agent runtime, and an env
# var pointing at a store path the image does not carry is worse than absent: it
# fails at use, not at build. A separate devenv means those project vars are
# never in this image's env in the first place.
#
# It is still a devenv container build, deliberately: devenv's module wires the
# one primitive that makes the writable-store model work —
# `initializeNixDatabase = true; nixUid = nixGid = 1000` (containers.nix, the
# `mkDerivation` binding), which ships a valid /nix/var/nix/db registering every
# baked path and chowns the nix state trees to the agent uid. Hand-rolling
# nix2container would mean re-implementing that, plus the passwd/group/shadow
# scaffold and /tmp perms, for no gain.
#
# WHAT THE AGENT DOES WITH IT: the agent owns /nix as itself (single-user Nix, no
# daemon), so it clones its own repos, edits devenv.nix, `direnv allow`s, and
# rebuilds mid-session exactly as a developer does. There is
# no host /nix/store mount and no assumption the host has Nix at all; Compass must
# run on hosts without it.
let
  # `compass-agent` as a real command on PATH — a bundled single-file entrypoint.
  # The Runner execs a bare `compass-agent` argv (relay.go `agentCommand`), and cli.ts has a
  # real import graph, so it is bundled rather than store-copied as a lone file;
  # entrypoint.nix carries the full rationale.
  compassAgent = import ./entrypoint.nix { inherit pkgs lib; };

  toolchain = import ./toolchain.nix { inherit pkgs compassAgent; };

in
{
  # No dev-shell packages: nothing enters a shell here. This devenv exists only to
  # express the container, so the toolchain lives in the image layer, not in
  # `packages` — listing it twice would build the same closure for a shell no one
  # opens. (skopeo, which the publish lane needs, lives in the ROOT compass dev
  # shell the publish job enters — NOT here: a package here would bake into the
  # image via the container entrypoint's `source ${shell.envScript}`.)
  packages = [ ];

  # One entry, and only because it has to be here: direnv resolves its rc as
  # `$DIRENV_CONFIG/direnvrc` (default `~/.config/direnv/direnvrc`) and has no
  # system-wide path, so the baked devenv stdlib in the toolchain's
  # `/etc/direnv/direnvrc` is only reachable if the image env points at it.
  # toolchain.nix carries why the rc exists and why it is in /etc. Everything
  # session-shaped (model, credentials) stays out — the Runner supplies it
  # per-exec, as the only component that knows the session; HOME and USER are
  # appended by the container module from the identity options below.
  #
  # devenv merges its own `DEVENV_*` vars into `config.env`, which the container
  # module serializes verbatim into the image `config.Env` (the RigelBuild fork
  # keeps no blanket `DEVENV_`-prefix strip — a reusable module applies the
  # minimal fix, not a namespace wipe, RIG-2404). Most are harmless: the fork
  # forces DEVENV_ROOT/STATE/RUNTIME/DOTFILE to the container's own home and
  # /tmp during a build (containers.nix, gated on isBuilding), so they name no
  # store path. The two that DO name an absolute /nix/store path are closure
  # roots — nix2container makes config.json a closure root (`deps=[configFile]`),
  # so a store path named in the env drags its whole closure into the image's
  # content layers and the initialized nix DB. `DEVENV_PROFILE` is the 266-path
  # dev profile; `DEVENV_TASK_FILE` is the generated tasks.json. Force both to a
  # non-store placeholder while a container is being built (isBuilding, inert in
  # a normal dev shell), exactly as the internal monorepo does. The
  # agent-image-env-gate is the regression backstop for this.
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
      # runtime runs the agent as uid 1000 with $HOME=/home/agent
      # (cmd/compass-runner/main.go `-home-dir`, `UID: defaultAgentUID`;
      # internal/runner/spec.go `SpecDefaults.CheckoutDir/HomeDir/UID`), and
      # launches containers with
      # `--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>`
      # (internal/runtime/podman.go createArgs), which remaps the invoking host
      # uid to the baked agent uid rather than passing it through — so an
      # arbitrary host uid still yields an agent that owns /nix. A startup
      # podman-version preflight (VerifyUsernsRemapSupport, ≥ 4.3) guards that
      # the engine supports the remap. devenv defaults to user `user` with
      # $HOME=/env; the uid agrees either way — that is what /nix ownership keys
      # on — but the passwd row and $HOME must match what the Runner execs with,
      # or nix/direnv/devenv hit "$HOME is not owned by you" and silently fall
      # back to the passwd home. The vendored fork exposes the identity
      # per-container, so other consumers keep their own defaults untouched.
      user = "agent";
      group = "agent";
      homeDir = "/home/agent";

      # The image carries no repo. devenv's default copyToRoot is the project
      # root, which here would bake this build directory into every agent
      # container for nothing — the agent clones the repos it works on itself
      # rather than being handed a checkout.
      copyToRoot = [ ];

      # Rootless podman reads from containers-storage directly, and the Runner
      # resolves its configured image ref (--image / $COMPASS_AGENT_IMAGE,
      # cmd/compass-runner/main.go:44-45) out of exactly that store when it
      # creates a container — there is no pull step. devenv's default registry is
      # `docker-daemon:`, which a rootless podman does not read.
      registry = "containers-storage:";

      # The agent's store is large and grows: nix, devenv's own closure, bun, the
      # git/gh/nftables set. One layer per default would collapse it into a single
      # enormous layer that re-transfers whole on any change; spreading it lets
      # podman share unchanged layers across agent containers on the same host.
      # A ceiling, not a count — the real layer count de-duplicates below it.
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
