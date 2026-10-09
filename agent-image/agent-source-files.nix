{ lib }:
# The compass-agent source the image bundles. A bare path would take a
# developer's checked-out `node_modules`, shadowing the pinned FOD tree
# (`maybeMissing`: a clean checkout lacks it); `moon.yml` drives moon, not the bundle.
lib.fileset.difference ../packages/compass-agent (
  lib.fileset.unions [
    (lib.fileset.maybeMissing ../packages/compass-agent/node_modules)
    ../packages/compass-agent/moon.yml
  ]
)
