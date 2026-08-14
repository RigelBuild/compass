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
	renderReport,
	type Verdict,
	verifyStorePath,
} from "./parity-core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

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

/**
 * Resolve the language toolchains (bun/node/moon/go) to their derivations and
 * command lists. Same identity shape as nixIdentities, from gate-tools.nix's
 * `langs` output — a closed set, so no `--arg attrs` is needed (the head there
 * defaults `attrs` to `[ ]`).
 */
function nixLangs(): Record<string, NixIdentity> {
	const out = execFileSync(
		"nix",
		[
			"eval",
			"--json",
			"-f",
			join(repoRoot, "tools/toolchain/gate-tools.nix"),
			"langs",
		],
		{ encoding: "utf8" },
	);
	return JSON.parse(out) as Record<string, NixIdentity>;
}

const devenvAttrs = parseDevenvPackages(
	readFileSync(join(repoRoot, "devenv.nix"), "utf8"),
);

// Resolve the closed language set up front so the refusal below can guard on it
// too: an empty `langs` identity set means gate-tools.nix's shape moved out
// from under the gate, the same false-green risk as an empty devenv parse.
const langs = nixLangs();

if (devenvAttrs.length === 0 || Object.keys(langs).length === 0) {
	// Either coming back empty means a source the gate parses moved out from
	// under it. Silently checking nothing is the exact false green this exists to
	// stop, so refuse rather than report a vacuous pass.
	console.error(
		`toolchain parity: parsed ${devenvAttrs.length} devenv.nix packages and ${Object.keys(langs).length} language toolchains — ` +
			"one of those sources no longer has the shape the gate parses. Refusing to report a pass over nothing.",
	);
	process.exit(1);
}

// `--print-nix-attrs`: emit the dev shell's nixpkgs attribute list as a nix
// list literal and stop. The workflow feeds it to gate-tools.nix to build the
// PATH it will then be checked against, so the tools CI installs and the tools
// the gate expects are derived from the one parse of devenv.nix — they cannot
// disagree, and adding a tool to the dev shell needs no workflow edit. The
// empty-source refusal above runs first on purpose: --print-nix-attrs stays a
// fail-fast path, refusing before it emits an attr list built over a langs set
// whose shape has moved out from under the gate.
if (process.argv.includes("--print-nix-attrs")) {
	console.log(`[${devenvAttrs.map((a) => `"${a}"`).join(" ")}]`);
	process.exit(0);
}

/**
 * One verdict per COMMAND a derivation provides, not per attribute, because
 * PATH resolves commands — an ambient /usr/bin/protoc winning over the pinned
 * one is precisely the drift worth catching, and it is invisible if the check
 * is at attribute granularity.
 */
function storePathVerdicts(
	name: string,
	identity: NixIdentity | undefined,
	notFoundReason: string,
): Verdict[] {
	if (identity === undefined) {
		return [{ kind: "unverifiable", tool: name, reason: notFoundReason }];
	}
	if (identity.bins.length === 0) {
		return [
			{
				kind: "unverifiable",
				tool: name,
				reason: "derivation exposes no bin/, nothing to resolve",
			},
		];
	}
	return identity.bins.map((bin) => {
		const label = bin === name ? name : `${name}:${bin}`;
		return verifyStorePath(label, identity.store, resolveOnPath(bin));
	});
}

const verdicts: Verdict[] = [];

// Half one: the language toolchains, whose derivations gate-tools.nix's `langs`
// output builds — the closed set the dev shell appends outside its parsed
// `packages` literal.
for (const [name, identity] of Object.entries(langs)) {
	verdicts.push(
		...storePathVerdicts(name, identity, "not built by gate-tools.nix langs"),
	);
}

// Half two: the nixpkgs attributes parsed out of devenv.nix's packages literal,
// each resolved to the derivation the devenv.lock-pinned nixpkgs builds.
const identities = nixIdentities(devenvAttrs);
for (const attr of devenvAttrs) {
	verdicts.push(
		...storePathVerdicts(
			attr,
			identities[attr],
			"no such attribute in the pinned nixpkgs",
		),
	);
}

const report = renderReport(verdicts);
console.log(
	"toolchain parity — CI toolchain vs the dev shell (devenv.nix packages + versions/*.nix @ devenv.lock)\n",
);
console.log(report.table);
process.exit(report.ok ? 0 : 1);
