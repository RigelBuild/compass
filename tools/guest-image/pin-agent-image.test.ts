import { afterEach, beforeEach, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import {
	chmodSync,
	mkdirSync,
	mkdtempSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { EXIT } from "./pin-core.ts";

const cli = join(import.meta.dir, "pin-agent-image.ts");

let sandbox = "";
let binDir = "";

beforeEach(() => {
	sandbox = mkdtempSync(join(tmpdir(), "guest-pin-"));
	binDir = join(sandbox, "bin");
	mkdirSync(binDir, { recursive: true });
	// Stands in for a runner whose default auth file is unreadable: skopeo
	// fails unless told to skip credential lookup.
	writeFileSync(
		join(binDir, "skopeo"),
		`#!/usr/bin/env bash
[ "$2" = "--no-creds" ] || { echo "reading auth.json: permission denied" >&2; exit 1; }
echo "skopeo called past the auth check" >&2
exit 1
`,
	);
	chmodSync(join(binDir, "skopeo"), 0o755);
});

afterEach(() => {
	rmSync(sandbox, { recursive: true, force: true });
});

test("registry reads skip credential lookup", () => {
	const result = spawnSync("bun", [cli, "--tag", "git-0123456789ab"], {
		encoding: "utf8",
		env: { ...process.env, PATH: `${binDir}:${process.env.PATH}` },
	});
	expect(result.status).toBe(EXIT.registryFailed);
	expect(result.stderr).toContain("skopeo called past the auth check");
	expect(result.stderr).not.toContain("permission denied");
});
