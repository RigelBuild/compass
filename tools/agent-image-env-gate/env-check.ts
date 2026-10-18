// Check serialized image env for store-path references and builder-home leaks.
// Store references add unintended closure contents; host paths break reproducibility.

export interface ForbiddenEnv {
	/** The offending `KEY=value` entry, verbatim. */
	readonly entry: string;
	/** The parsed key (substring before the first `=`). */
	readonly key: string;
	/** Why it is forbidden — surfaced in the gate's failure message. */
	readonly reason: string;
}

export interface EnvCheckOptions {
	/**
	 * The gate runner's own home directory (the build host's home). When set, any
	 * Env value that embeds this path is a build-host leak — the container's
	 * legitimate `HOME` is the container user's home, which never equals the
	 * builder's, so this cannot false-positive on the image's own `HOME`.
	 */
	readonly builderHome?: string;
}

const DEVENV_PREFIX = "DEVENV_";
const NIX_STORE_PREFIX = "/nix/store/";

/**
 * Return every forbidden entry in an image's OCI `config.Env`. Empty result =
 * the image env is clean. Pure: no I/O, deterministic in its inputs.
 *
 * Two independent invariants:
 *   1. No `DEVENV_*` key whose value NAMES an absolute `/nix/store` path —
 *      anywhere in the value, not only as the whole value, since a value can
 *      embed a store path mid-string (e.g. a JSON list of task commands). Every
 *      such path is a closure root nix2container drags whole into the image
 *      (`config.json` `deps = [configFile]`), the reproducibility/bloat defect
 *      the consumer neutralizes in agent-image/devenv.nix. A `DEVENV_*` with a
 *      value that names no store path (a container path like `/home/agent`,
 *      `/tmp/devenv`, or empty) expands no closure and is not flagged.
 *   2. No key (DEVENV_ or otherwise) whose value embeds the build host's home —
 *      a build-host path baked into the image, non-reproducible across hosts.
 */
export function findForbiddenEnv(
	env: readonly string[],
	opts: EnvCheckOptions = {},
): ForbiddenEnv[] {
	const forbidden: ForbiddenEnv[] = [];
	const builderHome = opts.builderHome?.trim();

	for (const entry of env) {
		const eq = entry.indexOf("=");
		const key = eq === -1 ? entry : entry.slice(0, eq);
		const value = eq === -1 ? "" : entry.slice(eq + 1);

		if (key.startsWith(DEVENV_PREFIX) && value.includes(NIX_STORE_PREFIX)) {
			forbidden.push({
				entry,
				key,
				reason: `devenv-internal key names an absolute ${NIX_STORE_PREFIX} path — a closure root nix2container drags into the image (neutralize it consumer-side in agent-image/devenv.nix, gated on isBuilding)`,
			});
			// Already forbidden by its root cause; don't also flag a builder path,
			// so each entry reports once.
			continue;
		}

		if (builderHome && value.includes(builderHome)) {
			forbidden.push({
				entry,
				key,
				reason: `build-host home path (${builderHome}) baked into the image — non-reproducible across build hosts`,
			});
		}
	}

	return forbidden;
}
