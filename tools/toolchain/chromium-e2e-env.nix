# The out-of-band Chromium for the dev-boot smoke gate (apps/ui:dev-smoke,
# design record compass-dev-boot-gate). The gate drives a headless Chromium
# through Playwright, pointed at it via launchOptions.executablePath resolved
# from PLAYWRIGHT_CHROMIUM_PATH (apps/ui/playwright.config.ts:31-33).
#
# Realized here rather than through the shared gate-tools.nix / toolchain-parity
# machinery for two reasons, mirroring how gtk-e2e-env.nix handles the gtk3 e2e
# browser stack:
#   - nixpkgs `chromium` is Linux-only (no darwin build), so it cannot live in
#     devenv.nix's parsed `packages` literal — the parity gate resolves every
#     bare attr in that literal on macOS too, where chromium does not exist, and
#     an unverifiable attr fails the gate. chromium is guarded Linux-only in the
#     dev shell (devenv.nix, alongside xvfb-run) and provisioned onto the CI
#     runner from here.
#   - chromium is a browser binary, not a dev-shell CLI whose ambient-vs-pinned
#     PATH drift the parity gate exists to catch, so keeping it out of the parsed
#     attrs is conceptually right — it is a gate input, not a toolchain.
#
# Pins nixpkgs to the SAME devenv.lock revision the dev shell and gate-tools.nix
# resolve, so CI drives byte-for-byte the Chromium a Linux dev box does.
#
# One output the ci.yml step consumes, realized with `nix build` (never
# `nix eval`, which strips the store context that would build the derivation):
#
#   chromium  the nix-wrapped Chromium derivation; the step reads its
#             bin/chromium out-path into PLAYWRIGHT_CHROMIUM_PATH.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
in
{
  # Referenced by its own store path, never merged into a buildEnv: the
  # nix-wrapped chromium resolves its sandbox helper + unwrapped browser through
  # its own bin/ wrapper scripts and store-relative references that a
  # symlink-merge would break. ci.yml reads `bin/chromium` off this out-path.
  chromium = pkgs.chromium;
}
