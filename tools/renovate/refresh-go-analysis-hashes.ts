#!/usr/bin/env bun
// Refresh the source and vendor FOD hashes coupled to the two pinned analysis tools.
// Renovate runs this after a pin update; every hash is recomputed from its Nix derivation.

import { readFileSync, writeFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import { $ } from "bun";
import {
	parseGotForFragment,
	rewriteInlineHash,
} from "./refresh-fod-hashes.ts";

export const PIN_FILE = "tools/toolchain/versions/go-analysis.nix";
export const BUILD_FILE = "tools/toolchain/gate-tools.nix";

export type ToolEntry = {
	tool: "nilaway" | "golangci-lint";
	attr: string;
	srcAttr: string;
	vendorAttr: string;
	derived: "tag-from-version" | "version-from-rev-date";
};

export const TOOL_ENTRIES: ToolEntry[] = [
	{
		tool: "nilaway",
		attr: "nilaway",
		srcAttr: "src",
		vendorAttr: "goModules",
		derived: "version-from-rev-date",
	},
	{
		tool: "golangci-lint",
		attr: "golangci-lint",
		srcAttr: "src",
		vendorAttr: "goModules",
		derived: "tag-from-version",
	},
];

const FAKE_SRI = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
const SOURCE_MARKER = 'hash = "sha256-';
const VENDOR_MARKER = 'vendorHash = "sha256-';

function errorMessage(error: unknown): string {
	return error instanceof Error ? error.message : String(error);
}

function fail(message: string): never {
	throw new Error(`renovate-go-analysis: ${message}`);
}

// Synchronous so a signal handler can never interleave with a half-done pin write.
function writePin(text: string): void {
	writeFileSync(PIN_FILE, text);
}

function toolBlock(fileText: string, entry: ToolEntry): string {
	const lines = fileText.split("\n");
	const start = lines.findIndex((line) =>
		line.trimStart().startsWith(`${entry.attr} = {`),
	);
	if (start === -1) fail(`attribute '${entry.attr}' not found in ${PIN_FILE}`);
	const end = lines.findIndex(
		(line, index) => index > start && line.trim() === "};",
	);
	if (end === -1)
		fail(`attribute '${entry.attr}' is not closed in ${PIN_FILE}`);
	return lines.slice(start, end + 1).join("\n");
}

function replaceToolBlock(
	fileText: string,
	entry: ToolEntry,
	block: string,
): string {
	const original = toolBlock(fileText, entry);
	const start = fileText.indexOf(original);
	if (start === -1)
		fail(`cannot splice attribute '${entry.attr}' in ${PIN_FILE}`);
	return `${fileText.slice(0, start)}${block}${fileText.slice(start + original.length)}`;
}

function fieldValue(block: string, field: string, entry: ToolEntry): string {
	const match = block.match(new RegExp(`^\\s*${field}\\s*=\\s*"([^"]+)"`, "m"));
	if (!match?.[1]) fail(`missing '${field}' in ${entry.attr} block`);
	return match[1];
}

function rewriteField(
	block: string,
	field: string,
	value: string,
	entry: ToolEntry,
): string {
	const pattern = new RegExp(
		`(^\\s*${field}\\s*=\\s*")[^"]*("\\s*;\\s*$)`,
		"m",
	);
	if (!pattern.test(block)) fail(`missing '${field}' in ${entry.attr} block`);
	return block.replace(
		pattern,
		(_line, before: string, after: string) => `${before}${value}${after}`,
	);
}

function rewriteBlockHash(
	block: string,
	marker: string,
	newSri: string,
	entry: ToolEntry,
): string {
	try {
		return rewriteInlineHash(block, marker, newSri, PIN_FILE);
	} catch (error) {
		fail(`${entry.attr}: ${errorMessage(error)}`);
	}
}

async function nilawayVersionDate(rev: string): Promise<string> {
	const fetchUrl =
		process.env.NILAWAY_FETCH_URL || "https://github.com/uber-go/nilaway";
	const tempDir = await mkdtemp(join(tmpdir(), "renovate-go-analysis-"));
	try {
		await $`git init -q`.cwd(tempDir).quiet();
		const fetched = await $`git fetch --depth=1 ${fetchUrl} ${rev}`
			.cwd(tempDir)
			.nothrow()
			.quiet();
		if (fetched.exitCode !== 0) {
			fail(
				`nilaway commit-date fetch failed for ${rev}: ${fetched.stderr.toString()}`,
			);
		}
		const logged =
			await $`git log -1 --date=format-local:%Y-%m-%d --format=%cd FETCH_HEAD`
				.cwd(tempDir)
				.env({ TZ: "UTC" })
				.nothrow()
				.quiet();
		const date = logged.stdout.toString().trim();
		if (logged.exitCode !== 0 || !/^\d{4}-\d{2}-\d{2}$/.test(date)) {
			fail(
				`nilaway commit-date lookup failed for ${rev}: ${logged.stderr.toString()}`,
			);
		}
		return date;
	} finally {
		await rm(tempDir, { recursive: true, force: true });
	}
}

async function resolveDrvPath(entry: ToolEntry, attr: string): Promise<string> {
	const expression = `analysis.${entry.tool}.${attr}.drvPath`;
	const result = await $`nix eval --raw -f ${BUILD_FILE} ${expression}`
		.nothrow()
		.quiet();
	if (result.exitCode !== 0) {
		fail(`could not resolve ${expression}: ${result.stderr.toString()}`);
	}
	const drvPath = result.stdout.toString().trim();
	if (!drvPath.endsWith(".drv"))
		fail(`invalid derivation path for ${expression}: ${drvPath}`);
	return basename(drvPath);
}

async function recomputeHash(
	entry: ToolEntry,
	currentText: string,
): Promise<string> {
	let currentBlock = toolBlock(currentText, entry);
	currentBlock = rewriteBlockHash(currentBlock, SOURCE_MARKER, FAKE_SRI, entry);
	currentBlock = rewriteBlockHash(currentBlock, VENDOR_MARKER, FAKE_SRI, entry);
	writePin(replaceToolBlock(currentText, entry, currentBlock));

	const sourceFragment = await resolveDrvPath(entry, entry.srcAttr);
	const sourceBuild =
		await $`nix build -f ${BUILD_FILE} analysis.${entry.tool} --no-link --keep-going`
			.nothrow()
			.quiet();
	const sourceOutput = `${sourceBuild.stdout.toString()}\n${sourceBuild.stderr.toString()}`;
	const sourceHash = parseGotForFragment(sourceOutput, sourceFragment);
	if (!sourceHash) {
		fail(
			`no 'got:' SRI for source derivation '${sourceFragment}' after building analysis.${entry.tool}: ${sourceOutput}`,
		);
	}

	currentBlock = rewriteBlockHash(
		currentBlock,
		SOURCE_MARKER,
		sourceHash,
		entry,
	);
	writePin(
		replaceToolBlock(readFileSync(PIN_FILE, "utf8"), entry, currentBlock),
	);

	const vendorFragment = await resolveDrvPath(entry, entry.vendorAttr);
	const vendorBuild =
		await $`nix build -f ${BUILD_FILE} analysis.${entry.tool} --no-link --keep-going`
			.nothrow()
			.quiet();
	const vendorOutput = `${vendorBuild.stdout.toString()}\n${vendorBuild.stderr.toString()}`;
	const vendorHash = parseGotForFragment(vendorOutput, vendorFragment);
	if (!vendorHash) {
		fail(
			`no 'got:' SRI for vendor derivation '${vendorFragment}' after building analysis.${entry.tool}: ${vendorOutput}`,
		);
	}

	currentBlock = rewriteBlockHash(
		currentBlock,
		VENDOR_MARKER,
		vendorHash,
		entry,
	);
	return replaceToolBlock(readFileSync(PIN_FILE, "utf8"), entry, currentBlock);
}

async function refreshTool(
	entry: ToolEntry,
	initialText: string,
): Promise<string> {
	let block = toolBlock(initialText, entry);
	if (entry.derived === "tag-from-version") {
		const version = fieldValue(block, "version", entry);
		block = rewriteField(block, "tag", `v${version}`, entry);
	} else {
		const rev = fieldValue(block, "rev", entry);
		if (!/^[a-f0-9]{40}$/.test(rev)) fail(`invalid nilaway rev '${rev}'`);
		const date = await nilawayVersionDate(rev);
		block = rewriteField(block, "version", `0-unstable-${date}`, entry);
	}
	let updatedText = replaceToolBlock(initialText, entry, block);
	writePin(updatedText);
	updatedText = await recomputeHash(entry, updatedText);
	writePin(updatedText);
	console.log(
		`renovate-go-analysis: refreshed ${entry.attr} source and vendor hashes`,
	);
	return updatedText;
}

async function main(): Promise<void> {
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	const baseBranch = process.env.RENOVATE_BASE_BRANCH || "main";
	let baseRef = baseBranch;
	if (
		(await $`git rev-parse --verify -q origin/${baseBranch}`.nothrow().quiet())
			.exitCode === 0
	) {
		baseRef = `origin/${baseBranch}`;
	}

	const gate = await $`git diff --quiet ${baseRef} -- ${PIN_FILE}`
		.nothrow()
		.quiet();
	if (gate.exitCode === 0) {
		console.log(
			`renovate-go-analysis: ${PIN_FILE} unchanged vs ${baseRef}; nothing to do.`,
		);
		return;
	}

	const initialText = await Bun.file(PIN_FILE).text();
	const baseResult = await $`git show ${baseRef}:${PIN_FILE}`.nothrow().quiet();
	if (baseResult.exitCode !== 0)
		fail(`could not read ${PIN_FILE} from ${baseRef}`);
	const baseText = baseResult.stdout.toString();
	let currentText = initialText;
	const onSignal = (signal: "SIGINT" | "SIGTERM") => {
		try {
			writePin(initialText);
			console.error(`renovate-go-analysis: interrupted by ${signal}`);
		} catch (error) {
			console.error(
				`renovate-go-analysis: interrupted by ${signal}; could not restore ${PIN_FILE}: ${errorMessage(error)}`,
			);
		}
		process.exit(1);
	};
	const onSigint = () => onSignal("SIGINT");
	const onSigterm = () => onSignal("SIGTERM");
	process.on("SIGINT", onSigint);
	process.on("SIGTERM", onSigterm);
	try {
		for (const entry of TOOL_ENTRIES) {
			if (toolBlock(initialText, entry) === toolBlock(baseText, entry))
				continue;
			currentText = await refreshTool(entry, currentText);
		}
	} catch (error) {
		writePin(initialText);
		const message = errorMessage(error);
		if (message.startsWith("renovate-go-analysis:")) throw error;
		fail(message);
	} finally {
		process.off("SIGINT", onSigint);
		process.off("SIGTERM", onSigterm);
	}
}

if (import.meta.main) {
	try {
		await main();
	} catch (error) {
		console.error(
			errorMessage(error).startsWith("renovate-go-analysis:")
				? errorMessage(error)
				: `renovate-go-analysis: ${errorMessage(error)}`,
		);
		process.exitCode = 1;
	}
}
