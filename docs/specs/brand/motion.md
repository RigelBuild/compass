# Motion system and tech stack

The motion system is how the spine scales up: the same chase-light logic, from a
12px pulse to a full-screen hero. This file folds the motion vocabulary, its
tokens, and its reduced-motion discipline together with the tech stack that
renders it. See [spine.md](spine.md) for the chase-light idiom the motion
serves, and [state-icons.md](state-icons.md) for the one animated agent state.
The motion/timing tokens are defined in
[`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css).

## The motion system

Register: **precision instrument that comes alive** — retro-computing honesty
plus high-craft motion, never arcade cheese.

### The five motion primitives

The vocabulary is five signatures, reused everywhere:

1. **Pixel-assembly.** The mark, wordmark, and headings draw in cell-by-cell,
   like a bitmap painting to screen. The signature load motion, tied directly to
   the bitmap R. Cheap: SVG/CSS `steps()` or per-cell stagger, no WebGL needed.
2. **Phosphor pulse.** The star's heart cell glows and pulses `#b57eff` on load,
   active, and loading states. The signature micro-interaction, the "cursor
   blink" of the brand.
3. **Blur-to-sharp text.** A split-text reveal in Departure Mono for every
   heading or hero line: `filter: blur(12px)→blur(0)` plus opacity, characters
   settling out of order; set `will-change` only during the tween and strip it
   after.
4. **Boot-sequence.** A staged, monospaced, terminal-honest reveal for
   first-loads. The premium version of a spinner.
5. **CRT-honest texture.** A subtle scanline/glow on hero surfaces only, never
   body; the retro signal dialed to roughly 10%, not 100%.

### The effort/wow curve per surface

Where each surface sits on the effort/wow curve:

- **Marketing site (the showcase).** The full timeline / smooth-scroll / 3D
  stack; one hero object (the counter-star as an interactive 3D/shader object
  with magnetic hover and pixel-assembly on load); blur-to-sharp headings; CRT
  texture on the hero. This is where the WebGL budget is spent.
- **Compass first-load (the highest-leverage wow).** The boot-sequence: Compass
  starts up like a precision instrument powering on, bitmap R pixel-assembles,
  phosphor pulse, wordmark, then the UI fades in behind it. Terminal-honest,
  fast (deferred behind idle time), skippable, and respects reduced-motion.
  Every user sees it every launch.
- **Docs and app chrome (restraint).** Page transitions plus reveal-on-scroll
  plus the phosphor pulse for loading. No heavy WebGL; legibility and speed win
  here.

### The motion tokens

The discipline that keeps it coherent:

- **Durations.** `--cx-motion-fast: 80ms` (user-caused feedback: hover, press,
  toggle, focus); `--cx-motion-base: 140ms` (system-caused change: a panel
  opening, a row settling, a toast); reveal 500-800ms for hero/text. A third,
  rare speed `--rigel-motion-slow: 320ms` is capped and used almost never. If a
  motion needs to be slower than 320ms to read, the motion is wrong.
- **Easing.** One curve family. The "settle" curve is `cubic-bezier(0.16, 1,
  0.3, 1)` (`--rigel-ease-out`); the shared-element morph curve is
  `cubic-bezier(0.65, 0, 0.35, 1)` (`--rigel-ease-morph`).
- **The one motion accent.** The phosphor pulse (`#b57eff`,
  `--rigel-pulse-period: 1.6s`) is the sole motion accent, exactly as purple is
  the one color accent, reserved for the live / loading / active signal. The
  working agent-state dot reuses the same pulse cadence in green `#addb67` (a
  state color, not the purple accent; see [color.md](color.md)). Everything else
  is plain ease-out translate plus fade.

The related timing tokens are `--rigel-stream-char-ms: 12ms` (streamed-text
cadence) and `--rigel-cursor-blink: 1s`.

**Consumption rule** (mirrors the color tier): components never hardcode a
duration or an easing; they read `--cx-motion-*` or `--rigel-motion-*`. A
literal `200ms` in a component is a review failure, exactly like a literal hex
is.

### Reduced-motion discipline

Motion honors `prefers-reduced-motion` everywhere; the rule is *substitution,
not removal*. Motion becomes an instant state change (or a pure opacity
crossfade ≤80ms), and the information a motion conveys (this is new, this is
loading, this settled) survives; only the travel is dropped. The spinner holds
frame 0, the dither holds the middle tile, the pulse goes static, and the token
file zeroes all durations and `--rigel-pulse-period` under the media query
(plus a `data-reduce="on"` manual toggle).

### Requirement: motion honors prefers-reduced-motion and never sole-carries meaning

Every animated primitive SHALL honor `prefers-reduced-motion` by substituting an
instant state change or an opacity crossfade ≤80ms for travel, and SHALL NOT be
the sole channel that carries a meaning; the color/glyph/text still distinguishes
the state when motion is removed.

#### Scenario: a user has reduced motion enabled

- **Given** a user with `prefers-reduced-motion: reduce`
- **When** a loading, working, arrival, or transition primitive would animate
- **Then** the animation degrades to an instant state change or a ≤80ms
  crossfade, and the state remains distinguishable by color/glyph/text alone.

### Requirement: the phosphor pulse is the only motion accent

The phosphor pulse (`#b57eff`) SHALL be the only motion *accent*, the sole
sanctioned purple-in-motion, reserved for a live / loading / active signal (the
mark's heart cell on load, loading indicators, streaming text). It is a brand
motion, not a state color: the working *agent-state* dot uses the same pulse
cadence in its state color green `#addb67` (see [color.md](color.md)), never the
purple phosphor. At most one unbounded pulse SHALL run per viewport region (the
scannable state-dot column is the sanctioned exception).

#### Scenario: multiple elements want to pulse

- **Given** more than one candidate for the phosphor pulse in one viewport region
- **When** the region is composed
- **Then** only one unbounded pulse runs (or the pulses form a single scannable
  state-dot field), so nothing else competes for the "alive" signal.

## The tech stack

Award sites converge on one stack; the taste is in restraint. The toolkit:

| Layer | Tool | Job |
| --- | --- | --- |
| Timelines / transitions | GSAP (+ `useGSAP`) | The spine: every timeline, page transition, component animation |
| Scroll | GSAP ScrollTrigger / ScrollSmoother | Pinned sections, scrubbed sequences, reveal-on-enter |
| Text | GSAP SplitText | Per-char / word / line staggered reveals |
| Smooth scroll | Lenis | Bound to `gsap.ticker` so DOM + WebGL move in the same frame |
| 3D / shaders | Three.js (raw) | Hero symbols, shader reveals, CRT effects |
| Page transitions (MPA) | Barba.js | A clicked element visually travels between pages, no white flash |
| Sound (optional) | Web Audio API | Runtime-generated UI blips, not audio files |
| Framework | Astro (marketing MPA) | Astro for a lightweight marketing MPA; the product-UI framework is out of this reference's scope |

The key integration insight: the magic is not any one library, it is **one
shared render loop**. Lenis drives from `gsap.ticker`; ScrollTrigger, WebGL, and
DOM all update in the same execution block. That is why award sites feel
coherent instead of like a pile of plugins. Stay fast with a `transitionReady`
flag plus idle-deferred work: don't start the hero animation until the loader
clears, and defer non-critical work off the initial load.

This full stack is the marketing showcase's aspiration; the product UI expresses
the *same motion vocabulary* on a pure CSS/SVG substrate instead (see
[surfaces.md](surfaces.md)).
