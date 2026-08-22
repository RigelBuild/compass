# Compass mark — the needle

The Compass **product** mark, a sibling to the Rigel counter-star R specified in
[identity.md](identity.md). Rigel is the company (a star); Compass is the
product (an agentic software factory). The company mark is the counter-star R;
the product needs its own mark for the places Compass appears on its own.

## What it is

A compass needle floating inside a diamond bezel, built on the SAME locked tile
as the R:

- **Tile:** 96×96, 8px cells, corner radius 16, navy `#011627` ground,
  `shape-rendering="crispEdges"` — identical to the R's construction (see
  [identity.md](identity.md)).
- **Needle:** two-tone directional. The NORTH half is lit phosphor `#b57eff`
  (the brand's lit/active color, see [color.md](color.md)); the SOUTH half is
  hollow (outline only). North-solid / south-hollow is the universal
  magnetized-needle read, and it degrades to a clear north-pointer at favicon
  size.
- **Accent:** purple lives on the mark, so the tile keeps its accent. The
  one-accent rule (see [color.md](color.md)) exempts the mark itself, the same
  exemption granted to the R sigil.
- **Placement:** centered on the true tile center on both axes (symmetric).

## Status

**Placeholder.** It is wired as the footer Compass-repo icon and is usable as a
placeholder but needs more iteration. The R mark is LOCKED; the Compass mark is
NOT yet locked.

## Rules it inherits

As a true sibling to the R, the Compass mark honors the identity system's
construction discipline rather than sitting beside it as a foreign object:

- Same 8px grid, same navy tile, same `crispEdges` rendering.
- Purple `#a66ef5` / phosphor `#b57eff` drawn from the locked palette (see
  [color.md](color.md)).
- The 16px icon floor applies, since it is a bitmap tile mark.

### Requirement: the Compass mark honors the R's tile discipline

The Compass mark SHALL be constructed on the locked 96×96 navy `#011627` tile at
8px cells with `shape-rendering="crispEdges"`, SHALL draw its accent only from
the locked purple/phosphor palette, and SHALL NOT be rendered below the 16px
icon floor; there is no smaller fallback glyph (see [identity.md](identity.md)
§"Clear space and minimum size").

#### Scenario: the Compass mark is applied to a surface

- **Given** a surface that shows the Compass product mark
- **When** the mark is rendered
- **Then** it is drawn on the locked navy tile at 8px cells with `crispEdges`,
  its lit north half uses phosphor `#b57eff` from the locked palette, and it is
  never placed below 16px.
- **And** below the 16px floor the mark is omitted rather than shrunk, matching
  the R's floor rule (see [identity.md](identity.md)).
