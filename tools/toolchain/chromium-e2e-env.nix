# The out-of-band Chromium for the dev-boot smoke gate (apps/ui:dev-smoke,
# design record compass-dev-boot-gate). The gate drives a headless Chromium
# through Playwright, pointed at it via launchOptions.executablePath resolved
# from PLAYWRIGHT_CHROMIUM_PATH (apps/ui/playwright.config.ts:31-33).
#
# Realized here rather than through the shared gate-tools.nix / toolchain-parity
# machinery for two reasons, mirroring how gtk-e2e-env.nix handles the gtk4 e2e
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
# Two outputs the ci.yml steps consume, realized with `nix build` (never
# `nix eval`, which strips the store context that would build the derivation):
#
#   chromium    the nix-wrapped Chromium derivation; the step reads its
#               bin/chromium out-path into PLAYWRIGHT_CHROMIUM_PATH.
#   fontconfig  a self-contained fontconfig read into FONTCONFIG_FILE by both
#               pixel-touching lanes and by the Linux dev shell (devenv.nix).
let
  lock = builtins.fromJSON (builtins.readFile ../../devenv.lock);
  node = lock.nodes.nixpkgs.locked;
  nixpkgsSrc = builtins.fetchTarball {
    url = "https://github.com/${node.owner}/${node.repo}/archive/${node.rev}.tar.gz";
    sha256 = node.narHash;
  };
  pkgs = import nixpkgsSrc { };

  # The two branded faces the design tokens name (`--rigel-mono`,
  # `--rigel-display` in apps/ui/src/design/tokens.css), then a coverage
  # fallback: the branded pair leaves 21 of the UI's 49 non-ASCII glyphs
  # uncovered, and an uncovered glyph bakes tofu into a baseline.
  #
  # Unifont, not a stock system font. DejaVu pulled DejaVu Math TeX Gyre in
  # behind it, putting a proportional math face in a monospace grid; Unifont is
  # 1-bit 16x16, so it reads as pixel-grid instead of as a foreign sans. It is
  # dual-width though (fontconfig spacing=90): 17 fallback glyphs sit at 0.5em
  # and 4 — including the gear — at 1.0em, against Space Mono's 0.612em cell,
  # so these glyphs do not land on the grid. RIG-3603 retires them to
  # BadgeGlyph-style dot-matrix SVG, which is what actually fixes that.
  fontDirs = [
    "${pkgs.google-fonts.override { fonts = [ "SpaceMono" ]; }}/share/fonts/truetype"
    "${pkgs.departure-mono}/share/fonts/otf"
    "${pkgs.unifont}/share/fonts/opentype"
    "${pkgs.unifont_upper}/share/fonts/opentype"
  ];
in
{
  # Referenced by its own store path, never merged into a buildEnv: the
  # nix-wrapped chromium resolves its sandbox helper + unwrapped browser through
  # its own bin/ wrapper scripts and store-relative references that a
  # symlink-merge would break. ci.yml reads `bin/chromium` off this out-path.
  chromium = pkgs.chromium;

  # A self-contained fontconfig: the pinned faces, and the rasterization
  # parameters. Both halves are load-bearing.
  #
  # The <dir> list makes the host's fonts invisible, so a box with 800+
  # families installed resolves the same faces as a bare runner. Format
  # subdirectories rather than each package's share/fonts, because the whole
  # tree also ships woff/woff2 duplicates and Unifont's bdf/otb/pcf bitmap
  # strikes — and a strike outsorts the scalable face at some pixel sizes, so
  # exposing them makes glyph choice depend on font size.
  #
  # The <match> block pins hinting and antialiasing, which the font set alone
  # does not. Left unset these come from fontconfig's compiled-in defaults, so
  # they are a property of the linked library rather than of this repo: under
  # the built-in hintfull, Space Mono's 612/1000em advance renders 0.26px/glyph
  # wider than under hintslight, in the same browser with the same font file.
  # A devenv.lock bump that moved fontconfig would then shift every
  # text-bearing baseline with no declared cause. rgba=none because subpixel
  # order changes per-pixel colour, and a diffed screenshot wants grayscale AA.
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
