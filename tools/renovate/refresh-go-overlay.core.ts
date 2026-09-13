// Pure decision/transform core for refresh-go-overlay.ts (RIG-3100): the
// go-overlay input's locked rev out of devenv.lock and the go version out of
// go.nix — unit-testable without a nix runner, network, or git tree. NO
// shell-outs, NO fs beyond text passed in.

/**
 * The concrete go-overlay rev the dev shell resolved, read from devenv.lock's
 * `go-overlay` node (`nodes.go-overlay.locked.rev`). This is the overlay rev
 * gate-tools.nix fetches (`lock.nodes.go-overlay.locked`) to resolve
 * `go-bin.versions.<goPin>` — so a `devenv update go-overlay` that advances it
 * is what makes a newly-released go version resolve. Parsed as JSON (devenv.lock
 * is JSON), throwing loudly if the node or a 40-hex rev is absent — a shape
 * change must fail the task, never silently eval against a stale (too-old) rev.
 */
export function goOverlayLockedRev(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(
			`refresh-go-overlay: devenv.lock is not valid JSON: ${String(error)}`,
		);
	}
	// Narrow with `in`/`typeof` at each level so every access is actually
	// checked (devenv.lock is external-boundary data; no schema validator is in
	// the repo). A shape change surfaces as the loud throw below, never a
	// silently-wrong read.
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let rev: unknown;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const node = lock.nodes["go-overlay"];
		if (isObj(node) && "locked" in node && isObj(node.locked)) {
			rev = node.locked.rev;
		}
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			"refresh-go-overlay: could not read a 40-hex go-overlay rev from devenv.lock " +
				"(nodes['go-overlay'].locked.rev) — devenv lock shape may have changed.",
		);
	}
	return rev;
}

// The go pin literal in go.nix, which single-sources the toolchain version
// (hashes come from go-overlay). The go customManager rewrites exactly this on a
// bump; the refresh task reads it back to know which version the new overlay rev
// must provide.
const GO_PIN_VERSION_RE = /version\s*=\s*"([^"]+)"/;

/**
 * The go version pinned in go.nix, read from the `version = "<ver>"` literal.
 * Returns the bare version string. Throws if the literal is absent or the value
 * is not a dotted version (fail loud — a missing pin must not silently no-op and
 * then validate the eval against `undefined`). Mirrors the raw-text read the
 * config.json5 go manager's matchString does, so both key off the same literal.
 */
export function goPinVersion(goNixText: string): string {
	const match = GO_PIN_VERSION_RE.exec(goNixText);
	const version = match?.[1];
	if (version === undefined || !/^\d+\.\d+/.test(version)) {
		throw new Error(
			"refresh-go-overlay: could not read a dotted go version from go.nix " +
				'(`version = "<ver>"`) — go pin shape may have changed.',
		);
	}
	return version;
}
