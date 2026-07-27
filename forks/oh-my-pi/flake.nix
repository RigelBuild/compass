{
  # Sealed nix build for the vendored oh-my-pi fork (SEA-1242 T1b, Option C —
  # Matt-ruled 2026-07-18). Builds omp from THIS subtree (`src = self`), so the
  # fork's sealed patches — the auth-broker `/metrics` exposition the
  # `llm_usage_*` dashboard scrapes (SEA-1242), and the composite tool-call-id
  # pairing fix (SEA-1160) — ship on Matt's schedule, decoupled from the upstream
  # release chain `numtide/llm-agents.nix` fetches.
  #
  # The recipe under `nix/` is numtide's omp `package.nix` re-homed here, with
  # `src` bound to the fork tree and a fork-specific `bun.nix`/`cargoHash`. This
  # is the nix-source fork pattern (devenv / nix2container / hermes-agent): a
  # sealed-added, export-excluded build wrapper inside an otherwise byte-faithful
  # vendored subtree. Regenerate the root `bun.nix` (bun2nix) and refresh
  # `nix/hashes.json` `cargoHash` on a subtree bump.
  #
  # mattfw consumes this via the sealed flake's `oh-my-pi` input:
  # `omp = inputs.oh-my-pi.packages.x86_64-linux.omp`.
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    bun2nix = {
      url = "github:nix-community/bun2nix/5a39d717029e94163ac223aee8d5c9946cafed1c";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      bun2nix,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        # bun's standard x64 build requires AVX2. This nix build runs on the
        # sealed CI static host fleet (the `nix` project pins to type=linux,
        # size=large), which includes mattlinuxpro (2013 Mac Pro Xeon,
        # pre-Haswell, no AVX2) where the standard build SIGILLs (exit 132) on
        # its first exec. nixpkgs no longer packages a baseline variant, so
        # fetch the upstream `-baseline` build directly, exactly as the CI image
        # toolchain does (ci/toolchain/ci-toolchain.nix). x86_64-linux only: ARM
        # has no AVX2, and only packages.x86_64-linux.omp is built in CI /
        # consumed (mattfw).
        #
        # Applied as an overlay UNDERNEATH bun2nix, not merely as the omp
        # `callPackage` bun arg: `bun2nix.hook` bakes `pkgs.bun` into its own
        # propagatedBuildInputs when the overlay builds the hook, and its
        # setup-hook runs a bare `bun resolve-catalog.ts` from PATH during
        # bunNodeModulesInstallPhase — the first bun exec of the build.
        # Overriding bun only at callPackage leaves that hook bun standard
        # (AVX2), so the catalog-resolve exec SIGILLs before the compile is even
        # reached. Layering the override below bun2nix rebuilds the hook against
        # baseline, leaving exactly one bun (baseline) in the whole build closure.
        bunBaselineOverlay =
          _: prev:
          prev.lib.optionalAttrs (prev.stdenv.hostPlatform.system == "x86_64-linux") {
            bun = prev.stdenv.mkDerivation {
              pname = "bun-baseline";
              inherit (prev.bun) version;
              src = prev.fetchurl {
                url = "https://github.com/oven-sh/bun/releases/download/bun-v${prev.bun.version}/bun-linux-x64-baseline.zip";
                hash = "sha256-nYokKSpwaAkCBdqsCloiP19pc29Sh+N7+I07QDHtx1A=";
              };
              nativeBuildInputs = [
                prev.unzip
                prev.autoPatchelfHook
              ];
              buildInputs = [ prev.stdenv.cc.cc.lib ];
              dontConfigure = true;
              dontBuild = true;
              installPhase = ''
                runHook preInstall
                install -Dm755 "$(find . -name bun -type f | head -1)" "$out/bin/bun"
                ln -s bun "$out/bin/bunx"
                runHook postInstall
              '';
            };
          };
        # bun override first, bun2nix overlay on top, so bun2nix.hook captures
        # the baseline bun (see comment above).
        pkgs = (nixpkgs.legacyPackages.${system}.extend bunBaselineOverlay).extend bun2nix.overlays.default;
        omp = pkgs.callPackage ./nix/package.nix {
          # bun2nix, bun (baseline via the overlay), and the rust/native
          # toolchain are auto-injected from pkgs; only the fork subtree src
          # is explicit.
          src = self;
        };
      in
      {
        packages = {
          inherit omp;
          default = omp;
        };
      }
    );
}
