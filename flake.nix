{
  # Compass distribution flake (docs/designs/platform/compass-distribution/design.md
  # §T6). Packages the four backend binaries + the native gtk3 app + the
  # microVM stack-env from a bare checkout, so
  # `nix profile install github:RigelBuild/compass#<pkg>` and
  # `nix run .#compass-stack -- status` work with nothing but nix on PATH.
  #
  # PIN DISCIPLINE (the gtk-e2e-env.nix:9-13 single-pin rule): nixpkgs is pinned
  # to the SAME revision devenv.lock resolves (cachix/devenv-nixpkgs, the rolling
  # devenv channel), so the flake-built binaries link byte-for-byte the libraries
  # a dev box and the app-bundle build do. A flake carries its OWN flake.lock, so
  # this is a SECOND independent nixpkgs lock — nothing enforces it stays equal to
  # devenv.lock by construction. tools/toolchain/flake-parity.ts is the named gate
  # that does, failing CI on skew (moon task flake-gate:flake-parity).
  description = "Compass — binaries, native app, and microVM stack-env";

  # Pinned to the exact rev devenv.lock's nixpkgs node records
  # (c946ff36bf193309589932c371bd5ae6653c912e). flake.lock will record this rev;
  # the parity gate asserts flake.lock's rev == devenv.lock's rev.
  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/c946ff36bf193309589932c371bd5ae6653c912e";

  outputs =
    { self, nixpkgs }:
    let
      # A manual forAllSystems (no flake-utils dependency — the record's preferred
      # simplest shape). x86_64-linux is the load-bearing system: it builds every
      # package including the gtk3 cgo app. aarch64-darwin is a follow-up (see the
      # TODO in the per-system set below) — not blocked on here.
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));

      # ONE version string stamped into all four backend binaries + the app
      # (Global Constraint 4: the stack binaries carry ONE stamp). Short form of
      # the flake rev; dirtyShortRev on an uncommitted working copy; "dev" when
      # neither is available (a bare tree with no VCS metadata).
      version = self.shortRev or self.dirtyShortRev or "dev";

      # The backend module rooted at go/ (github.com/RigelBuild/compass/go).
      # Renamed off `go` (buildGoModule unpacks src into $GOPATH=/build/go, and a
      # root literally named `go` collides — see guest-image/default.nix:78-81).
      goSrc = builtins.path {
        path = ./go;
        name = "compass-go-src";
      };

      # proxyVendor: the backend pulls wails/secretspec, whose //go:embed patterns
      # reference darwin/windows-only asset files a vendor-tree build fails on;
      # proxyVendor populates the module cache so only compiled packages are
      # touched (guest-image/default.nix:82-87). vendorHash pins the fetched set —
      # the whole module graph, so it matches guestd's proxyVendor hash. Recompute
      # with lib.fakeHash on a go.mod/go.sum move.
      vendorHash = "sha256-LmCUIADU45KTiLTuAvq57ZdFzdUqrcGctopPtT/u/yk=";
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          # One CGO_ENABLED=0 backend binary, version-stamped. Each of the four
          # shares this builder so they carry the identical stamp.
          goBin =
            name:
            pkgs.buildGoModule {
              pname = name;
              inherit version;
              src = goSrc;
              subPackages = [ "cmd/${name}" ];
              proxyVendor = true;
              inherit vendorHash;
              env.CGO_ENABLED = 0;
              ldflags = [ "-X main.version=${version}" ];
              # Package-level logic is gated under compass-go:ci; re-running the
              # suite in the nix build would only re-pay it.
              doCheck = false;
            };
        in
        {
          compass = goBin "compass";
          compass-server = goBin "compass-server";
          compass-runner = goBin "compass-runner";
          compass-stack = goBin "compass-stack";

          # The Linux gtk3 cgo native shell (Wails v3). Links the
          # WebKitGTK closure through cgo — the same gtk-closure.nix the dev shell
          # and the e2e helper realize, applied against this flake's pinned pkgs so
          # the three cannot drift (gtk-e2e-env.nix:38). tags=[gtk3] selects the
          # gtk3 build (main.go's //go:build unix && gtk3).
          #
          # TODO(aarch64-darwin follow-up): the darwin app links system WebKit via
          # frameworks, NOT this gtk closure — no pkg-config/gtk buildInputs, a
          # different tag set. Out of scope for this slice (systems is x86_64-linux
          # only); add a darwin branch when the systems list grows.
          compass-app = pkgs.buildGoModule {
            pname = "compass-app";
            inherit version;
            src = goSrc;
            subPackages = [ "cmd/compass-app" ];
            proxyVendor = true;
            inherit vendorHash;
            env.CGO_ENABLED = 1;
            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = pkgs.lib.closePropagation (import ./tools/toolchain/gtk-closure.nix pkgs);
            tags = [ "gtk3" ];
            ldflags = [ "-X main.version=${version}" ];
            doCheck = false;
          };

          # The microVM stack runtime trio (cloud-hypervisor + virtiofsd + passt)
          # at the pinned rev, joined so `nix profile install .#compass-stack-env`
          # puts all three on PATH for the stack's LookPath spawns.
          compass-stack-env = pkgs.symlinkJoin {
            name = "compass-stack-env-${version}";
            paths = [
              pkgs.cloud-hypervisor
              pkgs.virtiofsd
              pkgs.passt
            ];
          };
        }
      );

      # `nix flake check` builds only the flake's `checks.*` outputs — it merely
      # EVALUATES `packages.*` to a .drv without realizing them, so a build-time
      # break (a go compile error, a vendorHash drift) would pass flake-check
      # green. Aliasing every package as a check forces `nix flake check` to
      # realize each one: each leaf is a derivation, which is exactly what a
      # check must be. This is what makes the §T6 promise — "every package
      # BUILDS from a bare checkout" — true.
      checks = self.packages;
    };
}
