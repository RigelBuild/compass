#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import { publicVerdict } from "./assert-public-core.ts";

function arg(name: string): string | undefined {
	const i = process.argv.indexOf(`--${name}`);
	return i === -1 ? undefined : process.argv[i + 1];
}

const ref = arg("ref");
if (!ref) {
	console.error(
		"usage: bun tools/runner-image/assert-public.ts --ref <repo:tag>",
	);
	process.exit(2);
}
const probe = spawnSync(
	"skopeo",
	["inspect", "--no-creds", "--raw", `docker://${ref}`],
	{
		encoding: "utf8",
	},
);
const verdict = publicVerdict(
	{
		exitCode: probe.status ?? 1,
		stdout: probe.stdout ?? "",
		stderr: probe.error ? String(probe.error) : (probe.stderr ?? ""),
	},
	ref,
);
if (!verdict.ok) {
	console.error(`::error::${verdict.message}`);
	process.exit(1);
}
console.log(`${ref} is anonymously readable`);
