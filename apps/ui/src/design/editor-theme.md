# Editor theme (D8)

The embedded code editor and any Shiki-highlighted content share the chrome
palette: syntax on the `--cx-ed-*` semantic tokens resolves to the same Night
Owl primitives the surrounding UI uses, so an editor pane is indistinguishable
in palette from the chrome around it (frozen record D8).

This side owns **the palette mapping** — the `--cx-ed-*` token set in
`tokens.css` and the resolved Shiki theme artifact. Wiring the theme into an
editor component is a later adoption task and is not specified here.

Conventions:

- Every `--cx-ed-*` value is a `var(--cx-*)` or `var(--rigel-*)` reference — no
  raw hex in the semantic tier (enforced by `cx-token-gate`).
- The two-greens rule is a hard invariant: attributes / working =
  `--rigel-green` (`#addb67`); diff-add / success = `--rigel-success`
  (`#22da6e`). They are never swapped.
- No purple anywhere in the editor theme — purple is the brand mark only.

## `--cx-ed-*` token set

| token | role | source primitive |
| --- | --- | --- |
| `--cx-ed-bg` | editor surface (night sky) | `var(--cx-bg)` → `--rigel-night` |
| `--cx-ed-text` | primary editor text (fog) | `var(--cx-text)` → `--rigel-fog` |
| `--cx-ed-selection` | selection / highlight surface | `--rigel-selection` |
| `--cx-ed-cursor` | caret (interaction color) | `--rigel-blue` |
| `--cx-ed-gutter` | line-number / gutter faint | `--rigel-faint` |
| `--cx-ed-fn` | functions · links · interaction | `--rigel-blue` |
| `--cx-ed-op` | support · operators | `--rigel-cyan` |
| `--cx-ed-keyword` | keywords · tags | `--rigel-magenta` |
| `--cx-ed-string` | strings | `--rigel-amber` |
| `--cx-ed-number` | numbers · params · language constants | `--rigel-syntax-coral` |
| `--cx-ed-attr` | attributes · warnings (working green) | `--rigel-green` |
| `--cx-ed-comment` | comments · disabled | `--rigel-faint` |
| `--cx-ed-diff-add` | diff-add · success (distinct green) | `--rigel-success` |
| `--cx-ed-diff-del` | diff-del | `--rigel-red` |
| `--cx-ed-error` | errors | `--rigel-red` |

## Shiki theme

`editor-theme.json` is the resolved Shiki / TextMate dark theme
(`{ name, type: "dark", colors, tokenColors }`). A JSON file cannot read CSS
custom properties, so it carries the **resolved** Night Owl hex values — this
is the one sanctioned artifact where the resolved hexes appear, because it is a
highlighter input, not a CSS rule. Its `colors` map the editor UI
(`editor.background`, `editor.foreground`, `editor.selectionBackground`,
`editorCursor.foreground`, `editorLineNumber.foreground`) and its `tokenColors`
map standard TextMate scopes to the same ramp the `--cx-ed-*` tokens carry.

When the two ever drift, `tokens.css` is the source of truth and
`editor-theme.json` is regenerated from it.
