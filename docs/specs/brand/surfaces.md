# How the style touches each surface

The concrete per-layer mapping — one dialect across the whole system. See
[spine.md](spine.md), [identity.md](identity.md), [color.md](color.md),
[type.md](type.md), [motion.md](motion.md), and
[state-icons.md](state-icons.md) for the systems this page applies.

## Per-layer mapping

- **Motion / loaders.** The one "lit segment on a track" primitive across
  topologies, sharing the staircase-comet head so they are visibly ONE object:
  spinner (closed square loop, handles indeterminate work); bar (the mechanic
  unrolled onto an open track, handles determinate progress, blue body plus fog
  head, non-purple); ring (loop topology); skeleton / grid-ripple (2D field).
  Indeterminate bars are intentionally NOT used: the spinner covers
  indeterminate, and a bar that can't fill is a worse "working" signal.
- **Symbols / marks.** The counter-star R rendered in the SAME bitmap grid, so
  the logo lives in the same dialect as the loaders. The mark itself is LOCKED
  (see [identity.md](identity.md)); chase-light is the idiom on top of it, it
  does not reopen the mark.
- **Glyphs / state icons.** The agent-state icons are already thin single-cell
  bitmap motifs, so they are ALREADY in-dialect: a 9×9 grid, CVD-safe, with only
  `working` carrying the phosphor pulse (see [state-icons.md](state-icons.md)).
- **Type.** Dot-matrix / retro display type for headers and product codes
  (Departure Mono display over Space Mono workhorse), the sigil-led wordmark; the
  display face feels bitmap, the body stays legible mono (see
  [type.md](type.md)).
- **Color.** Night Owl throughout; purple = brand mark plus the sanctioned
  spinner exception; loading = non-purple (blue fill, fog bright head, `#0a2036`
  empty); working state always green `#addb67`; the chase-light head is the
  shared phosphor logic tying loading, streaming, and spinner into one system
  (see [color.md](color.md)).

## The marketing site (as-built)

The first full expression of the brand system is a real Astro static marketing
site, pure CSS/SVG motion with zero JS runtime deps (a `<details>`-based CSS
dropdown). This is the concrete per-surface application the system describes in
the abstract, and it is the RESTRAINT end of the effort/wow curve
([motion.md](motion.md) §"The effort/wow curve per surface"), deliberately: the
full GSAP / Three.js / Lenis / Barba stack ([motion.md](motion.md) §"The tech
stack") is the aspiration, not yet built.

### Chrome (every page)

- **Topbar:** the sigil-led wordmark (inline bitmap R in purple plus Departure
  IGEL), a Compass nav dropdown (CSS-only `<details>`), then the value page,
  Pricing, Design, and a GitHub icon link. The sigil carries the phosphor pulse.
- **Footer:** an "R Rigel" display mark (fog, not purple, a typographic R),
  GitHub plus LinkedIn `currentColor` icon links, a Compass-repo link carrying
  the Compass needle TILE mark (full-colour, the product mark), and a
  reduce-motion demo toggle.
- **Scanlines:** a fixed CRT wash (`repeating-linear-gradient`, roughly 0.5%
  opacity).

See [compass-mark.md](compass-mark.md) for the Compass needle tile mark.

### Anti-slop rules the site holds to

- No 3- or 4-card EQUAL grids; ONE dominant element per section.
- Inline links are blue (the flow color); purple never appears off the mark.
- Departure Mono display only at even 11px multiples (22/44/66/88); max hero
  88px (see [type.md](type.md)).

### The board reference render

The company brand system ships production-quality mockups of the Compass
supervision surfaces — the Bridge issues/PRs board, the Manager tree, and the
thread view — each authored as "the real UI would ship it", citing the real
compass components, as the excellence bar the product UI builds toward. These
are the canonical brand reference renders for those surfaces; a compass design
record treats them as the starting-point reference for the board's visual
structure (hairline grid, panel-tier lane gutters, flat square cards, quiet-key
/ bright-title hierarchy, display-face heading, CI/review pips, the advancing
chase-light sweep), not a redraw.
