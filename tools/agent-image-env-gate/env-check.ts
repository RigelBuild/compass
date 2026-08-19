// The pure core of the agent-image env gate: given the OCI `config.Env` of the
// built compass-agent image, decide whether any entry is a signal that the
// image was built wrong.
//
// WHY THIS GATE EXISTS. The vendored devenv fork strips every devenv-internal
// variable from the image at serialization —
// `imageEnv = filterAttrs (name: _: !hasPrefix "DEVENV_" name) config.env`
// (forks/devenv/src/modules/containers.nix). That filter is a sealed-added
// option the fork's own suite cannot cover (upstream has no such option), and
// three separate wrong-image defects have shipped through it, each reachable
// only by hand-inspecting the built image:
//   1. `DEVENV_ROOT=/home/<builder>/…` — the build host's home baked into the
//      image, making it non-reproducible across build hosts.
//   2. `DEVENV_ROOT=/env` — a path the image does not carry, so every container
//      start emitted mkdir/ln errors.
//   3. a stale `DEVENV_CONTAINER` corrupting an unrelated image's home.
// All three surface the same way: a `DEVENV_`-prefixed key survives into the
// final image `config.Env`. That is the invariant this gate asserts.

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

/**
 * Return every forbidden entry in an image's OCI `config.Env`. Empty result =
 * the image env is clean. Pure: no I/O, deterministic in its inputs.
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

		if (key.startsWith(DEVENV_PREFIX)) {
			forbidden.push({
				entry,
				key,
				reason: `devenv-internal key leaked past the imageEnv filter (containers.nix strips ${DEVENV_PREFIX}* — its survival means the filter regressed)`,
			});
			// A DEVENV_ key is already forbidden; don't also flag it for a builder
			// path, so each entry reports its root cause once.
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
