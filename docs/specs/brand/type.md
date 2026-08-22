# Type

Rigel runs on three type faces with hard boundaries between them. Each face
owns a role, and crossing those boundaries is a defect, not a style choice. For
the palette these faces sit on, see [color.md](color.md); for the counter-star
R mark, see [identity.md](identity.md). The two font stacks are defined in
[`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css).

## The three faces

- **RIGEL bitmap** — the in-house heavy bitmap counter-star R. The mark
  only: the icon and the wordmark's leading R. Never body copy, never UI labels,
  never headlines. It exists as SVG geometry, not a font file.
- **Departure Mono** — the wordmark's trailing I-G-E-L and brand display
  moments. It is a bitmap face used for large brand display at 11px multiples
  only (see the constraint below).
- **Space Mono** (Colophon, OFL) — the body/UI workhorse (`--rigel-mono`),
  chosen over IBM Plex Mono for retro-futurist character while staying legible
  at 12px; Plex is kept as the fallback in the `--rigel-mono` stack. Everything
  ≤16px and every off-grid size uses `--rigel-mono`.

## The Departure-Mono 11px-grid constraint

Departure Mono is a bitmap face with UPM 550 (= 11 × 50), so its design grid is
11px tall: it renders pixel-clean at integer multiples of 11px and goes soft at
every off-grid size (12/14/16px are muddy).

There is a finer crispness detail: the face is truly crisp only at even 11px
multiples (22/44/66/88), because those land on whole device pixels across the
common DPR range. Odd multiples like 33px smear at fractional DPR
(125/150/175%), so even multiples are the safe display sizes.

The standing rule: Departure is for large brand display at 11px multiples only;
everything ≤16px and every off-grid size uses `--rigel-mono` (Space Mono,
hinted TrueType, crisp at any size). The constraint is documented at the
`--rigel-display` token in
[`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css).

## The token stacks

The two font stacks, verbatim from
[`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css):

```css
--rigel-mono: "Space Mono", "IBM Plex Mono", ui-monospace, monospace;
--rigel-display: "Departure Mono", "Space Mono", monospace;
```

### Requirement: Departure Mono is used only for large display at 11px multiples

Departure Mono (`--rigel-display`) SHALL be used only for large brand display at
integer multiples of 11px (11, 22, 33, 44…); every size ≤16px and every off-grid
size SHALL use `--rigel-mono` (Space Mono), which stays crisp at any size.

#### Scenario: a heading or label needs a type face

- **Given** text at a size that is ≤16px or not a multiple of 11px
- **When** its face is chosen
- **Then** it uses `--rigel-mono` (Space Mono), not `--rigel-display`, so it
  renders crisp rather than soft.
