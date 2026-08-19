#!/usr/bin/env bun

// agent-image-env-gate — assert the built compass-agent image is not a
// wrong-image build.
//
// A green `container build` proves the image REALISES; it does not prove the
// image is CORRECT. Three wrong-image defects have shipped through a successful
// build, each caught only by hand-inspecting the image (see env-check.ts for
// the catalogue). This gate closes that hole: it builds the exact same spec the
// publish lane ships, inspects its OCI config, and fails on the signals those
// defects leave behind — a leaked `DEVENV_` env key, a build-host home path in
// the env, or a platform-contract mismatch.
//
// Runs the same fork-pinned derivation as `dogfood:agent-image` and
// `agent-image/publish.sh`, from the `agent-image/` cwd so the `path:../forks/*`
// flake refs resolve. In CI the `compass-agent-image:build` task has already
// realised this closure, so the build here is a nix cache hit.

import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { $ } from "bun";
import { findForbiddenEnv } from "./env-check.ts";

// The platform the consumer's frozen record pins (compass-native #1073,
// linux/amd64 for the dogfood milestone). A drift here is the cheapest tripwire
// for a platform-contract regression.
const EXPECTED_OS = "linux";
const EXPECTED_ARCH = "amd64";

interface ImageConfig {
	readonly Os?: string;
	readonly Architecture?: string;
	readonly Env?: readonly string[];
}

function fail(message: string): never {
	console.error(`agent-image-env-gate: FAIL — ${message}`);
	process.exit(1);
}

// The repo's agent-image/ directory: three levels up from this file
// (tools/agent-image-env-gate/index.ts -> repo root) then into agent-image/.
const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const agentImageDir = join(repoRoot, "agent-image");

// Build the image spec exactly as the publish lane does. devenv tracing goes to
// stderr; the spec store path is the last stdout line.
const buildOut =
	await $`nix run path:../forks/devenv#devenv -- container build agent`
		.cwd(agentImageDir)
		.text();
const spec = buildOut.trimEnd().split("\n").at(-1)?.trim() ?? "";
if (!spec.startsWith("/nix/store/")) {
	fail(`image build did not print a store path (got: ${JSON.stringify(spec)})`);
}

// Inspect the built spec through the fork's patched skopeo (understands the
// `nix:` transport). The config form (not --raw) exposes Os/Architecture/Env.
const inspectOut =
	await $`nix run path:../forks/nix2container#skopeo-nix2container -- inspect nix:${spec}`
		.cwd(agentImageDir)
		.text();

let config: ImageConfig;
try {
	config = JSON.parse(inspectOut) as ImageConfig;
} catch (cause) {
	fail(`skopeo inspect did not return JSON: ${String(cause)}`);
}

const problems: string[] = [];

if (config.Os !== EXPECTED_OS) {
	problems.push(`image Os is ${config.Os ?? "unset"}, expected ${EXPECTED_OS}`);
}
if (config.Architecture !== EXPECTED_ARCH) {
	problems.push(
		`image Architecture is ${config.Architecture ?? "unset"}, expected ${EXPECTED_ARCH}`,
	);
}

const forbidden = findForbiddenEnv(config.Env ?? [], {
	builderHome: process.env.HOME,
});
for (const f of forbidden) {
	problems.push(`config.Env carries ${f.entry} — ${f.reason}`);
}

if (problems.length > 0) {
	fail(
		`the built image is not publish-safe:\n${problems.map((p) => `  - ${p}`).join("\n")}`,
	);
}

console.log(
	`agent-image-env-gate: OK — ${EXPECTED_OS}/${EXPECTED_ARCH}, no forbidden env keys (${(config.Env ?? []).length} entries checked)`,
);
