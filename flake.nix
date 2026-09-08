{
  # Compass distribution flake (docs/designs/infra/release/compass-distribution/design.md
  # §T6). Packages the four backend binaries + the native gtk4 app + the
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

  # Pinned to the exact rev devenv.lock's nixpkgs node records (the URL below is
  # the single source of the concrete rev — this comment names no literal, so an
  # automated devenv-nixpkgs bump that rewrites the URL leaves nothing stale
  # here). flake.lock records the same rev; the parity gate
  # (tools/toolchain/flake-parity.ts) asserts flake.lock's rev == devenv.lock's.
  # The refresh-devenv-nixpkgs.ts postUpgradeTask keeps this URL + flake.lock in
  # lockstep on a channel bump.
  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/c946ff36bf193309589932c371bd5ae6653c912e";

  outputs =
    { self, nixpkgs }:
    let
      # A manual forAllSystems (no flake-utils dependency — the record's preferred
      # simplest shape). x86_64-linux is the load-bearing system: it builds every
      # package including the gtk4 cgo app. aarch64-darwin is a follow-up (see the
      # TODO in the per-system set below) — not blocked on here.
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));

      # ONE version string stamped into all four backend binaries + the app
      # (Global Constraint 4: the stack binaries carry ONE stamp). The semver
      # base comes from version.txt — the same single source release.yml and
      # ci.yml read — trimmed because the file ends in a newline that would
      # otherwise land in the ldflag (and leave a trailing dash in the store
      # path name). A `+g<shortRev>` build-metadata suffix keeps a non-release
      # artifact identifiable (ci.yml's dev-compile lane stamps this same
      # clean-tree shape); dirtyShortRev already carries its own `-dirty`
      # marker, and a bare tree with no VCS metadata stamps the plain base.
      # Empty content throws rather than stamping a coreless `+g<rev>`, which
      # is not valid semver but would otherwise build and ship silently — the
      # guard `ci.yml`'s dev-compile lane and `app-bundle/build.sh` already
      # carry (release.yml's two stamp steps do NOT; RIG-3428). The character
      # class is then applied to the trimmed value, which is exactly what the
      # devenv lane does after trimming the same four whitespace bytes, so the
      # two lanes accept the same file: without the class an inner space passes
      # here and lands raw in the ldflag (the store-path name silently
      # sanitizes it to a dash), which is the fail-quiet outcome the guard
      # exists to stop. The flake-gate:version-guard parity gate holds the two
      # lanes in agreement by running both guards over a shared candidate table.
      versionBase =
        let
          v = nixpkgs.lib.strings.trim (builtins.readFile ./version.txt);
        in
        if v == "" then
          throw "version.txt is missing or empty"
        else if builtins.match "[0-9A-Za-z.+-]+" v == null then
          throw "version.txt is not a version string: '${v}'"
        else
          v;
      version =
        if self ? shortRev then
          "${versionBase}+g${self.shortRev}"
        else if self ? dirtyShortRev then
          "${versionBase}+g${self.dirtyShortRev}"
        else
          versionBase;

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
      vendorHash = "sha256-hxjuJ8jRbNNnk4ZhXDIaFpSwno1hb6P7aeH0G9OWd8o=";
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

          # The web UI built to a static dist (apps/ui/dist.nix, which carries the
          # rationale). Bound in the `let` because it has TWO consumers below: its
          # own package output, and compass-app's bin/dist staging.
          compass-ui = import ./apps/ui/dist.nix {
            inherit pkgs version;
            inherit (pkgs) lib;
          };
        in
        {
          compass = goBin "compass";
          compass-server = goBin "compass-server";
          compass-runner = goBin "compass-runner";
          compass-stack = goBin "compass-stack";

          # Exposed on its own, not only as compass-app's input: it is the
          # gate-able unit for the bun-workspace FOD pin (a `checks` alias, so
          # `nix flake check` realizes it), and app-bundle can stage this store
          # path instead of requiring a working-tree `apps/ui/dist`.
          inherit compass-ui;

          # The Linux gtk4 cgo native shell (Wails v3). Links the
          # WebKitGTK closure through cgo — the same gtk-closure.nix the dev shell
          # and the e2e helper realize, applied against this flake's pinned pkgs so
          # the three cannot drift (gtk-e2e-env.nix:38). tags=[gtk4] selects the
          # gtk4 build (main.go's //go:build unix && gtk4).
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
            tags = [ "gtk4" ];
            ldflags = [ "-X main.version=${version}" ];
            doCheck = false;

            # THE UI (RIG-3474). The gtk4 shell loads its front-end off disk:
            # `distDirForExecutable` (go/cmd/compass-app/main.go:353-366) resolves
            # `dist` BESIDE the executable, so a bare buildGoModule emitting only
            # `bin/compass-app` installs a shell with no UI — which is what
            # `nix profile install github:RigelBuild/compass#compass-app` did.
            # Staging `bin/dist` next to `bin/compass-app` is the SAME layout the
            # release tarball builds (app-bundle/build.sh:97), so the two
            # distribution channels resolve the UI identically.
            #
            # A real copy, not a `ln -s`: the resolver joins `dist` onto the
            # binary's own directory and reads through it, and a `nix profile`
            # install materializes `$out/bin` as symlinks into this store path —
            # so the directory has to BE there, and a copy keeps the served tree
            # independent of how the profile is linked. `compass-ui`'s output IS
            # the dist contents (apps/ui/dist.nix's trailing `cp -R
            # apps/ui/dist/. $out/`), so the store path is staged as `dist`
            # itself. `chmod -R u+w` because store sources are read-only and
            # nothing downstream (fixup, strip) should trip on that.
            postInstall = ''
              cp -R ${compass-ui} $out/bin/dist
              chmod -R u+w $out/bin/dist
            '';
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
