# Compass motion primitives (D9)

This document plus the keyframes in `design/base.css`, `design/components/`, and
the tokens in `design/tokens.css` are the **motion contract**: the primitive
vocabulary, the keyframes each primitive runs, the `--cx-*` motion tokens it
consumes, and its reduced-motion substitution. It is the human-readable contract
`compass-ui` renders against, the sibling of `components.md`.

The whole system is one idiom: a **chase-light** — a lit segment travelling a
fixed track, discrete 1-bit cells whose illumination flows while the pixels stay
put (`brand spine.md` §"The spine"). Every primitive below is an expression of
that idiom, scaled from a 12px state dot to a full-screen boot sequence.

The product UI renders it in **pure CSS/SVG** — no client-side animation runtime
(no GSAP/Three.js/Lenis; that stack is the marketing showcase's, scoped out for
the product UI in `brand motion.md` §"The tech stack").

## Laws (brand co-reviewed)

- **Token law.** Components never hardcode a duration or an easing. Every timing
  and curve is a `--cx-*` motion token: `--cx-motion-fast` (80ms, user-caused
  feedback), `--cx-motion-base` (140ms, system-caused change), `--cx-motion-slow`
  (320ms, rare full-view morph — if a motion needs to be slower than 320ms to
  read, the motion is wrong), `--cx-ease-out` (settle), `--cx-ease-morph`
  (shared-element), `--cx-pulse-color`, `--cx-pulse-period` (1.6s),
  `--cx-stream-char-ms` (12ms), `--cx-cursor-blink` (1s). A literal `200ms` or
  `cubic-bezier(...)` in a component is a review failure, exactly like a raw hex
  (`brand motion.md` §"Consumption rule"; enforced by `cx-token-gate`). A
  `steps(N)` count is keyframe structure, not a duration, and is allowed; the
  duration of a stepped animation still comes from a token.
- **The one motion accent.** The phosphor pulse (`--cx-pulse-color`) is the sole
  sanctioned purple-in-motion, reserved for the live / loading / active signal
  (`brand motion.md` §"The one motion accent"). It is a brand motion, not a state
  color, and is never aliased into a generic `--cx-*` token.
- **The two greens.** The working state is `--cx-st-working` (`--rigel-green`
  `#addb67`); it is never the success / diff-add green `--rigel-success`
  (`#22da6e`), and the working pulse is never the purple phosphor
  (`brand color.md` §"State-color mapping").
- **Pulse budget.** At most one unbounded pulse runs per viewport region; the
  scannable state-dot column (tree, board gutter) is the sanctioned exception
  (`brand motion.md` pulse-budget Requirement).
- **Reduced-motion is substitution, not removal.** Every primitive degrades to an
  instant state change or a ≤80ms opacity crossfade; the meaning survives, only
  the travel drops, and motion never sole-carries meaning — state is always also
  in glyph / color / text (`brand motion.md` §"Reduced-motion discipline").
  `tokens.css` zeroes every duration and `--cx-pulse-period` under
  `prefers-reduced-motion: reduce` and mirrors it on `[data-reduce="on"]`, so
  token-driven travel collapses automatically; the per-primitive notes below
  cover only what needs an explicit substitution beyond that zeroing.

## Working pulse

The one recurring animation in the ADE: the `working` agent-state dot breathes at
the brand pulse cadence.

- **Where it lives.** Already shipped in `components/state-dot.css` as
  `@keyframes cx-state-dot-pulse`, gated on
  `.cx-state-dot[data-state="working"][data-alive="1"]` and run at
  `--cx-pulse-period` with `--cx-ease-out`. It is **not** duplicated here; this
  document points at it.
- **Color.** The working-state green `--cx-st-working` (`#addb67`), never the
  purple phosphor and never the success green. Working is a state, not generic
  loading (`brand color.md`; `brand state-icons.md`).
- **Budget.** One unbounded pulse per region, with the state-dot column as the
  sanctioned exception — the whole scannable column pulsing in green reads as one
  "alive" field, not competing accents.
- **Reduced-motion.** Handled upstream: `--cx-pulse-period` is zeroed in
  `tokens.css`, collapsing the pulse to a static green glyph. The other seven
  states never animate — glyph + color distinguish them (`brand state-icons.md`),
  so the set is reduced-motion-safe by construction.

