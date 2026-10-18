# Resolve the locked nixpkgs and Meissa tools used by the dev shell.
# Parsed devenv package attrs extend CI; identity reports each tool's version,
# store path, and binaries. Language and Go analysis tools use separate sets.
{ attrs ? [ ] }:
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };
  meissaInput = lock.nodes.root.inputs.meissa;
  meissaNode = lock.nodes.${meissaInput}.locked;
  meissaFlake = builtins.getFlake "github:${meissaNode.owner}/${meissaNode.repo}/${meissaNode.rev}?narHash=${meissaNode.narHash}";
  meissaPkgs = meissaFlake.packages.${builtins.currentSystem};
  meissaTools = { inherit (meissaPkgs) rumdl biome; };
  meissaLib = meissaFlake.lib;
  # Select go-overlay's raw version key; the dev shell's dotted key is not valid.
  goOverlayNode = lock.nodes.go-overlay.locked;
  goOverlaySrc = builtins.fetchTarball {
    url = "https://github.com/${goOverlayNode.owner}/${goOverlayNode.repo}/archive/${goOverlayNode.rev}.tar.gz";
    sha256 = goOverlayNode.narHash;
  };
  goPin = import ./versions/go.nix;
  pkgsWithGo = import nixpkgsSrc { overlays = [ (import goOverlaySrc) ]; };
  goToolchain = pkgsWithGo.go-bin.versions.${goPin.version};

  toolchainTools = import ./toolchain-tools.nix { inherit pkgs; };

  # The Go analysis battery comes from the locked Meissa input, using the same
  # nixpkgs and Go toolchain as the dev shell.
  goAnalysisTools = meissaLib.goAnalysis { inherit pkgs goToolchain; };
  # Built from Compass's Go module graph and included in the CI tool environment.
  protocGenConnectGo = import ./protoc-gen-connect-go.nix { inherit pkgs goToolchain; };

  # Exclude dot-prefixed Nix wrapper internals; they are not PATH commands.
  binsOf = drv:
    let dir = "${drv}/bin";
    in
    if builtins.pathExists dir
    then builtins.filter (n: !(pkgs.lib.hasPrefix "." n)) (builtins.attrNames (builtins.readDir dir))
    else [ ];

  identityOf = drv: {
    version = drv.version or "";
    store = drv.outPath;
    bins = binsOf drv;
  };

in
{
  env = pkgs.buildEnv {
    name = "compass-gate-tools";
    paths =
      map (a: pkgs.${a}) attrs
      ++ builtins.attrValues meissaTools
      ++ [ protocGenConnectGo ];
  };

  identity = builtins.listToAttrs (map
    (a: {
      name = a;
      value = identityOf pkgs.${a};
    })
    attrs);

  langs = {
    bun = identityOf toolchainTools.bun;
    node = identityOf toolchainTools.node;
    moon = identityOf toolchainTools.moon;
    go = identityOf goToolchain;
  };
  goAnalysis = builtins.mapAttrs (_: identityOf) goAnalysisTools;
  meissa = builtins.mapAttrs (_: identityOf) meissaTools;
  # The base policy .rumdl.toml extends; CI exports it as RUMDL_BASE_CONFIG.
  rumdlBaseConfig = meissaPkgs."rumdl-base-config";
  inherit (toolchainTools) bun;
}
