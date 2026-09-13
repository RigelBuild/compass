# The out-of-band Chromium for the dev-boot smoke gate (apps/ui:dev-smoke). The
# gate drives a headless Chromium through Playwright via PLAYWRIGHT_CHROMIUM_PATH.
#
# Realized here rather than through gate-tools.nix (mirroring gtk-e2e-env):
# nixpkgs `chromium` is Linux-only, so it cannot live in devenv.nix's parsed
# `packages` literal (the parity gate resolves every bare attr on macOS too). It
# is Linux-guarded in the dev shell and provisioned onto the CI runner from here.
# Pins nixpkgs to the SAME devenv.lock rev the dev shell resolves.
#
# Two outputs, realized with `nix build` (never `nix eval`):
#   chromium    the nix-wrapped Chromium; the step reads bin/chromium into
#               PLAYWRIGHT_CHROMIUM_PATH.
#   fontconfig  a self-contained fontconfig read into FONTCONFIG_FILE by both
#               pixel-touching lanes and the Linux dev shell.
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };

  # The two branded faces the design tokens name, then a coverage fallback: the
  # branded pair leaves 21 of the UI's 49 non-ASCII glyphs uncovered, and an
  # uncovered glyph bakes tofu into a baseline. Unifont, not a stock face:
  # DejaVu pulled a proportional math face into the monospace grid; Unifont is
  # 1-bit 16x16 so it reads as pixel-grid. It is dual-width though, so some
  # fallback glyphs miss the cell — RIG-3603 retires those to dot-matrix SVG.
  fontDirs = [
    "${pkgs.google-fonts.override { fonts = [ "SpaceMono" ]; }}/share/fonts/truetype"
    "${pkgs.departure-mono}/share/fonts/otf"
    "${pkgs.unifont}/share/fonts/opentype"
    "${pkgs.unifont_upper}/share/fonts/opentype"
  ];
in
{
  # Referenced by its own store path, never merged into a buildEnv: the
  # nix-wrapped chromium resolves its sandbox helper + browser through bin/
  # wrapper scripts a symlink-merge would break. ci.yml reads `bin/chromium`.
  chromium = pkgs.chromium;

  # A self-contained fontconfig: the pinned faces + the rasterization parameters.
  # Both halves are load-bearing. The <dir> list makes the host's fonts invisible
  # so a box with 800+ families resolves the same faces as a bare runner — format
  # subdirs, not share/fonts, because bitmap strikes would outsort the scalable
  # face at some sizes. The <match> block pins hinting/AA, which the font set
  # alone does not: unset they come from fontconfig's compiled-in defaults, so a
  # devenv.lock bump moving fontconfig would shift every text baseline with no
  # declared cause. rgba=none because a diffed screenshot wants grayscale AA.
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
