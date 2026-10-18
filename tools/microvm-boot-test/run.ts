#!/usr/bin/env bun

// Drive the tagged local microVM suite using CI's guest-image and VMM contract.
// Parsing stays in the pure core so this entry point can be tested offline.

import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostPlatform, kernelFileName } from "../runner-image/build-core.ts";
import { guestImageEnv, parseOutPaths, prependBins } from "./run-core.ts";

// This file is tools/microvm-boot-test/run.ts, so `../..` is the workspace root.
const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

/** Run `nix build --no-link --print-out-paths -f <file> <attrs...>` and return
 * the realised store paths in argument order. Exits the process on failure so a
 * broken build is a named, fail-closed abort, never a silent empty env. */
function nixBuild(file: string, attrs: readonly string[]): string[] {
	const result = spawnSync(
		"nix",
		["build", "--no-link", "--print-out-paths", "-f", file, ...attrs],
		{
			cwd: workspaceRoot,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "inherit"],
		},
	);
	if (result.status !== 0) {
		console.error(
			`nix build -f ${file} ${attrs.join(" ")} failed (exit ${result.status})`,
		);
		process.exit(result.status ?? 1);
	}
	const paths = parseOutPaths(result.stdout);
	if (paths.length !== attrs.length) {
		console.error(
			`nix build -f ${file} produced ${paths.length} out-paths, expected ${attrs.length} (${attrs.join(", ")})`,
		);
		process.exit(1);
	}
	return paths;
}

// Realise the three guest-image artifacts and the three VMM-stack binaries, in
// the SAME attr order ci.yml relies on.
const guest = nixBuild("guest-image/default.nix", [
	"compass-guest-kernel",
	"compass-guest-rootfs",
	"compass-guest-initrd",
]);
const vmm = nixBuild("tools/toolchain/microvm-vmm-env.nix", [
	"cloud-hypervisor",
	"virtiofsd",
	"passt",
]);
const platform = hostPlatform(process.arch);
const kernelDir = guest[0];
const kernelPath =
	kernelDir === undefined ? "" : `${kernelDir}/${kernelFileName(platform)}`;
if (guest[0] === undefined || !existsSync(kernelPath)) {
	console.error(
		`microvm-boot-test: guest kernel file is missing: ${kernelPath}`,
	);
	process.exit(1);
}
if (process.argv.slice(2).includes("--build-only")) process.exit(0);

const env: NodeJS.ProcessEnv = {
	...process.env,
	...guestImageEnv(guest, platform),
	PATH: prependBins(vmm, process.env.PATH ?? ""),
	// The -race lane needs cgo.
	CGO_ENABLED: "1",
};

// Exec the tagged suite, inheriting stdio so the go test output (incl. the boot
// latency + PSS t.Logf lines and any serial-console tail) streams live. cwd=go
// so go resolves the module.
const test = spawnSync(
	"go",
	[
		"test",
		"-tags",
		"microvm",
		"-race",
		"-v",
		"-timeout",
		"15m",
		"./internal/runtime/microvm/...",
	],
	{ cwd: join(workspaceRoot, "go"), env, stdio: "inherit" },
);
process.exit(test.status ?? 1);
