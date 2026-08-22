# The spine: the chase-light idiom

The one mechanic the whole visual system is built on. Everything downstream (see
[identity.md](identity.md), [color.md](color.md), [type.md](type.md),
[motion.md](motion.md), [state-icons.md](state-icons.md)) is an expression of
this idiom.

The whole Rigel visual system is narrowed to ONE style: a bitmap / dot-matrix
"chase-light" idiom, discrete ON/OFF pixel cells where the illumination
*travels* (a lit segment flows along a fixed track), not the shape. This is not
a loader treatment; it is the SPINE of the identity. Motion, micro-excellence,
symbols, glyphs, type, and color are all expressed in the one chase-light /
dot-matrix dialect.

The precise mechanic, as locked:

- **Propagating light-segment on a fixed pixel track.** Discrete cells activate
  in sequence along a path; persistence of vision (beta movement / the phi
  phenomenon, the theatre-marquee trick) fuses them into continuous directional
  flow. The pixels do not move; the lit-ness flows through them.
- **1-bit / bitmap-native.** Cells are hard ON or OFF on a coarse grid. No
  anti-aliasing, no sub-pixel. The chunkiness is the point.
- **Comet, not uniform.** The lit segment has a staircase profile: thick at the
  leading corner, tapering to a one-cell tail. The taper gives it direction; you
  read where it is heading, not just that it is busy.
- **Frameless.** The unlit track is invisible; you see only the flow.
- **One primitive, many topologies.** The same "lit segment on a track" becomes
  a spinner (closed square loop), a bar (open line), a ring (loop), and a
  skeleton / grid ripple (2D field). This is the unifying idea everything is
  built on.

The UX nuance to keep: a rotating spinner communicates process ("busy,
timeless"); a propagating segment adds *direction*, which implies progression
along a path, which is exactly why the style extends to a bar so naturally, a
bar being a directional path. The shared chase-light "head" (a bright leading
cell plus subtle glow, cooling to the fill color behind it) is the phosphor
logic that makes loading, streaming, and the spinner feel like one system.

The seed reference is a chunky comet segment (~10-12 lit cells, staircase
profile) tracing a square perimeter clockwise, corners square, on a 7×7 grid,
24 distinct keyframes, ~1.25s loop. This discrete style superseded an earlier
flow-field / particle "falling sand" loader treatment; the discrete style won
because it is distinctive, cheap, on-brand (bitmap), and matches the seed
reference. Do not relitigate it.

## Rendering

### Requirement: the bitmap track renders with crisp, un-antialiased cells

Chase-light surfaces SHALL render as 1-bit cells with hard edges
(`shape-rendering="crispEdges"`), never anti-aliased or sub-pixel-smoothed, so
the discrete pixel grid reads as the identity rather than a soft glow.

#### Scenario: a chase-light element is drawn at any DPR

- **Given** a spinner, bar, ring, or skeleton rendered from the chase-light
  primitive
- **When** it is painted at any device-pixel ratio
- **Then** its cells have hard ON/OFF edges with no anti-aliasing, and any seam
  between adjacent lit cells is a 1px non-scaling stroke that stays crisp at any
  size.
