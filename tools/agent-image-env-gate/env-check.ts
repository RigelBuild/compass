// The pure core of the agent-image env gate: given the OCI `config.Env` of the
// built compass-agent image, decide whether any entry is a signal that the
// image was built wrong.
//
// WHY THIS GATE EXISTS. devenv merges its own internal `DEVENV_*` variables
// into the container's serialized `config.Env` (top-level.nix sets
// `env.DEVENV_PROFILE/STATE/RUNTIME/DOTFILE/ROOT`, tasks.nix sets
// `env.DEVENV_TASK_FILE`). The RigelBuild/devenv fork does NOT blanket-strip
// them — a reusable container module applies the minimal fix, not a namespace
// wipe (RIG-2404). It forces the dev-shell-only paths off store/phantom values
// during a container build (`containers.nix`: `devenv.root`/`dotfile`/`runtime`
// → the container's own home and `/tmp`), so the live risk is narrower than
// "any DEVENV_ key present". The one class that actually corrupts the image is
// a `DEVENV_*` whose VALUE names an absolute `/nix/store` path (anywhere in
// the value): nix2container makes `config.json` a closure root
// (`deps = [configFile]`), so every store path
// NAMED in the env drags its whole closure into the image's content layers and
// the initialized nix DB — non-reproducible bloat and phantom DB entries. Two
// such vars exist (`DEVENV_PROFILE`, a 266-path dev profile; `DEVENV_TASK_FILE`,
// the tasks.json), each neutralized consumer-side in agent-image/devenv.nix
// (mkForce to a non-store placeholder, gated on isBuilding, as the internal monorepo ships it).
// This gate is the regression backstop for that
// neutralization, plus a build-host-home leak check that catches a builder path
// baked into ANY key (a non-reproducible-across-hosts defect). Both invariants
// are set operations on the built image's env, not an eyeball.

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
