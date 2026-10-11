#!/usr/bin/env bun
// Realise only the agent image's node_modules FOD for this host's system, so a stale
// outputHash fails in seconds with the fix named, not at the end of the image build.
import { join } from "node:path";
import { $ } from "bun";
import { FOD_ENTRIES, parseGotForFragment } from "./refresh-fod-hashes.ts";

const entry = FOD_ENTRIES.find(
	(e) => e.id === "agent-node-modules-agent-image-pkgs",
);
if (!entry) {
	console.error("check-agent-fod: no agent node_modules entry in FOD_ENTRIES");
	process.exit(1);
}

const vehicle = join(import.meta.dir, "..", "..", entry.buildFile);
const res =
	await $`nix build -f ${vehicle} ${entry.buildTarget}.nodeModules --no-link`
		.nothrow()
		.quiet();
if (res.exitCode === 0) {
	console.log(
		`check-agent-fod: ${entry.file} outputHash matches for ${process.arch}.`,
	);
	process.exit(0);
}

const out = `${res.stdout.toString()}\n${res.stderr.toString()}`;
const got = parseGotForFragment(out, entry.drvFragment);
if (got) {
	console.error(
		`check-agent-fod: ${entry.file} outputHash is stale for the current ` +
			`${entry.triggers.join(" / ")}. The installed node_modules now hashes to ${got}.\n` +
			"Run `bun tools/renovate/refresh-fod-hashes.ts` and commit the result.",
	);
} else {
	console.error(
		`check-agent-fod: building ${entry.buildTarget}.nodeModules failed without a hash mismatch:\n` +
			out.split("\n").slice(-30).join("\n"),
	);
}
process.exit(1);
