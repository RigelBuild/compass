#!/usr/bin/env bun
// Finds the newest first-parent commit at or before HEAD whose immutable
// `:git-<sha12>` agent image is published, so main's e2e can diff that commit's
// tree against HEAD and seed from an image that matches it. Prints the full sha,
// or nothing when no published commit is within reach (the caller then builds).

const REGISTRY = "https://ghcr.io";
const REPOSITORY = "rigelbuild/compass-agent";
const INDEX_ACCEPT =
	"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json";

/** Whether `:git-<sha12>` exists. Throws on any answer but present or absent. */
export type TagProbe = (sha12: string) => Promise<boolean>;

/** The first commit (newest first) whose image tag is published, or "". */
export async function findPublishedBase(
	commits: string[],
	probe: TagProbe,
): Promise<string> {
	for (const commit of commits) {
		if (await probe(commit.slice(0, 12))) return commit;
	}
	return "";
}

/** Map a manifest HEAD response to present/absent; anything else is ambiguous. */
export function classifyManifestStatus(status: number, sha12: string): boolean {
	if (status === 200) return true;
	if (status === 404) return false;
	throw new Error(
		`ambiguous registry answer ${status} for :git-${sha12}; refusing to guess the seed base`,
	);
}

async function anonymousToken(): Promise<string> {
	const res = await fetch(
		`${REGISTRY}/token?scope=repository:${REPOSITORY}:pull`,
	);
	if (!res.ok) throw new Error(`ghcr token request failed: ${res.status}`);
	const body = (await res.json()) as { token?: string };
	if (!body.token) throw new Error("ghcr token response carried no token");
	return body.token;
}

if (import.meta.main) {
	const limit = Number(process.argv[2] ?? "200");
	const log = Bun.spawnSync([
		"git",
		"rev-list",
		"--first-parent",
		`--max-count=${limit}`,
		"HEAD",
	]);
	if (log.exitCode !== 0) {
		throw new Error(`git rev-list failed: ${log.stderr.toString()}`);
	}
	const token = await anonymousToken();
	const probe: TagProbe = async (sha12) => {
		const res = await fetch(
			`${REGISTRY}/v2/${REPOSITORY}/manifests/git-${sha12}`,
			{
				method: "HEAD",
				headers: { Authorization: `Bearer ${token}`, Accept: INDEX_ACCEPT },
			},
		);
		return classifyManifestStatus(res.status, sha12);
	};
	const commits = log.stdout.toString().split("\n").filter(Boolean);
	console.log(await findPublishedBase(commits, probe));
}
