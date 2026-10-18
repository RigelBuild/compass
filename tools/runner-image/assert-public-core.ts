import type { ProbeResult } from "./retag-core.ts";

function packageName(ref: string): string {
	const withoutTransport = ref.startsWith("docker://")
		? ref.slice("docker://".length)
		: ref;
	const lastSlash = withoutTransport.lastIndexOf("/");
	const lastColon = withoutTransport.lastIndexOf(":");
	return lastColon > lastSlash
		? withoutTransport.slice(0, lastColon)
		: withoutTransport;
}

export function publicVerdict(
	result: ProbeResult,
	ref: string,
): { ok: true } | { ok: false; message: string } {
	if (result.exitCode === 0) return { ok: true };
	const name = packageName(ref);
	const detail = result.stderr.trim() || `skopeo exited ${result.exitCode}`;
	const [org, ...rest] = name.replace(/^ghcr\.io\//, "").split("/");
	return {
		ok: false,
		message: `${name} is not anonymously readable (${detail}). Set package visibility to public: https://github.com/orgs/${org}/packages/container/${rest.join("/")}/settings`,
	};
}
