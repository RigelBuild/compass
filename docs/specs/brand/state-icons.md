# State-icon vocabulary

Eight agent-state icons on a 9×9 bitmap grid, static by default and CVD-safe.
Only `working` carries `data-alive=1` and animates: the working pulse (the pulse
cadence in the working-state green `#addb67`, not the purple phosphor accent of
[motion.md](motion.md)) runs in the parent. See [color.md](color.md) for the
state colors and [motion.md](motion.md) for the pulse cadence.

## The eight states

| state | glyph | notes |
| --- | --- | --- |
| working | double-chevron `»` (fast-forward) | two forward-pointing staircase chevrons; the ONLY animated state (the working pulse, green `#addb67`) |
| idle | 3×3 block | resting |
| waiting | `?` | |
| done | check-tick | |
| paused | two bars | |
| stopped | hollow square outline | |
| error | `!` | |
| disconnected | broken square outline | |

The `working` form was ruled over an earlier ring, which read as a bare "C" at
the 12px row-dot size; the forward-chevron pair reads "advancing / in progress"
at row-dot size and its forward direction is the split from done's check-tick.
Motion is reserved for *alive*: `stopped`, `disconnected`, and `paused` have no
motion; the glyph and color carry them.

### Requirement: only the working state animates; the others are distinguished statically

Among the eight agent-state icons, only `working` SHALL animate (the working
pulse, the pulse cadence in the working-state green `#addb67`, not the purple
phosphor accent; see [color.md](color.md) and [motion.md](motion.md)); the other
seven SHALL be distinguished by glyph and color alone, so the set stays CVD-safe
and reduced-motion-safe.

#### Scenario: a row shows a non-working agent state

- **Given** an agent in idle, waiting, done, paused, stopped, error, or
  disconnected
- **When** its state icon is rendered
- **Then** the icon is static and is distinguished by its glyph and color, with
  no motion.