## Chase-light spinner + bar

The loading topologies of the chase-light primitive (`components/loader.css`).
The spinner is the closed square loop; the bar is the open track. Both use the
**non-purple loading palette** by default: blue fill `--cx-accent`, fog head
`--cx-text` (`brand color.md` §"The loading palette").

### Spinner

A genuine 1-bit comet, not a CSS border ring. `compass-ui` emits an inline
`<svg viewBox="0 0 7 7" shape-rendering="crispEdges">` holding the 24 perimeter
cells of a 7×7 grid as 1×1 `<rect class="cx-loader-cell">`, in clockwise
perimeter order, each carrying `style="--i:<0..23>"` (its clockwise step index).
The pixels are fixed; only their lit-ness flows through them (`brand spine.md`
§"The spine").

- **Keyframe.** `@keyframes cx-loader-chase` runs on every cell, phase-offset by
  one perimeter step via a negative `animation-delay` computed from `--i`. Over
  one lap a cell ignites bright at the head color, cools across the next four
  steps to the blue body color (the staircase tail), then holds dark for the rest
  of the lap. 24 cells at a one-step offset = a comet ~5 cells long sweeping the
  perimeter clockwise, one lap per `--cx-pulse-period`. `steps(1, end)` keeps each
  cell hard ON/OFF between brightness levels — 1-bit, no sub-pixel fade — so the
  chunkiness reads as the identity, not a soft glow.
- **Comet, not uniform.** The opacity taper (head `1` → `0.6` → `0.4` → `0.24` →
  `0.12` → off) is the staircase profile: thick leading corner, one-cell tail.
  The taper gives direction — you read where it is heading, not just that it is
  busy (`brand spine.md` §"Comet, not uniform").
- **Seed reference.** The Even Realities loader mechanic — a chunky comet tracing
  a square perimeter clockwise, corners square, transcribed to a 7×7 grid, 24
  distinct step positions, ~1.25s loop (`brand spine.md` §"The seed reference").
  We reproduce the mechanic: one keyframe cycle per cell, phase-stepped one cell
  per perimeter position, the ~1.25s loop mapped onto the tokenized
  `--cx-pulse-period` (the sanctioned looping brand cadence) so the spinner shares
  the pulse system's timing rather than introducing a literal duration.
- **Color.** Default = non-purple: fog head cooling to blue body. The one
  sanctioned purple spinner — the brand-mark-in-motion moment — opts in with
  `.cx-loader[data-topology="spinner"][data-accent="phosphor"]`, which remaps head
  and body to `--cx-pulse-color`. Used at most once per surface and never adjacent
  to a state-dot field, where a purple-vs-green ring pair could misread as state
  (`brand color.md` §"The one-accent rule"; record D9).
- **Reduced-motion.** The animation is removed and the static frame-0 comet ladder
  holds — a bright fog head tapering across four cells still reads as a directional
  "loading" glyph.

### Bar

Open track, determinate only — no indeterminate bar (`brand surfaces.md`).

- **Mechanic.** `.cx-loader-fill` is a blue (`--cx-accent`) fill whose width is
  `calc(var(--cx-loader-value, 0) * 100%)`, with a fog write-head
  (`border-right` in `--cx-text`) at the leading edge. Width settles to each new
  value at `--cx-motion-base` with `--cx-ease-out`, so growth reads as a
  write-head advancing along the track — the chase-light head, on an open line.
- **Reduced-motion.** The width transition is zeroed via `--cx-motion-base` in
  `tokens.css`; the fill snaps to its value with no travel and the value stays
  legible.

## Streamed-text cadence + cursor

The trace-surface signature and the one place richness is always-on, because it
*is* the product (record D9; `brand motion.md` §micro-excellence). Keyframes in
`base.css`.

- **Ignite → cool.** A freshly-revealed character ignites at the phosphor
  `--cx-pulse-color` and cools to fog `--cx-text` behind the write-head.
  `@keyframes cx-stream-ignite` runs on `.cx-stream-char` over one
  `--cx-stream-char-ms` step with `--cx-ease-out`. `compass-ui` wraps only the
  bounded trailing window of just-drained characters; the settled prefix is a
  plain text node, so cost stays bounded across many concurrent streams.
