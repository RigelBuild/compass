import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	changedNodeNames,
	integrityReport,
	isTransientFetchError,
	type LockedGithubNode,
	lockedGithubNodes,
	parsePrefetch,
	prefetchRef,
} from "./devenv-lock-integrity.core.ts";
import { DEVENV_LOCK_PATHS } from "./refresh-devenv-lock.core.ts";

const repoRoot = join(import.meta.dir, "..", "..");
const REV = "a".repeat(40);

function lock(nodes: Record<string, unknown>): string {
	return JSON.stringify({
		nodes: { root: { inputs: {} }, ...nodes },
		root: "root",
		version: 7,
	});
}

function githubLocked(over: Record<string, unknown> = {}) {
	return {
		locked: {
			lastModified: 1700000000,
			narHash: "sha256-AAAA",
			owner: "RigelBuild",
			repo: "devenv",
			rev: REV,
			type: "github",
			...over,
		},
	};
}

const node: LockedGithubNode = {
	node: "devenv",
	owner: "RigelBuild",
	repo: "devenv",
	rev: REV,
	narHash: "sha256-AAAA",
	lastModified: 1700000000,
};

describe("lockedGithubNodes", () => {
	// Real-manifest guard: a lock-format change fails here, not as a silent skip in CI.
	test.each([...DEVENV_LOCK_PATHS])(
		"parses every locked node of the real %s",
		(path) => {
			const text = readFileSync(join(repoRoot, path), "utf8");
			const nodes = lockedGithubNodes(text);
			const expected = Object.entries(JSON.parse(text).nodes)
				.filter(([, v]) => (v as { locked?: unknown }).locked !== undefined)
				.map(([k]) => k);
			expect(nodes.map((n) => n.node).sort()).toEqual(expected.sort());
			expect(nodes.some((n) => n.node === "root")).toBe(false);
		},
	);

	test("throws naming the node for a non-github type", () => {
		const text = lock({ local: githubLocked({ type: "path" }) });
		expect(() => lockedGithubNodes(text)).toThrow(
			/local.*"path".*extend lock-integrity/,
		);
	});

	test("throws naming the node when narHash is missing", () => {
		const text = lock({ devenv: githubLocked({ narHash: undefined }) });
		expect(() => lockedGithubNodes(text)).toThrow(/devenv.*narHash/);
	});

	test("throws when rev is not 40-hex", () => {
		const text = lock({ devenv: githubLocked({ rev: "main" }) });
		expect(() => lockedGithubNodes(text)).toThrow(/devenv.*rev/);
	});

	test("throws on invalid JSON", () => {
		expect(() => lockedGithubNodes("{nope")).toThrow(/not valid JSON/);
	});

	test("a nodes array fails instead of checking nothing", () => {
		expect(() => lockedGithubNodes('{"nodes":[],"version":7}')).toThrow(
			/no `nodes` object/,
		);
	});

	// Raw JSON: an object literal would itself swallow the `__proto__` key.
	test("keeps a node named __proto__", () => {
		const text = `{"nodes":{"__proto__":${JSON.stringify(githubLocked())}},"version":7}`;
		expect(lockedGithubNodes(text).map((n) => n.node)).toEqual(["__proto__"]);
		expect([...changedNodeNames(null, text)]).toEqual(["__proto__"]);
	});
});

