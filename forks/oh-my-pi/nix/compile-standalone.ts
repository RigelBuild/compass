// Compile the omp standalone binary via upstream's compileCodingAgent helper.
//
// Since v16.4.6 the compiled binary needs the in-memory `omp-legacy-pi-modules`
// virtual module, which is provided only by a Bun.build() plugin
// (packages/coding-agent/scripts/legacy-pi-virtual-module.ts). The
// `bun build --compile` CLI cannot load Bun build plugins, so drive upstream's
// own compile function directly instead of hand-rolling the invocation — this
// tracks upstream's build across subtree bumps rather than reimplementing it.
//
// Run from packages/coding-agent (as upstream's build-binary.ts does) so the
// bare package specifiers the virtual-module plugin emits resolve the same way:
//   bun compile-standalone.ts <bun-compile-target>

import { createRequire } from "node:module";
import * as path from "node:path";

const target = process.argv[2];
if (!target) throw new Error("usage: compile-standalone.ts <bun-compile-target>");

const codingAgentDir = process.cwd();
const repoRoot = path.resolve(codingAgentDir, "..", "..");

// Dynamic import (deliberate ts-no-dynamic-import exception): this file is a
// sealed nix-support script that executes from a detached /nix/store path, while
// compile-binary.ts lives in the unpacked build tree. That module's absolute
// path is only known at runtime (the sandbox build dir), so a static relative
// import from the store cannot resolve it — the specifier is genuinely
// runtime-selected, the case the rule carves out.
const { compileCodingAgent } = await import(path.join(codingAgentDir, "scripts", "compile-binary.ts"));

// Mirror scripts/build-binary.ts: embed the concrete Transformers.js version so
// the tiny-model worker can pin its runtime install against it.
const require = createRequire(path.join(codingAgentDir, "package.json"));
const transformersManifest = require("@huggingface/transformers/package.json") as {
  version: string;
};

await compileCodingAgent({
  repoRoot,
  entrypoint: path.join(codingAgentDir, "src", "cli.ts"),
  outfile: path.join(repoRoot, "dist", "omp"),
  transformersVersion: transformersManifest.version,
  target: target as Bun.Build.CompileTarget,
});
