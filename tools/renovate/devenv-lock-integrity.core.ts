// Pure core of devenv-lock-integrity.ts: parse each devenv lock's locked github
// nodes, pick which to verify, and judge prefetch results. No I/O.

export interface LockedGithubNode {
	readonly node: string;
	readonly owner: string;
	readonly repo: string;
	readonly rev: string;
	readonly narHash: string;
	readonly lastModified: number;
}

export interface Prefetched {
	readonly narHash: string;
	readonly lastModified: number;
}

export interface IntegrityReport {
	readonly ok: boolean;
	readonly report: string;
}

export interface IntegrityCheck {
	readonly lock: string;
	readonly node: LockedGithubNode;
	readonly observed: Prefetched | { readonly error: string };
}

type LockedMap = Map<string, Record<string, unknown>>;

function lockedObjects(lockText: string): LockedMap {
	let parsed: unknown;
	try {
		parsed = JSON.parse(lockText);
	} catch (e) {
		throw new Error(`devenv lock is not valid JSON: ${(e as Error).message}`);
	}
	const nodes = (parsed as { nodes?: unknown }).nodes;
	if (typeof nodes !== "object" || nodes === null || Array.isArray(nodes)) {
		throw new Error("devenv lock has no `nodes` object");
	}
	// A Map, so a node named `__proto__` is kept rather than swallowed.
	const out: LockedMap = new Map();
	for (const [name, value] of Object.entries(nodes)) {
		const locked = (value as { locked?: unknown }).locked;
		if (locked === undefined) continue;
		if (typeof locked !== "object" || locked === null) {
			throw new Error(`node "${name}": \`locked\` is not an object`);
		}
		out.set(name, locked as Record<string, unknown>);
	}
	return out;
}

function stringField(
	name: string,
	locked: Record<string, unknown>,
	field: string,
): string {
	const v = locked[field];
	if (typeof v !== "string" || v === "") {
		throw new Error(
			`node "${name}": locked.${field} is missing or not a string`,
		);
	}
	return v;
}

/**
 * Every node with `locked`. Throws naming the node on an ill-formed field or on
 * a non-github type, so an unverifiable node fails instead of being skipped.
 */
export function lockedGithubNodes(
	lockText: string,
): readonly LockedGithubNode[] {
	return [...lockedObjects(lockText)].map(([name, locked]) => {
		if (locked.type !== "github") {
			throw new Error(
				`node "${name}": locked.type ${JSON.stringify(locked.type)} is not "github"; extend lock-integrity to verify it`,
			);
		}
		const rev = stringField(name, locked, "rev");
		if (!/^[0-9a-f]{40}$/.test(rev)) {
			throw new Error(
				`node "${name}": locked.rev ${JSON.stringify(rev)} is not a 40-hex commit`,
			);
		}
		const lastModified = locked.lastModified;
		if (typeof lastModified !== "number" || !Number.isInteger(lastModified)) {
			throw new Error(
				`node "${name}": locked.lastModified is missing or not an integer`,
			);
		}
		return {
			node: name,
			owner: stringField(name, locked, "owner"),
			repo: stringField(name, locked, "repo"),
			rev,
			narHash: stringField(name, locked, "narHash"),
			lastModified,
		};
	});
}

function canonical(v: unknown): string {
	if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
	if (typeof v === "object" && v !== null) {
		const entries = Object.entries(v).sort(([a], [b]) =>
			a < b ? -1 : a > b ? 1 : 0,
		);
		return `{${entries.map(([k, x]) => `${JSON.stringify(k)}:${canonical(x)}`).join(",")}}`;
	}
	return JSON.stringify(v);
}

/** Nodes whose `locked` object is new or not deep-equal to base's; a null base returns all. */
export function changedNodeNames(
	baseText: string | null,
	headText: string,
): ReadonlySet<string> {
	const head = lockedObjects(headText);
	if (baseText === null) return new Set(head.keys());
	const base = lockedObjects(baseText);
	return new Set(
		[...head]
			.filter(([n, locked]) => {
				const was = base.get(n);
				return was === undefined || canonical(was) !== canonical(locked);
			})
			.map(([n]) => n),
	);
}

/** `github:<owner>/<repo>/<rev>`: the dedupe key and the prefetch argument. */
export function prefetchRef(n: LockedGithubNode): string {
	return `github:${n.owner}/${n.repo}/${n.rev}`;
}

/** Parses `nix flake prefetch --json` stdout; throws on a missing field. */
export function parsePrefetch(stdout: string): Prefetched {
	const parsed = JSON.parse(stdout) as {
		hash?: unknown;
		locked?: { lastModified?: unknown };
	};
	if (typeof parsed.hash !== "string" || parsed.hash === "") {
		throw new Error("nix flake prefetch output has no `hash`");
	}
	const lastModified = parsed.locked?.lastModified;
	if (typeof lastModified !== "number") {
		throw new Error("nix flake prefetch output has no `locked.lastModified`");
	}
	return { narHash: parsed.hash, lastModified };
}

/** True only for HTTP 429, HTTP 5xx or a rate-limit message; never for a hash mismatch. */
export function isTransientFetchError(stderr: string): boolean {
	return /HTTP error (429|5\d\d)\b/.test(stderr) || /rate limit/i.test(stderr);
}

/** One line per node; `ok` only when nothing mismatched or went unverified. */
export function integrityReport(
	checks: readonly IntegrityCheck[],
): IntegrityReport {
	let ok = true;
	const lines = checks.map(({ lock, node, observed }) => {
		const where = `${lock} ${node.node} (${prefetchRef(node)})`;
		if ("error" in observed) {
			ok = false;
			return `FAIL ${where}: unverified: ${observed.error}`;
		}
		const bad: string[] = [];
		if (observed.narHash !== node.narHash) {
			bad.push(`narHash expected ${observed.narHash}, got ${node.narHash}`);
		}
		if (observed.lastModified !== node.lastModified) {
			bad.push(
				`lastModified expected ${observed.lastModified}, got ${node.lastModified}`,
			);
		}
		if (bad.length === 0) return `ok   ${where}`;
		ok = false;
		return `FAIL ${where}: ${bad.join("; ")}`;
	});
	return { ok, report: lines.join("\n") };
}
