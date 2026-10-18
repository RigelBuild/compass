# Provide Linux-only Chromium outside the cross-platform dev-shell package list.
# Pin nixpkgs to devenv.lock and expose Chromium plus the fontconfig used by
# screenshot lanes. Realize both outputs with nix build.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.${lock.nodes.root.inputs.nixpkgs}.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };

  # Only the two branded faces the design tokens name. The font-coverage gate
  # keeps literal UI source text, including the baseline fixtures, inside Space
  # Mono. Runtime text it cannot see renders as tofu under this config.
  fontDirs = [
    "${pkgs.google-fonts.override { fonts = [ "SpaceMono" ]; }}/share/fonts/truetype"
    "${pkgs.departure-mono}/share/fonts/otf"
  ];
in
{
  # Referenced by its own store path, never merged into a buildEnv: the
  # nix-wrapped chromium resolves its sandbox helper + browser through bin/
  # wrapper scripts a symlink-merge would break. ci.yml reads `bin/chromium`.
  chromium = pkgs.chromium;

  # Pin fonts and rasterization so screenshots do not depend on host defaults.
  fontconfig = pkgs.writeText "compass-visual-gate-fonts.conf" ''
    <?xml version="1.0"?>
    <!DOCTYPE fontconfig SYSTEM "fonts.dtd">
    <fontconfig>
    ${builtins.concatStringsSep "\n" (map (d: "  <dir>${d}</dir>") fontDirs)}
      <match target="font">
        <edit name="antialias" mode="assign"><bool>true</bool></edit>
        <edit name="hinting" mode="assign"><bool>true</bool></edit>
        <edit name="hintstyle" mode="assign"><const>hintslight</const></edit>
        <edit name="rgba" mode="assign"><const>none</const></edit>
        <edit name="lcdfilter" mode="assign"><const>lcddefault</const></edit>
      </match>
      <cachedir prefix="xdg">fontconfig</cachedir>
    </fontconfig>
  '';
}
