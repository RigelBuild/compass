# Rigel brand + design system (compass reference)

This is the compass-owned brand reference, based off the canonical Rigel
company brand system. Rigel is the company (Beta Orionis, the blue-white
supergiant star); Compass is the product (an agentic software factory). The
company brand system is the source of truth for the visual language; this
reference states the parts of it that Compass depends on, on their own terms,
so a compass design record can cite a public compass doc rather than a
company-internal one.

It is not a second, competing brand spec: the design-system *code* that this
reference describes already lives in this repo under
[`apps/ui/src/design/`](../../../apps/ui/src/design/) — the primitive token
set ([`tokens.css`](../../../apps/ui/src/design/tokens.css)), the base layer,
and the component CSS. Where a value matters, this reference cites that
in-repo code as ground truth rather than restating a company-internal
document. Some crossover with the company brand is unavoidable and accepted:
the token values, the palette, and the type/motion rules are shared by
construction, because Compass mirrors the brand primitive tier verbatim.

## The thesis

Rigel's identity is one idiom, the **chase-light / dot-matrix spine**,
expressed at every scale, from a 12px state dot up to a full-screen hero. A lit
segment travels along a fixed pixel track (not the shape); discrete ON/OFF
cells fuse into directional flow. That single mechanic generalizes across
motion, marks, glyphs, type, and color, so the whole system reads as one hand.

## How this reference is organized

This reference is a directory of focused files. Each owns one layer of the
system and cross-references its siblings by relative path.

| File | What it covers |
| --- | --- |
| [README.md](README.md) | This index: the thesis and the file map. |
| [spine.md](spine.md) | The chase-light idiom, the one mechanic the whole system is built on. |
| [identity.md](identity.md) | The counter-star R mark and the sigil-led wordmark. |
| [compass-mark.md](compass-mark.md) | The Compass product needle mark. |
| [color.md](color.md) | The Night Owl palette and the one-accent rule. |
| [type.md](type.md) | The three faces and the 11px-grid constraint. |
| [motion.md](motion.md) | The motion system and the marketing-showcase tech stack. |
| [state-icons.md](state-icons.md) | The eight-state agent glyph vocabulary. |
| [surfaces.md](surfaces.md) | Per-surface application of the system. |

Load-bearing rules (the ones a reviewer or a build could violate) are stated as
`### Requirement:` + `#### Scenario:` contracts (RFC 2119). Everything else is
prose.
