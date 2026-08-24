#!/usr/bin/env bun
// The LOCAL microVM boot-test lane (RIG-2591). The tagged microVM suite compiles
// only under `-tags microvm`, and the existing compass-go:test lane is `go test
// -race ./...` with NO tag, so it never builds these tests. This script is what
// the `compass-go:test-microvm` moon task runs to drive the tagged suite on a
// KVM-capable dev box: realise the guest image + the VMM stack from nix, export
// the same env vars the CI KVM leg does (ci.yml:385-406), and exec `go test
// -tags microvm`.
//
// TypeScript, not bash (rule://scripts-ts-over-bash + the no-bash-gate CI task):
// this has real logic — two nix builds whose out-paths must be parsed and mapped
// to distinct env vars, a PATH assembled from three store paths, and a fail-fast
// on any missing output — so the parsing/mapping core is pure and unit-tested
// (./run.test.ts) while this file is the thin I/O shell.
//
// It deliberately mirrors ci.yml's microVM step (the reviewed source of truth
// for the exact nix invocations); it does NOT replace it — CI runs the microVM
// leg its own way. This is the dev-box entry point.

import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
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

const env: NodeJS.ProcessEnv = {
	...process.env,
	...guestImageEnv(guest),
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
