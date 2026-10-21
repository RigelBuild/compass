{
  # A bare checkout can install the packages with only nix on PATH.
  # `flake.lock` is independent of `devenv.lock`, so the parity gate keeps their
  # nixpkgs revisions aligned.
  description = "Compass — binaries, native app, and microVM stack-env";

  # Pinned to the exact rev devenv.lock's nixpkgs node records (the URL is the
  # single source of the concrete rev). flake.lock records the same rev; the
  # parity gate asserts they match, and refresh-devenv-nixpkgs.ts keeps this URL
  # + flake.lock in lockstep on a channel bump.
  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/760b85220489f69855e6df78a6d1dad5873f8bf6";

  outputs =
    { self, nixpkgs }:
    let
      # A manual forAllSystems (no flake-utils). x86_64-linux is load-bearing: it
      # builds every package including the gtk4 cgo app. aarch64-darwin is a
      # follow-up, not blocked on here.
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));

      # All binaries share one version stamp; trim `version.txt` so its newline does
      # not enter ldflags. Append the revision when the flake has one; else stamp the plain base.
      # Match devenv's accepted characters so the version guard keeps both paths aligned.
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
      # The full commit GetServerInfo reports as `rev`; dirtyRev is "<sha>-dirty".
      rev = self.rev or self.dirtyRev or "";

      # The backend module rooted at go/. Renamed off `go` because buildGoModule
      # unpacks src into $GOPATH=/build/go and a root named `go` collides.
      goSrc = builtins.path {
        path = ./go;
        name = "compass-go-src";
      };

      # proxyVendor: the backend pulls wails, whose //go:embed patterns
      # reference darwin/windows-only files a vendor-tree build fails on;
      # proxyVendor touches only compiled packages. vendorHash pins the whole
      # module graph (matches guestd's and the plugin's); recompute with lib.fakeHash on a go.sum move.
      vendorHash = "sha256-hTx1g3emTlQrjxI6WCb0SpeQUXg2HHRzA28vRc1KmGQ=";
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          # One CGO_ENABLED=0 backend binary, version-stamped. All four share this
          # builder so they carry the identical stamp.
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
              ldflags = [
                "-X main.version=${version}"
                "-X main.rev=${rev}"
              ];
              # Package-level logic is gated under compass-go:ci; re-running the
              # suite in the nix build would only re-pay it.
              doCheck = false;
            };

          # The dedicated static helper shares the same module source and vendor closure.
          compassForgeCredential = import ./tools/toolchain/compass-forge-credential.nix {
            inherit pkgs;
            src = goSrc;
          };

          # The web UI built to a static dist (apps/ui/dist.nix). Bound in the
          # `let` because it has two consumers: its own package output and
          # compass-app's bin/dist staging.
          compass-ui = pkgs.lib.makeOverridable (import ./apps/ui/dist.nix) {
            inherit pkgs version;
            inherit (pkgs) lib;
          };

          inherit (import ./tools/toolchain/secretspec-env.nix { inherit (pkgs.stdenv.hostPlatform) system; })
            secretspec
            ;
        in
        {
          compass = goBin "compass";
          compass-forge-credential = compassForgeCredential;
          # Boot spawns secretspec by name for the master key. The pin comes from
          # devenv.lock (this nixpkgs has 0.14.0), and it wins over a host copy.
          compass-server = (goBin "compass-server").overrideAttrs (old: {
            nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.makeWrapper ];
            buildInputs = (old.buildInputs or [ ]) ++ [ pkgs.git ];
            postFixup = ''
              wrapProgram $out/bin/compass-server --prefix PATH : ${secretspec}/bin --prefix PATH : ${pkgs.git}/bin
            '';
          });
          compass-runner = goBin "compass-runner";
          compass-stack = goBin "compass-stack";

          # Exposed on its own: the gate-able unit for the bun-workspace FOD pin,
          # and app-bundle can stage this store path instead of a working-tree dist.
          inherit compass-ui;

          # The Linux gtk4 app links the shared GTK closure through cgo, keeping the dev
          # shell, e2e helper, and flake package aligned. `gtk4` selects the build variant.
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

            # The shell loads UI from `dist` beside the executable, so stage it at `bin/dist`
            # in the same layout as the release tarball.
            # Copy instead of symlink: profile installs symlink `bin/` into the store, so the
            # directory must exist in the package output. Make it writable for `chmod`.
            postInstall = ''
              cp -R ${compass-ui} $out/bin/dist
              chmod -R u+w $out/bin/dist
            '';
          };

          # The microVM stack runtime trio (cloud-hypervisor + virtiofsd + passt)
          # at the pinned rev, joined so `nix profile install .#compass-stack-env`
          # puts all three on PATH for the stack's LookPath spawns. secretspec
          # rides along so `compass-stack preflight` finds the server's pin.
          compass-stack-env = pkgs.symlinkJoin {
            name = "compass-stack-env-${version}";
            paths = [
              pkgs.cloud-hypervisor
              pkgs.virtiofsd
              pkgs.passt
              secretspec
            ];
          };
        }
      );

      # `nix flake check` builds only `checks.*` — it merely EVALUATES `packages.*`
      # to a .drv, so a build-time break (go compile error, vendorHash drift) would
      # pass green. Aliasing every package as a check forces each to be realized,
      # which makes the §T6 promise — "every package BUILDS from a bare checkout" — true.
      checks = forAllSystems (
        pkgs:
        let
          packages = self.packages.${pkgs.stdenv.hostPlatform.system};
        in
        packages
        // {
          compass-server-secretspec = import ./tools/flake-gate/compass-server-secretspec.nix {
            inherit pkgs;
            inherit (packages) compass-server;
          };
        }
      );
    };
}
