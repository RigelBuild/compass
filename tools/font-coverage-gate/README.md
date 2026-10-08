# font-coverage-gate

Scans rendered UI source for literal non-ASCII characters and checks that
Space Mono contains each character. It also parses Departure Mono as a font
format sanity check; missing coverage fails by default.

## Run

```sh
moon run font-coverage-gate:check
moon run font-coverage-gate:ci
FONT_COVERAGE_GATE=warn bun run tools/font-coverage-gate/index.ts
```

Use WARN mode only for a non-blocking report; the default is ERROR. CI runs the
selected task in the `moon (bun)` job of `.github/workflows/ci.yml`.

## Credentials

None. The gate scans checked-in UI source and font files; it does not call an
authenticated service.

## Rotate

Nothing to rotate.
