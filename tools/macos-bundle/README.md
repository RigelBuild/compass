# macos-bundle

Builds a macOS `Compass.app` and compressed UDZO `.dmg` from the shell binary,
UI distribution, and sidecar binaries. It templates `Info.plist`, ad-hoc signs
the app, and wraps it in a disk image. It does not notarize the app.

## Run

```sh
moon run macos-bundle:ci
bun run tools/macos-bundle/index.ts \
  --binary /tmp/compass-app \
  --dist apps/ui/dist \
  --version 1.2.3 \
  --sidecar /tmp/compass-stack \
  --out /tmp/compass.dmg
```

The `ci` task runs typecheck and unit tests. The full bundling command runs in
`.github/workflows/ci.yml` job `darwin` and `.github/workflows/release.yml` job
`release-assets-macos`.

## Credentials

None. Signing uses `codesign --sign -` for ad-hoc signing. The workflows do not
provide Apple signing or notarization credentials.

## Rotate

Nothing to rotate.
