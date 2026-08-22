# Identity — the counter-star R

The corporate mark: a heavyweight bitmap R that holds a knocked-out four-point
star, plus the sigil-led wordmark built from it. The chase-light idiom (see
[spine.md](spine.md)) is the motion layer on top of this mark and does not
reopen it. The Compass **product** mark is a sibling to this company mark,
specified separately in [compass-mark.md](compass-mark.md).

## The counter-star

A heavyweight bitmap slab R with a true 4-point star knocked out of its enclosed
counter. The letter *holds* the star — R and star share the same pixels. The R
is the company; the star is Rigel itself, Beta Orionis. One device carries both,
which is why nothing else in the system ever needs a second logo, badge, or
emblem.

Construction and rendering:

- **Tile:** the R sits on a rounded-corner tile with a safe margin, so it never
  touches the tile edge. It is a bitmap mark on a fixed pixel grid — a heavy
  stem and diagonal leg, a solid bowl, the star a knocked-out plus with corner
  ticks.
- **Rendering:** the bitmap face *is* the mark. `shape-rendering="crispEdges"`,
  always — no anti-aliased redraws, no vector smoothing, no outlining.
- **Master asset:** the navy icon SVG is the locked master; every other
  rendition (mono, maskable, coral, phosphor) derives from it.

## The wordmark is sigil-led

The leading bitmap R *is* the mark; Departure Mono carries the trailing
I-G-E-L, sized to sit against the R on a shared baseline so its pixel grid keeps
the word coherent with the mark while staying live text. There is no tagline;
the wordmark alone is the lockup.

## Which mark on which surface

| surface | asset | notes |
| --- | --- | --- |
| favicon 16–64 | navy icon | rounded tile, transparent corners |
| apple-touch (180) | square icon | full-bleed square; iOS masks its corners |
| PWA any-purpose 192/512 | navy icon | |
| PWA maskable 192/512 | maskable icon | generous safe zone; survives circle/squircle crops |
| monochrome / pinned-tab / notification | mono icon | fog R, zero purple, identical geometry; also the CI/automation-bot machine-identity avatar |
| machine-identity avatar (agent) | coral icon | coral R (`#f78c6c`, from the syntax palette, no state meaning); a scoped one-accent exception for GitHub avatars only |
| loading / active | phosphor icon | the only permitted `#b57eff` use |
| wordmark default | fog wordmark | all fog (bitmap R + Departure I-G-E-L), mono-native |
| wordmark hero / site header | accent wordmark | leading R purple, star knocked to navy, I-G-E-L fog |
| light mode / print | mono wordmark | navy on fog, polarity inversion only |
| CLI banner | `R:` mark | SECONDARY, terminal-only — never a primary icon |
| social / OG | OG card (1200×630) | accent wordmark on navy, slate `RIGEL · BETA ORIONIS` footer |

## Clear space and minimum size

- **Clear space:** one stem width on all sides of icon or wordmark. Inside the
  app tile the safe area already provides it.
- **Minimum size, icon:** 16px is the floor — judged at true 16px
  nearest-neighbor; below 16px use nothing (no shrunken fallback glyph exists or
  is permitted).
- **Minimum size, wordmark:** 24px tall.

## Dead ideas

Do not resurrect: the lit foot-pixel star; icon + RIGEL two-R lockups; open-bowl
R variants; any gradient treatment.

### Requirement: the icon never renders below its 16px floor

The counter-star icon SHALL NOT be rendered below 16px, and the wordmark SHALL
NOT be rendered below 24px tall; there is no smaller fallback glyph.

#### Scenario: a surface needs a mark smaller than the floor

- **Given** a context that would place the icon below 16px (or the wordmark
  below 24px)
- **When** the mark is applied
- **Then** the mark is omitted rather than shrunk — no downscaled or substitute
  glyph is used.

### Requirement: no icon-beside-wordmark lockup

Because the wordmark is sigil-led (its leading R *is* the mark), a composed
icon-beside-wordmark lockup SHALL NOT be built — it would place two Rs on one
lockup.

#### Scenario: a lockup is composed

- **Given** a surface needing a brand lockup
- **When** the lockup is assembled
- **Then** the wordmark stands alone as the lockup, and the icon does not
  reappear beside the word.
- **And** an icon-only context adjacent to a text context (e.g. a browser tab's
  favicon beside the page title) is fine — only a *composed* two-R lockup is
  banned.
