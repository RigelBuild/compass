{ lib }:
# The compass-agent source the image bundles, as a DENYLIST. `moon.yml` is
# excluded (it drives moon tasks, not the bundle); a developer's checked-out
# `node_modules` would shadow the pinned FOD tree, and is `maybeMissing` because
# `lib.fileset` errors on a path a clean checkout lacks.
lib.fileset.difference ../packages/compass-agent (
  lib.fileset.unions [
    (lib.fileset.maybeMissing ../packages/compass-agent/node_modules)
    ../packages/compass-agent/moon.yml
  ]
)
