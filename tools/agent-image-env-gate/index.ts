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
export const EXPECTED_OS = "linux";
export const EXPECTED_ARCH = "amd64";

export interface ImageConfig {
	readonly Os?: string;
	readonly Architecture?: string;
	readonly Env?: readonly string[];
}

function fail(message: string): never {
	console.error(`agent-image-env-gate: FAIL — ${message}`);
	process.exit(1);
}

// Pick the image spec store path from the build output. devenv tracing goes to
// stderr; the spec store path is the last stdout line. Pure: returns the raw
// last non-empty line (which the caller validates as a /nix/store/ path) so an
// empty or non-store-path input routes through the caller's fail(), not a throw
// here.
export function extractSpec(buildOut: string): string {
	return buildOut.trimEnd().split("\n").at(-1)?.trim() ?? "";
}

// Evaluate a built image's OCI config against the publish-safety contract,
// returning every problem found (empty = publish-safe). Pure: no I/O, no exit —
// so the caller decides how to surface the result.
export function evaluateImage(
	config: ImageConfig,
	opts: { builderHome?: string },
): string[] {
	const problems: string[] = [];

	if (config.Os !== EXPECTED_OS) {
		problems.push(
			`image Os is ${config.Os ?? "unset"}, expected ${EXPECTED_OS}`,
		);
	}
	if (config.Architecture !== EXPECTED_ARCH) {
		problems.push(
			`image Architecture is ${config.Architecture ?? "unset"}, expected ${EXPECTED_ARCH}`,
		);
	}

	// An absent or empty Env is itself a wrong-image signal: a real
	// compass-agent image always sets HOME/USER (containers.nix), so an empty
	// env means we inspected the wrong artifact or the config form regressed.
	if (!config.Env || config.Env.length === 0) {
		problems.push(
			"image config carries no Env — a real compass-agent image always sets HOME/USER (containers.nix), so an empty/absent Env is itself a wrong-image signal",
		);
	}

	const forbidden = findForbiddenEnv(config.Env ?? [], {
		builderHome: opts.builderHome,
	});
	for (const f of forbidden) {
		problems.push(`config.Env carries ${f.entry} — ${f.reason}`);
	}

	return problems;
}

if (import.meta.main) {
	// The repo's agent-image/ directory: three levels up from this file
	// (tools/agent-image-env-gate/index.ts -> repo root) then into agent-image/.
	const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
	const agentImageDir = join(repoRoot, "agent-image");

	// Build the image spec exactly as the publish lane does. devenv tracing goes
	// to stderr; the spec store path is the last stdout line.
	let buildOut: string;
	try {
		buildOut =
			await $`nix run path:../forks/devenv#devenv -- container build agent`
				.cwd(agentImageDir)
				.text();
	} catch (cause) {
		fail(`image build failed: ${String(cause)}`);
	}
	const spec = extractSpec(buildOut);
	if (!spec.startsWith("/nix/store/")) {
		fail(
			`image build did not print a store path (got: ${JSON.stringify(spec)})`,
		);
	}

	// Inspect the built spec through the fork's patched skopeo (understands the
	// `nix:` transport). The config form (not --raw) exposes Os/Architecture/Env.
	let inspectOut: string;
	try {
		inspectOut =
			await $`nix run path:../forks/nix2container#skopeo-nix2container -- inspect nix:${spec}`
				.cwd(agentImageDir)
				.text();
	} catch (cause) {
		fail(`skopeo inspect failed: ${String(cause)}`);
	}

	let config: ImageConfig;
	try {
		config = JSON.parse(inspectOut) as ImageConfig;
	} catch (cause) {
		fail(`skopeo inspect did not return JSON: ${String(cause)}`);
	}

	// The build host's own home; when unset the builder-home leak check can't
	// run, so make that degraded coverage visible in CI logs rather than
	// silently passing.
	const builderHome = process.env.HOME;
	if (!builderHome) {
		console.warn(
			"agent-image-env-gate: WARN — HOME unset; the build-host-home leak check is inactive this run",
		);
	}

	const problems = evaluateImage(config, { builderHome });

	if (problems.length > 0) {
		fail(
			`the built image is not publish-safe:\n${problems.map((p) => `  - ${p}`).join("\n")}`,
		);
	}

	console.log(
		`agent-image-env-gate: OK — ${EXPECTED_OS}/${EXPECTED_ARCH}, no forbidden env keys (${(config.Env ?? []).length} entries checked)`,
	);
}
