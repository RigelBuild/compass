#!/usr/bin/env bun
// The toolchain version-parity gate: fail the build when the toolchain CI is
// running is not the toolchain the dev shell defines.
//
// This is the thin execution shell — read files, run probes, exit. All parsing
// and comparison, including the pass/fail decision, lives in ./parity-core.ts,
// which is pure and unit-tested (./parity-core.test.ts). The two halves of the
// toolchain and why they are checked differently are documented there.
//
// Run it anywhere: in CI as the first gate, or locally (`bun
// tools/toolchain/parity.ts`) where it should always pass, since locally the
// dev shell IS both sides of the comparison. That symmetry is deliberate — a
// gate you cannot run outside CI is a gate nobody debugs.
//
// Exit 0 = every pinned tool matched. Exit 1 = at least one mismatched OR could
// not be checked. Unverifiable is a failure, never a skip.

import { execFileSync } from "node:child_process";
import { readFileSync, realpathSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
	parseDevenvPackages,
	parseProtoTools,
	renderReport,
	type Verdict,
	verifySelfReport,
	verifyStorePath,
} from "./parity-core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

// How each .prototools runtime reports its version. Go is the odd one out —
// `go --version` is an error, it is `go version` — so the argv is explicit per
// tool rather than assumed. A pin appearing in .prototools with no entry here
// is NOT skipped: it falls through to an unverifiable verdict below, which
// fails the build and forces this table to be extended.
const PROTO_PROBES: Readonly<Record<string, readonly string[]>> = {
	bun: ["--version"],
	node: ["--version"],
	moon: ["--version"],
	go: ["version"],
};

/** Run a probe, returning its combined output, or null if the tool is absent. */
function probe(command: string, args: readonly string[]): string | null {
	try {
		return execFileSync(command, [...args], {
			encoding: "utf8",
			stdio: ["ignore", "pipe", "pipe"],
		});
	} catch {
		return null;
	}
}

/** `realpath` of what PATH resolves `command` to, or null if it is absent. */
function resolveOnPath(command: string): string | null {
	const which = probe("sh", ["-c", `command -v ${command}`]);
	if (which === null) return null;
	const path = which.trim();
	if (path === "") return null;
	try {
		return realpathSync(path);
	} catch {
		return null;
	}
}

/** One nixpkgs attribute's identity as the devenv.lock-pinned nixpkgs resolves it. */
interface NixIdentity {
	readonly version: string;
	readonly store: string;
	readonly bins: readonly string[];
}

/**
 * Resolve every dev-shell nixpkgs attribute to its derivation and command list.
 *
 * Evaluation only (`nix eval`) — no build. The expected identity is a property
 * of the pinned revision, so the gate can state it without realising a single
 * derivation, which keeps it fast and lets it run before the tools are fetched.
 */
function nixIdentities(attrs: readonly string[]): Record<string, NixIdentity> {
	const list = `[${attrs.map((a) => `"${a}"`).join(" ")}]`;
	const out = execFileSync(
		"nix",
		[
			"eval",
			"--json",
			"-f",
			join(repoRoot, "tools/toolchain/gate-tools.nix"),
			"identity",
			"--arg",
			"attrs",
			list,
		],
		{ encoding: "utf8" },
	);
	return JSON.parse(out) as Record<string, NixIdentity>;
}

const protoPins = parseProtoTools(
	readFileSync(join(repoRoot, ".prototools"), "utf8"),
);
const devenvAttrs = parseDevenvPackages(
	readFileSync(join(repoRoot, "devenv.nix"), "utf8"),
);

if (protoPins.length === 0 || devenvAttrs.length === 0) {
	// Either parse coming back empty means the file's shape moved out from under
	// the gate. Silently checking nothing is the exact false green this exists to
	// stop, so refuse rather than report a vacuous pass.
	console.error(
		`toolchain parity: parsed ${protoPins.length} .prototools pins and ${devenvAttrs.length} devenv.nix packages — ` +
			"one of those files no longer has the shape the gate parses. Refusing to report a pass over nothing.",
	);
	process.exit(1);
}

// `--print-nix-attrs`: emit the dev shell's nixpkgs attribute list as a nix
// list literal and stop. The workflow feeds it to gate-tools.nix to build the
// PATH it will then be checked against, so the tools CI installs and the tools
// the gate expects are derived from the one parse of devenv.nix — they cannot
// disagree, and adding a tool to the dev shell needs no workflow edit.
if (process.argv.includes("--print-nix-attrs")) {
	console.log(`[${devenvAttrs.map((a) => `"${a}"`).join(" ")}]`);
	process.exit(0);
}

const verdicts: Verdict[] = [];

// Half one: .prototools literals vs what the runtime on PATH reports.
for (const pin of protoPins) {
	const args = PROTO_PROBES[pin.tool];
	if (args === undefined) {
		verdicts.push({
			kind: "unverifiable",
			tool: pin.tool,
			reason:
				"no version probe defined — add one to PROTO_PROBES in tools/toolchain/parity.ts",
		});
		continue;
	}
	verdicts.push(verifySelfReport(pin.tool, pin.version, probe(pin.tool, args)));
}

// Half two: every command the dev shell's nixpkgs packages provide must resolve
// to the derivation the pinned nixpkgs builds. One verdict per COMMAND, not per
// attribute, because PATH resolves commands — an ambient /usr/bin/protoc winning
// over the pinned one is precisely the drift worth catching, and it is invisible
// if the check is at attribute granularity.
const identities = nixIdentities(devenvAttrs);
for (const attr of devenvAttrs) {
	const identity = identities[attr];
	if (identity === undefined) {
		verdicts.push({
			kind: "unverifiable",
			tool: attr,
			reason: "no such attribute in the pinned nixpkgs",
		});
		continue;
	}
	if (identity.bins.length === 0) {
		verdicts.push({
			kind: "unverifiable",
			tool: attr,
			reason: "derivation exposes no bin/, nothing to resolve",
		});
		continue;
	}
	for (const bin of identity.bins) {
		const label = bin === attr ? attr : `${attr}:${bin}`;
		verdicts.push(verifyStorePath(label, identity.store, resolveOnPath(bin)));
	}
}

const report = renderReport(verdicts);
console.log(
	"toolchain parity — CI toolchain vs the dev shell (.prototools + devenv.nix @ devenv.lock)\n",
);
console.log(report.table);
process.exit(report.ok ? 0 : 1);
