import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import {
	chmodSync,
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const headSha = "c".repeat(40);
const cli = join(import.meta.dir, "nix-package-diff.ts");
let sandbox = "";
let binDir = "";
let patchBodyFile = "";
let callsFile = "";
let artifactFile = "";

beforeEach(() => {
	sandbox = mkdtempSync(join(tmpdir(), "nix-package-diff-test-"));
	binDir = join(sandbox, "bin");
	patchBodyFile = join(sandbox, "patched.md");
	callsFile = join(sandbox, "calls.jsonl");
	mkdirSync(binDir);
	writeFileSync(callsFile, "");
	artifactFile = join(sandbox, "diff.json");
	writeFileSync(
		join(binDir, "gh"),
		`#!/usr/bin/env bun
		import { appendFileSync, readFileSync, writeFileSync } from "node:fs";
		const args = process.argv.slice(2);
		appendFileSync(process.env.GH_CALLS_FILE, JSON.stringify(args) + "\\n");
		if (args.includes("PATCH")) {
			const field = args.find((arg) => arg.startsWith("body=@"));
			if (!field) process.exit(2);
			const body = readFileSync(field.slice("body=@".length), "utf8");
			writeFileSync(process.env.GH_PATCH_BODY, body);
			process.exit(0);
		}
		const body = process.env.GH_BODY ?? "Renovate body";
		console.log(JSON.stringify({
		  state: "open",
		  head: {
		    ref: "renovate/example",
		    repo: { full_name: "RigelBuild/compass" },
		    sha: "${"c".repeat(40)}",
		  },
		  body,
		}));
		`,
	);
	chmodSync(join(binDir, "gh"), 0o755);
	writeFileSync(
		artifactFile,
		`${JSON.stringify({ headSha, skip: false, results: [] })}\n`,
	);
});

afterEach(() => {
	rmSync(sandbox, { recursive: true, force: true });
});

function runPublish(body = "Renovate body") {
	return spawnSync(process.execPath, [cli, "publish"], {
		encoding: "utf8",
		env: {
			...process.env,
			PATH: `${binDir}:${process.env.PATH}`,
			GH_CALLS_FILE: callsFile,
			GH_BODY: body,
			GH_PATCH_BODY: patchBodyFile,
			GH_REPO: "RigelBuild/compass",
			PR_NUMBERS: "[123]",
			RUN_HEAD_SHA: headSha,
			RUN_URL: "https://github.com/RigelBuild/compass/actions/runs/1",
			ARTIFACT_FILE: artifactFile,
		},
	});
}

describe("nix package diff publisher", () => {
	test("does not fetch the body again or patch when there are no nix targets", () => {
		const result = runPublish();
		expect(result.status, result.stderr).toBe(0);
		const calls = readFileSync(callsFile, "utf8")
			.trim()
			.split("\n")
			.map((line) => JSON.parse(line) as string[]);
		expect(calls).toEqual([["api", "repos/RigelBuild/compass/pulls/123"]]);
		expect(calls.some((args) => args.includes("PATCH"))).toBe(false);
	});
	test("includes a non-empty Renovate prefix within the body limit", () => {
		const changes = Array.from({ length: 30 }, (_, index) => ({
			name: `package-${index}`,
			from: ["1.0"],
			to: ["2.0"],
		}));
		writeFileSync(
			artifactFile,
			`${JSON.stringify({
				headSha,
				skip: false,
				results: [{ label: "dev shell", direct: [], transitive: changes }],
			})}\n`,
		);
		const body = `Renovate body ${"x".repeat(65_000)}`;
		const result = runPublish(body);
		expect(result.status, result.stderr).toBe(0);
		const calls = readFileSync(callsFile, "utf8")
			.trim()
			.split("\n")
			.map((line) => JSON.parse(line) as string[]);
		const patch = calls.find((args) => args.includes("PATCH"));
		expect(patch).toBeDefined();
		const patchedBody = readFileSync(patchBodyFile, "utf8");
		expect(patchedBody.length).toBeLessThanOrEqual(65_536);
		expect(patchedBody).toStartWith(body);
	});
});