describe("changedNodeNames", () => {
	const base = lock({
		devenv: githubLocked(),
		nixpkgs: githubLocked({ repo: "nixpkgs" }),
	});

	test("identical locks change nothing", () => {
		expect(changedNodeNames(base, base).size).toBe(0);
	});

	test("a rev-only change is included", () => {
		const head = lock({
			devenv: githubLocked({ rev: "b".repeat(40) }),
			nixpkgs: githubLocked({ repo: "nixpkgs" }),
		});
		expect([...changedNodeNames(base, head)]).toEqual(["devenv"]);
	});

	test("a narHash-only change is included", () => {
		const head = lock({
			devenv: githubLocked(),
			nixpkgs: githubLocked({ repo: "nixpkgs", narHash: "sha256-BBBB" }),
		});
		expect([...changedNodeNames(base, head)]).toEqual(["nixpkgs"]);
	});

	test("a lastModified-only change is included", () => {
		const head = lock({
			devenv: githubLocked({ lastModified: 1700000001 }),
			nixpkgs: githubLocked({ repo: "nixpkgs" }),
		});
		expect([...changedNodeNames(base, head)]).toEqual(["devenv"]);
	});

	test("key order alone is not a change", () => {
		const reordered = lock({
			nixpkgs: githubLocked({ repo: "nixpkgs" }),
			devenv: {
				locked: Object.fromEntries(
					Object.entries(githubLocked().locked).reverse(),
				),
			},
		});
		expect(changedNodeNames(base, reordered).size).toBe(0);
	});

	test("a new node is included", () => {
		const head = lock({
			devenv: githubLocked(),
			nixpkgs: githubLocked({ repo: "nixpkgs" }),
			hk: githubLocked({ repo: "hk" }),
		});
		expect([...changedNodeNames(base, head)]).toEqual(["hk"]);
	});

	test("a null base returns every locked node", () => {
		expect([...changedNodeNames(null, base)].sort()).toEqual([
			"devenv",
			"nixpkgs",
		]);
	});
});

describe("prefetchRef", () => {
	test("is the github flake ref at the locked rev", () => {
		expect(prefetchRef(node)).toBe(`github:RigelBuild/devenv/${REV}`);
	});
});

describe("parsePrefetch", () => {
	test("reads hash and locked.lastModified", () => {
		const out = JSON.stringify({
			hash: "sha256-X",
			locked: { lastModified: 42 },
		});
		expect(parsePrefetch(out)).toEqual({
			narHash: "sha256-X",
			lastModified: 42,
		});
	});

	test("rejects output with no hash", () => {
		expect(() =>
			parsePrefetch(JSON.stringify({ locked: { lastModified: 42 } })),
		).toThrow(/hash/);
	});

	test("rejects output with no lastModified", () => {
		expect(() =>
			parsePrefetch(JSON.stringify({ hash: "sha256-X", locked: {} })),
		).toThrow(/lastModified/);
	});
});

describe("isTransientFetchError", () => {
	test.each([
		"HTTP error 429",
		"HTTP error 503 (curl error: …)",
		"API rate limit exceeded",
	])("%s is transient", (stderr) =>
		expect(isTransientFetchError(stderr)).toBe(true),
	);
	test.each([
		"error: NAR hash mismatch in input",
		"HTTP error 401",
		"HTTP error 404",
		"HTTP error 4290 bytes",
	])("%s is not transient", (stderr) =>
		expect(isTransientFetchError(stderr)).toBe(false),
	);
});

describe("integrityReport", () => {
	test("all consistent is ok", () => {
		const r = integrityReport([
			{
				lock: "devenv.lock",
				node,
				observed: { narHash: "sha256-AAAA", lastModified: 1700000000 },
			},
		]);
		expect(r.ok).toBe(true);
	});

	test("a wrong narHash fails naming lock, node and both values", () => {
		const r = integrityReport([
			{
				lock: "devenv.lock",
				node,
				observed: { narHash: "sha256-BBBB", lastModified: 1700000000 },
			},
		]);
		expect(r.ok).toBe(false);
		expect(r.report).toMatch(
			/devenv\.lock.*devenv.*narHash.*sha256-BBBB.*sha256-AAAA/,
		);
	});

	test("a wrong lastModified alone fails", () => {
		const r = integrityReport([
			{
				lock: "devenv.lock",
				node,
				observed: { narHash: "sha256-AAAA", lastModified: 1 },
			},
		]);
		expect(r.ok).toBe(false);
		expect(r.report).toMatch(/lastModified.*expected 1.*got 1700000000/);
	});

	test("an unverified node fails", () => {
		const r = integrityReport([
			{ lock: "devenv.lock", node, observed: { error: "HTTP error 404" } },
		]);
		expect(r.ok).toBe(false);
		expect(r.report).toMatch(
			/devenv\.lock.*devenv.*unverified.*HTTP error 404/,
		);
	});

	test("no checks is ok", () => {
		expect(integrityReport([]).ok).toBe(true);
	});
});
