# Color

The Rigel palette is the complete Night Owl set. It dresses the whole product
UI and drives the Compass editor theme, not just the brand surfaces. No new
colors, ever. Contrast ratios are computed WCAG 2.x on this exact hex set.
Every hex below matches the authoritative in-repo token file
([`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css)),
which Compass consumes read-only. For type, see [type.md](type.md); for how the
palette lands per surface, see [surfaces.md](surfaces.md); for the state
vocabulary, see [state-icons.md](state-icons.md).

## Surfaces

The dark scaffolding of the night sky.

| swatch | hex | role | contrast |
| --- | --- | --- | --- |
| navy | `#011627` | background — the night sky | fog on it 13.54:1 AAA |
| raised | `#0b2942` | raised surface (cards, dock, pills) | fog on it 11.00:1 AAA |
| panel | `#0e2a45` | panel / border / inset | fog on it 10.80:1 AAA |
| selection | `#1d3b53` | selection / highlight | fog on it 8.60:1 AAA |

## Text on navy

Primary through disabled text, all measured against navy.

| swatch | hex | role | contrast |
| --- | --- | --- | --- |
| fog | `#d6deeb` | primary text; the mono-native mark color | 13.54:1 AAA |
| bright | `#c5e4fd` | emphasis / active text | 13.87:1 AAA |
| muted | `#5f7e97` | secondary text, chrome | 4.29:1 AA-large |
| haze | `#89a4bb` | tertiary labels | 7.06:1 AAA |
| faint | `#637777` | comments, disabled | 3.87:1 AA-large |

## Syntax / UI

The Night Owl canonical set that dresses the Compass editor. These are UI
colors, not brand colors, and are never used as brand color on marketing
surfaces.

| swatch | hex | role | contrast |
| --- | --- | --- | --- |
| blue | `#82aaff` | functions · links · interaction | 7.98:1 AAA |
| cyan | `#7fdbca` | support · operators | 11.25:1 AAA |
| magenta | `#c792ea` | keywords · tags | 7.62:1 AAA |
| green | `#22da6e` | diff-add · success | 9.88:1 AAA |
| string | `#ecc48d` | strings | 11.22:1 AAA |
| coral | `#f78c6c` | numbers · params | 7.79:1 AAA |
| red | `#ef5350` | errors · diff-del | 5.26:1 AA |
| yellow | `#addb67` | attributes · warnings (same hex as the working-state green, distinct role) | 11.44:1 AAA |

## Brand

The sole accent tier.

| swatch | hex | role | contrast |
| --- | --- | --- | --- |
| purple | `#a66ef5` | THE brand accent — the mark's R only | 5.39:1 AA |
| phosphor | `#b57eff` | lit state only (loading / active pulse) | 6.45:1 AA |

## The one-accent rule

Purple is the sole brand accent, and it appears in exactly one place per
surface: the mark (the icon's R, or the wordmark's leading R). Never body text,
never buttons, never charts, never borders. If a layout seems to need more
purple, the layout is wrong. Purple is reserved for the brand mark/icon, the
streaming phosphor accent, and the spinner (the one sanctioned purple loader,
the brand-mark-in-motion moment); everything else loading is non-purple.
Phosphor (`#b57eff`) is a state, not a color choice: the star's heart cell
pulsing during loading/active. Never a static fill, never text.

The focus ring is blue (`--rigel-blue` `#82aaff`, the interaction color), not
purple. This keeps the one-accent rule literally true: the only purple on a
surface stays inside the mark, and interaction affordances live on the blue
flow color instead
([`apps/ui/src/design/tokens.css`](../../../apps/ui/src/design/tokens.css):
`--cx-focus-ring: 2px solid var(--rigel-blue)`).

## The loading palette (non-purple)

Loading (bar / skeleton / pulse) uses fill blue `#82aaff`, bright head fog
`#d6deeb`, and empty `#0a2036`.

## State-color mapping

- **working** = green `#addb67`, everywhere: the state dot, and, when it
  animates, the "agent working" pulse breathes in green (the pulse cadence in
  the working-state color, not the purple phosphor accent of the motion
  system). Never purple, never blue-generic, for the working state
  specifically.
- **done** = teal / cyan `#7fdbca`.
- **waiting / disconnected** = amber `#ecc48d`.
- **error** = red `#ef5350`.
- **idle / paused / stopped** = slate / muted.

Note the two greens are distinct roles and must not be swapped: `#addb67` is
the working-state green; `#22da6e` is the syntax-tier green (diff-add /
success). No gradients anywhere, flat fills only. Light mode / print is a
polarity inversion (navy mark on fog), not a redesign.

### Requirement: purple appears in exactly one place per surface

Purple (`#a66ef5`) SHALL appear in exactly one place per surface, inside the
mark (icon R or wordmark leading R), and SHALL NOT be used for body text,
buttons, links, borders, charts, or chrome. Phosphor (`#b57eff`) SHALL be used
only for a lit/active state (loading or active pulse), never as a static fill
or text.

#### Scenario: a surface is composed

- **Given** any brand or product surface
- **When** its colors are assigned
- **Then** at most one purple accent is present, and it sits inside the mark; no
  purple appears on body, buttons, links, borders, or chrome.

#### Scenario: a control shows a working state

- **Given** a control or row entering the working state
- **When** its state color is chosen
- **Then** it uses green `#addb67` (not purple, not generic blue), because
  working is a state, not generic loading.

### Requirement: no gradients and no off-palette colors

Surfaces SHALL use flat fills only, no gradients on the mark or anywhere else,
and SHALL NOT introduce colors outside the Night Owl set, including "close"
purples.

#### Scenario: a fill or accent is specified

- **Given** any fill, accent, or state color on a Rigel surface
- **When** it is specified
- **Then** it is a flat fill drawn from the Night Owl set, with no gradient and
  no near-palette substitute.
