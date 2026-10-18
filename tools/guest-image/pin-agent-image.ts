// Relock every platform descriptor: the rootfs requires a complete immutable
// image provenance record, not only a moved index digest.

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
	AGENT_REPO,
	checkMember,
	checkMemberConfig,
	EXIT,
	indexMembers,
	isBuildTag,
	lockFromIndex,
	locksEqual,
	PinError,
	type PinLock,
	type PinMember,
	type Platform,
	renderLock,
	validatePin,
} from "./pin-core.ts";

const LOCK_PATH = join(
	import.meta.dir,
	"..",
	"..",
	"guest-image",
	"agent-oci.lock",
);

/** Where the moving tag lives. Only ever used to DISCOVER an immutable tag;
 * never written into a lock. */
const DISCOVERY_TAG = "latest";

type Inspected = { digest: string; manifest: unknown };

async function skopeo(command: string, args: string[]): Promise<string> {
	const proc = Bun.spawn(["skopeo", command, "--no-creds", ...args], {
		stdout: "pipe",
		stderr: "pipe",
	});
	const [out, err, code] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	if (code !== 0) {
		throw new PinError(
			`skopeo ${command} ${args.join(" ")} failed (exit ${code}): ${err.trim()}`,
			EXIT.registryFailed,
		);
	}
	return out;
}

/** Read raw JSON and hash the exact bytes served by the registry. */
async function inspect(reference: string): Promise<Inspected> {
	const raw = await skopeo("inspect", ["--raw", `docker://${reference}`]);
	let manifest: unknown;
	try {
		manifest = JSON.parse(raw);
	} catch (err) {
		throw new PinError(
			`could not parse registry content for ${reference}: ${String(err)}`,
			EXIT.registryFailed,
		);
	}
	const digest = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
	return { digest, manifest };
}

/** Resolve the index, both member manifests and each config from the registry. */
async function resolveIndex(tag: string): Promise<PinLock> {
	const { digest, manifest: index } = await inspect(`${AGENT_REPO}:${tag}`);
	const members = indexMembers(index);
	const pins = {} as Record<Platform, PinMember>;
	for (const member of members) {
		const inspected = await inspect(`${AGENT_REPO}@${member.digest}`);
		pins[member.platform] = checkMember(
			member,
			inspected.digest,
			inspected.manifest,
		);
		const configRaw = await skopeo("inspect", [
			"--config",
			`docker://${AGENT_REPO}@${member.digest}`,
		]);
		let config: unknown;
		try {
			config = JSON.parse(configRaw);
		} catch (err) {
			throw new PinError(
				`could not parse ${member.platform} config: ${String(err)}`,
				EXIT.registryFailed,
			);
		}
		checkMemberConfig(member, config);
	}
	return lockFromIndex(AGENT_REPO, tag, digest, pins);
}

/** Find the immutable build tag whose raw index bytes equal the moving tag. */
async function discoverBuildTag(): Promise<string> {
	const target = await inspect(`${AGENT_REPO}:${DISCOVERY_TAG}`);
	const listed = await skopeo("list-tags", [`docker://${AGENT_REPO}`]);
	let parsed: unknown;
	try {
		parsed = JSON.parse(listed);
	} catch (err) {
		throw new PinError(
			`could not parse tag list: ${String(err)}`,
			EXIT.registryFailed,
		);
	}
	if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
		throw new PinError("registry returned no tag list", EXIT.registryFailed);
	}
	const tags = (parsed as Record<string, unknown>).Tags;
	if (!Array.isArray(tags) || !tags.every((tag) => typeof tag === "string")) {
		throw new PinError("registry returned no tag list", EXIT.registryFailed);
	}
	const candidates = tags.filter(isBuildTag).reverse();
	for (const tag of candidates) {
		if ((await inspect(`${AGENT_REPO}:${tag}`)).digest === target.digest)
			return tag;
	}
	throw new PinError(
		`no git-<sha12> tag resolves to the same index as :${DISCOVERY_TAG} (${target.digest}) across ${candidates.length} candidate tag(s); the publish lane requires the pair to share an index`,
		EXIT.digestMismatch,
	);
}

function readLock(): PinLock | undefined {
	let body: string;
	try {
		body = readFileSync(LOCK_PATH, "utf8");
	} catch {
		return undefined;
	}
	try {
		return validatePin(JSON.parse(body));
	} catch (err) {
		if (err instanceof PinError) throw err;
		throw new PinError(
			`${LOCK_PATH} is not valid JSON: ${String(err)}`,
			EXIT.badLock,
		);
	}
}

async function pin(tag: string): Promise<number> {
	const next = await resolveIndex(tag);
	const current = readLock();
	if (current && locksEqual(current, next)) {
		console.log(
			`agent-oci.lock already pins ${tag} (${next.digest}) — no change`,
		);
		return 0;
	}
	writeFileSync(LOCK_PATH, renderLock(next));
	console.log(
		`pinned ${AGENT_REPO}:${tag}\n  index: ${next.digest}\n  linux/amd64: ${next.platforms["linux/amd64"].manifest} (${next.platforms["linux/amd64"].layers.length} layers)\n  linux/arm64: ${next.platforms["linux/arm64"].manifest} (${next.platforms["linux/arm64"].layers.length} layers)`,
	);
	return 0;
}

function usage(): number {
	console.error(
		"usage: bun tools/guest-image/pin-agent-image.ts --tag git-<sha12>\n" +
			"       bun tools/guest-image/pin-agent-image.ts --relock",
	);
	return EXIT.usage;
}

async function main(): Promise<number> {
	const argv = process.argv.slice(2);
	if (argv.includes("--relock")) {
		if (argv.length !== 1) return usage();
		if (!readLock()) {
			console.log("no agent-oci.lock on this branch — nothing to relock");
			return 0;
		}
		return await pin(await discoverBuildTag());
	}
	const i = argv.indexOf("--tag");
	const tag = i === -1 ? undefined : argv[i + 1];
	if (tag === undefined || argv.length !== 2) return usage();
	return await pin(tag);
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof PinError) {
		console.error(`pin-agent-image: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
