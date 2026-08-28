// Pure resolution for the devenv-CLI source tool (RIG-2546). No I/O, no process
// exec — everything here is a total function over strings, so the load-bearing
// half (lock JSON → validated coordinates → flakeref; argv → parsed request) is
// unit-testable (core.test.ts) and the executable shell (index.ts) stays thin.
// This mirrors the tools/toolchain/parity.ts / parity-core.ts split, and the
// lock-parse posture of tools/renovate/refresh-devenv-nixpkgs.core.ts:25 — a
// shape change must fail the caller loudly, never resolve a stale/wrong source.

/** The devenv node's locked coordinates, as a nix flakeref fragment. */
export interface DevenvSource {
	readonly owner: string;
	readonly repo: string;
	readonly rev: string; // 40-hex, validated
}

/**
 * Parse `.nodes.devenv.locked` out of a devenv.lock's text. Throws loudly on
 * missing node, missing/short rev, or non-github type — a shape change must
 * fail the caller, never resolve a stale or wrong source (the same posture as
 * refresh-devenv-nixpkgs.core.ts's innerNixpkgsRev).
 *
 * The `dir` field some locks carry (e.g. the root lock's `src/modules`) is
 * deliberately IGNORED: the `#devenv` flake attribute is what the flakeref
 * selects, not a source subdir, so DevenvSource carries only owner/repo/rev.
 */
export function devenvSource(lockText: string): DevenvSource {
	let lock: unknown;
	try {
		lock = JSON.parse(lockText);
	} catch (error) {
		throw new Error(`devenv-cli: devenv.lock is not valid JSON: ${error}`);
	}
	// Narrow with `in`/`typeof` at each level so every access is actually
	// checked (devenv.lock is external-boundary data; no schema validator is in
	// the repo). A shape change surfaces as a loud throw, never a silent read.
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let locked: Record<string, unknown> | undefined;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const node = lock.nodes.devenv;
		if (isObj(node) && "locked" in node && isObj(node.locked)) {
			locked = node.locked;
		}
	}
	if (locked === undefined) {
		throw new Error(
			"devenv-cli: could not read the devenv node from devenv.lock " +
				"(nodes.devenv.locked absent) — devenv lock shape may have changed.",
		);
	}
	const { type, owner, repo, rev } = locked;
	if (type !== "github") {
		throw new Error(
			`devenv-cli: devenv node type is ${JSON.stringify(type)}, expected "github".`,
		);
	}
	if (typeof owner !== "string" || owner === "") {
		throw new Error("devenv-cli: devenv node has no owner in devenv.lock.");
	}
	if (!/^[A-Za-z0-9-]+$/.test(owner)) {
		throw new Error(
			"devenv-cli: devenv node owner is not a bare github owner " +
				"(nodes.devenv.locked.owner) — devenv lock shape may be malformed.",
		);
	}
	if (typeof repo !== "string" || repo === "") {
		throw new Error("devenv-cli: devenv node has no repo in devenv.lock.");
	}
	if (!/^[A-Za-z0-9._-]+$/.test(repo)) {
		throw new Error(
			"devenv-cli: devenv node repo is not a bare github repo " +
				"(nodes.devenv.locked.repo) — devenv lock shape may be malformed.",
		);
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			"devenv-cli: could not read a 40-hex devenv rev from devenv.lock " +
				"(nodes.devenv.locked.rev) — devenv lock shape may have changed.",
		);
	}
	return { owner, repo, rev };
}

/** `github:<owner>/<repo>/<rev>#devenv` for the parsed node. */
export function flakeref(src: DevenvSource): string {
	return `github:${src.owner}/${src.repo}/${src.rev}#devenv`;
}

/** What the caller wants printed. */
export type Mode = "flakeref" | "bin-dir";

export interface Request {
	readonly lockPath: string; // e.g. "devenv.lock" | "agent-image/devenv.lock"
	readonly mode: Mode;
}

const MODES: readonly Mode[] = ["flakeref", "bin-dir"];

function isMode(value: string): value is Mode {
	return (MODES as readonly string[]).includes(value);
}

/**
 * Parse argv (`--lock <path> --mode <flakeref|bin-dir>`, either order); throws
 * on an unknown flag, a missing flag value, a missing required flag, or an
 * invalid mode. Fail loud rather than defaulting — a mistyped invocation must
 * not silently resolve the wrong lock or mode.
 */
export function parseArgs(argv: readonly string[]): Request {
	let lockPath: string | undefined;
	let mode: Mode | undefined;
	for (let i = 0; i < argv.length; i++) {
		const flag = argv[i];
		if (flag === "--lock" || flag === "--mode") {
			const value = argv[i + 1];
			if (value === undefined || value.startsWith("--")) {
				throw new Error(`devenv-cli: ${flag} requires a value.`);
			}
			i++;
			if (flag === "--lock") {
				lockPath = value;
			} else if (isMode(value)) {
				mode = value;
			} else {
				throw new Error(
					`devenv-cli: invalid --mode ${JSON.stringify(value)}, expected one of ${MODES.join(", ")}.`,
				);
			}
			continue;
		}
		throw new Error(`devenv-cli: unknown argument ${JSON.stringify(flag)}.`);
	}
	if (lockPath === undefined) {
		throw new Error("devenv-cli: --lock <path> is required.");
	}
	if (mode === undefined) {
		throw new Error("devenv-cli: --mode <flakeref|bin-dir> is required.");
	}
	return { lockPath, mode };
}

/** One symlink to create in the bin-dir shim: `link` (a name) → `target`. */
export interface ShimLink {
	readonly link: string;
	readonly target: string;
}

/**
 * The single-binary shim plan for a `nix build` out-path: exactly one symlink
 * named `devenv` pointing at `<outPath>/bin/devenv`. Extracted as a pure helper
 * so the load-bearing RD-3 invariant — the printed dir exposes ONE binary, not
 * devenv's whole closure bin dir, so appending it to $GITHUB_PATH cannot shadow
 * the parity-pinned toolchain — is unit-checked without a nix build.
 */
export function shimPlan(outPath: string): readonly ShimLink[] {
	return [{ link: "devenv", target: `${outPath}/bin/devenv` }];
}