- **Cursor.** A block write-head cursor in `--cx-pulse-color` blinks at
  `--cx-cursor-blink` via `@keyframes cx-cursor-blink` with `steps(2, start)` —
  a hard 1-bit on/off, terminal-honest, no fade.
- **Cadence.** The steady visual drain is `--cx-stream-char-ms` (12ms/char). The
  network-buffer → steady-drain decoupling, rate adaptation, and
  partial-markdown / code-fence safety are compass-ui render-loop
  responsibilities; this file specs the visual contract and keyframes, not the JS
  buffering. The region is `aria-live` (`aria-busy` while streaming) so motion is
  never the sole streaming signal.
- **Reduced-motion.** Text reveals instantly (no ignite→cool travel; it lands at
  fog) and the cursor holds solid (no blink). The `aria-live` region still
  announces streaming, so meaning survives.

## Everyday translate + fade

The default, and the majority of all motion. Everything that is not a chase-light
loader, a working pulse, a stream, or the boot sequence is plain ease-out
translate + fade (`brand motion.md` §"The one motion accent"; record D9).

- **Timing.** `--cx-motion-fast` (80ms) for user-caused feedback (hover, press,
  toggle, focus); `--cx-motion-base` (140ms) for system-caused change (panel
  open/close, row settle, toast arrival). Curve is `--cx-ease-out`; the rare
  shared-element route morph uses `--cx-ease-morph` at `--cx-motion-slow`.
- **Discipline.** Richness is the exception; restraint is the default. If a
  motion needs to be slower than `--cx-motion-slow` (320ms) to read, the motion is
  wrong. These keyframes are trivial and inline in the component CSS that needs
  them; no shared keyframe block is warranted.
- **Reduced-motion.** The durations zero in `tokens.css`, collapsing translate +
  fade to an instant state change (or a ≤80ms opacity crossfade where a bare
  cut would lose the "this is new" read).

## First-load boot-sequence

The brand names Compass first-load the highest-leverage wow: Compass starts up
like a precision instrument powering on (`brand motion.md` §"The effort/wow
curve"). CSS/SVG only, skippable, deferred behind idle time, reduced-motion
honored.

- **Stages.**
  1. **Bitmap R pixel-assembly** — the mark's R draws in cell-by-cell, a bitmap
     painting to screen, via SVG/CSS `steps()` over the cell count (the cheap
     path the brand names explicitly — no WebGL). The mark component owns the R
     geometry; this document owns the choreography.
  2. **Phosphor pulse** — the assembled R's heart cell pulses once at
     `--cx-pulse-color`, the "alive" beat (the same pulse system, fired once, not
     unbounded — it does not spend the region's pulse budget).
  3. **Wordmark** — the wordmark settles in ease-out translate + fade at
     `--cx-motion-base`.
  4. **UI behind** — the shell fades in behind the mark at `--cx-motion-base`,
     then the boot layer clears.
- **Pixel-assembly mechanic.** A single SVG/CSS `steps(N)` animation drives the
  cell reveal (each step lights the next cell), so the assembly is one cheap
  stepped keyframe rather than N individually-timed elements. Total boot stays
  fast — bounded well under a second of perceived work — and never blocks input;
  it is deferred behind idle time (`requestIdleCallback`, a compass-ui hook) and
  is skippable.
- **Ownership.** The mark component + shell own the actual R and wordmark markup
  (cross-lane); this document owns the staging, timing, and token consumption.
- **Reduced-motion.** The sequence is instant: the mark and shell appear in their
  final state with no assembly, pulse, or fades. Every launch still lands in the
  same place, just without the travel.

## Reduced-motion summary

One place to read the substitution for every primitive (substitution, not
removal; state always also carried by glyph / color / text):

| primitive | animated | reduced-motion substitution |
| --- | --- | --- |
| working pulse | green dot breathes at `--cx-pulse-period` | static green glyph (period zeroed in `tokens.css`) |
| spinner | comet sweeps the 7×7 perimeter | holds frame 0 — the static comet ladder |
| bar | fog write-head advances to value | fill snaps to value, no travel |
| streamed text | char ignites at phosphor, cools to fog | reveals instantly at fog |
| cursor | block caret blinks at `--cx-cursor-blink` | holds solid |
| everyday | ease-out translate + fade | instant, or ≤80ms opacity crossfade |
| boot-sequence | staged pixel-assembly + pulse + fades | instant final state |
