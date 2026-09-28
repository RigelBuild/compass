import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { compileAgentBinary } from "./compile";
import { collectLegacyPiModuleEntries } from "./legacy-pi-modules-plugin";

describe("collectLegacyPiModuleEntries", () => {
	test("includes installed coding-agent root and exported module keys", async () => {
		const entries = await collectLegacyPiModuleEntries();
		const keys = entries.map((entry) => entry.key);

		expect(keys.length).toBeGreaterThan(0);
		expect(keys).toContain("@oh-my-pi/pi-coding-agent");
	});
});

describe("compiled legacy Pi plugin", () => {
	const sourceDir = mkdtempSync(path.join(tmpdir(), "compass-legacy-pi-src-"));
	const outputDir = mkdtempSync(path.join(tmpdir(), "compass-legacy-pi-bin-"));
	afterAll(() => {
		rmSync(sourceDir, { recursive: true, force: true });
		rmSync(outputDir, { recursive: true, force: true });
	});

	test("loads an extension importing the coding-agent root", async () => {
		const extensionPath = path.join(outputDir, "extension.ts");
		const entrypoint = path.join(sourceDir, "extension-smoke.ts");
		const outfile = path.join(outputDir, "extension-smoke");
		const sdkManifest = Bun.resolveSync(
			"@oh-my-pi/pi-coding-agent/package.json",
			path.resolve(import.meta.dir, ".."),
		);
		const sdkRoot = path.dirname(sdkManifest);
		const nativeAddon = Bun.resolveSync(
			`@oh-my-pi/pi-natives-${process.platform}-${process.arch}`,
			sdkRoot,
		);
		writeFileSync(
			extensionPath,
			'import { getPackageDir } from "@oh-my-pi/pi-coding-agent";\nexport default () => { if (typeof getPackageDir !== "function") throw new Error("unexpected SDK export"); };\n',
		);
		writeFileSync(
			entrypoint,
			'import { loadExtensions } from "@oh-my-pi/pi-coding-agent";\nconst extensionPath = process.env.EXTENSION_PATH;\nif (!extensionPath) throw new Error("EXTENSION_PATH is required");\nconst result = await loadExtensions([extensionPath], process.cwd());\nif (result.errors.length || result.extensions.length !== 1) throw new Error(JSON.stringify(result.errors));\nconsole.log("legacy-extension:loaded");\n',
		);

		await compileAgentBinary({ entrypoint, outfile });
		const addonFilename = path.basename(nativeAddon);
		await Bun.write(path.join(outputDir, addonFilename), Bun.file(nativeAddon));
		const child = Bun.spawn([outfile], {
			cwd: process.cwd(),
			env: {
				...Bun.env,
				EXTENSION_PATH: extensionPath,
				PI_COMPILED: "true",
				PI_NATIVE_VARIANT: "baseline",
			},
			stdout: "pipe",
			stderr: "pipe",
		});
		const [stdout, stderr, exitCode] = await Promise.all([
			new Response(child.stdout).text(),
			new Response(child.stderr).text(),
			child.exited,
		]);
		expect(exitCode, `${stderr}\n${stdout}`).toBe(0);
		expect(stderr).toBe("");
		expect(stdout.trim()).toBe("legacy-extension:loaded");
	});
});
