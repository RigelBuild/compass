#!/usr/bin/env bun

// Verify the built OCI image against publish-safety invariants: no store-backed DEVENV_ values, builder-home paths, or platform-contract mismatch.

import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { $ } from "bun";
import { devenvSource, flakeref } from "../toolchain/devenv-cli/core.ts";
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

// Return the raw final line so callers can report invalid output consistently.
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

	// Use the locked devenv revision so the gate builds the same derivation as CI.
	let ref: string;
	try {
		ref = flakeref(
			devenvSource(await Bun.file(join(agentImageDir, "devenv.lock")).text()),
		);
	} catch (cause) {
		fail(
			`could not resolve devenv source from agent-image/devenv.lock: ${String(cause)}`,
		);
	}
	let buildOut: string;
	try {
		buildOut = await $`nix run ${ref} -- container build agent`
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

	// Resolve the pinned nix2container-aware skopeo directly; adding it to the image
	// would include its closure in the published container.
	let skopeoBin: string;
	try {
		const out =
			await $`nix build --no-link --print-out-paths -f tools/toolchain/skopeo-nix2container-env.nix skopeo`
				.cwd(repoRoot)
				.text();
		// `--print-out-paths` prints every output of the derivation (skopeo ships a
		// separate `-man` output alongside the default). Pick the one that actually
		// carries `bin/skopeo` rather than trusting output order.
		const bin = out
			.trimEnd()
			.split("\n")
			.map((line) => line.trim())
			.filter((line) => line.startsWith("/nix/store/"))
			.map((store) => join(store, "bin", "skopeo"))
			.find((candidate) => existsSync(candidate));
		if (!bin) {
			fail(
				`skopeo helper produced no output carrying bin/skopeo (got: ${JSON.stringify(out)})`,
			);
		}
		skopeoBin = bin;
	} catch (cause) {
		fail(`skopeo resolution failed: ${String(cause)}`);
	}

	// The config form (not --raw) exposes Os/Architecture/Env.
	let inspectOut: string;
	try {
		inspectOut = await $`${skopeoBin} inspect nix:${spec}`
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
