{
  # Compass distribution flake. Packages the four backend binaries + the native
  # gtk4 app + the microVM stack-env from a bare checkout, so
  # `nix profile install github:RigelBuild/compass#<pkg>` works with nothing but
  # nix on PATH. Pin discipline: nixpkgs is pinned to the SAME rev devenv.lock
  # resolves, but a flake carries its OWN flake.lock, so this is a SECOND
  # independent nixpkgs lock; tools/toolchain/flake-parity.ts is the gate that
  # fails CI on skew.
  description = "Compass — binaries, native app, and microVM stack-env";

  # Pinned to the exact rev devenv.lock's nixpkgs node records (the URL is the
  # single source of the concrete rev). flake.lock records the same rev; the
  # parity gate asserts they match, and refresh-devenv-nixpkgs.ts keeps this URL
  # + flake.lock in lockstep on a channel bump.
  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/c946ff36bf193309589932c371bd5ae6653c912e";

  outputs =
    { self, nixpkgs }:
    let
      # A manual forAllSystems (no flake-utils). x86_64-linux is load-bearing: it
      # builds every package including the gtk4 cgo app. aarch64-darwin is a
      # follow-up, not blocked on here.
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));

      # ONE version string stamped into all four backend binaries + the app. The
      # semver base comes from version.txt (the same source release.yml/ci.yml
      # read), trimmed because a trailing newline would land in the ldflag. A
      # `+g<shortRev>` suffix keeps a non-release artifact identifiable;
      # dirtyShortRev carries its own `-dirty`, and a bare VCS-less tree stamps the
      # plain base. Empty content throws rather than stamping a coreless `+g<rev>`.
      # The character class is applied to the trimmed value, matching the devenv
      # lane exactly so both accept the same file; the flake-gate:version-guard
      # parity gate holds the two in agreement.
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

      # The backend module rooted at go/. Renamed off `go` because buildGoModule
      # unpacks src into $GOPATH=/build/go and a root named `go` collides.
      goSrc = builtins.path {
        path = ./go;
        name = "compass-go-src";
      };

      # proxyVendor: the backend pulls wails/secretspec, whose //go:embed patterns
      # reference darwin/windows-only files a vendor-tree build fails on;
      # proxyVendor touches only compiled packages. vendorHash pins the whole
      # module graph (matches guestd's); recompute with lib.fakeHash on a go.sum move.
      vendorHash = "sha256-hxjuJ8jRbNNnk4ZhXDIaFpSwno1hb6P7aeH0G9OWd8o=";
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
              ldflags = [ "-X main.version=${version}" ];
              # Package-level logic is gated under compass-go:ci; re-running the
              # suite in the nix build would only re-pay it.
              doCheck = false;
            };

          # The web UI built to a static dist (apps/ui/dist.nix). Bound in the
          # `let` because it has two consumers: its own package output and
          # compass-app's bin/dist staging.
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

          # Exposed on its own: the gate-able unit for the bun-workspace FOD pin,
          # and app-bundle can stage this store path instead of a working-tree dist.
          inherit compass-ui;

          # The Linux gtk4 cgo native shell (Wails v3). Links the WebKitGTK
          # closure through cgo — the same gtk-closure.nix the dev shell and e2e
          # helper use, applied against this flake's pinned pkgs so the three
          # cannot drift. tags=[gtk4] selects the gtk4 build.
          # TODO(aarch64-darwin follow-up): the darwin app links system WebKit via
          # frameworks, not this closure — out of scope while systems is x86_64-only.
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
            # distDirForExecutable resolves `dist` BESIDE the executable, so a bare
            # buildGoModule emitting only bin/compass-app installs a shell with no
            # UI. Staging bin/dist is the SAME layout the release tarball builds,
            # so both distribution channels resolve the UI identically. A real copy,
            # not a symlink: a `nix profile` install materializes bin/ as symlinks
            # into this store path, so the directory must BE there. `compass-ui`'s
            # output IS the dist contents. `chmod -R u+w` because store sources are
            # read-only.
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

      # `nix flake check` builds only `checks.*` — it merely EVALUATES `packages.*`
      # to a .drv, so a build-time break (go compile error, vendorHash drift) would
      # pass green. Aliasing every package as a check forces each to be realized,
      # which makes the §T6 promise — "every package BUILDS from a bare checkout" — true.
      checks = self.packages;
    };
}
