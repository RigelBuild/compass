import { afterAll, describe, expect, test } from "bun:test";
import { copyFileSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { createLegacyPiModulesPlugin } from "./legacy-pi-modules-plugin";

const tempDir = mkdtempSync(
	path.join(os.tmpdir(), "compass-legacy-pi-binary-"),
);
afterAll(() => rmSync(tempDir, { recursive: true, force: true }));

describe("compiled legacy Pi extension loading", () => {
	// biome-ignore lint/plugin: 130s is a slow-body budget, not a crash guard: Bun.build compile plus a cold start overran bun's 5s default on CI, and the build has no intermediate signal to event-gate on; the probe itself gates on proc.exited.
	test("loads a legacy-scope extension and keeps hashline grammar as text", async () => {
		const binaryPath = path.join(tempDir, "legacy-pi-probe");
		const fixturePath = path.join(
			import.meta.dir,
			"fixtures/legacy-pi-extension.ts",
		);
		const entryPath = path.join(
			import.meta.dir,
			"fixtures/legacy-pi-compiled-probe.ts",
		);
		const result = await Bun.build({
			entrypoints: [entryPath],
			plugins: [await createLegacyPiModulesPlugin()],
			compile: {
				outfile: binaryPath,
				autoloadBunfig: false,
				autoloadDotenv: false,
				autoloadTsconfig: false,
				autoloadPackageJson: false,
			},
		});
		expect(
			result.success,
			result.logs.map((log) => log.message).join("\n"),
		).toBe(true);

		const codingAgentDir = path.dirname(
			Bun.resolveSync(
				"@oh-my-pi/pi-coding-agent/package.json",
				import.meta.dir,
			),
		);
		const nativesDir = path.dirname(
			Bun.resolveSync(
				`@oh-my-pi/pi-natives-${process.platform}-${process.arch}/package.json`,
				codingAgentDir,
			),
		);
		for (const filename of readdirSync(nativesDir).filter((name) =>
			name.endsWith(".node"),
		)) {
			copyFileSync(
				path.join(nativesDir, filename),
				path.join(tempDir, filename),
			);
		}

		const child = Bun.spawn([binaryPath], {
			cwd: tempDir,
			env: {
				...process.env,
				HOME: tempDir,
				LEGACY_PI_EXTENSION_PATH: fixturePath,
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
		const resultLine = stdout.trim().split("\n").at(-1);
		if (!resultLine) throw new Error(`Probe produced no output: ${stderr}`);
		const output: unknown = JSON.parse(resultLine);
		expect(output).toMatchObject({ loaded: 1, errors: [] });
		if (
			typeof output !== "object" ||
			output === null ||
			!("hashlineGrammar" in output)
		) {
			throw new Error("Probe output has no hashlineGrammar");
		}
		const hashlineGrammar = output.hashlineGrammar;
		expect(typeof hashlineGrammar).toBe("string");
		expect(hashlineGrammar).toContain(
			"start: begin_patch file_patch+ end_patch",
		);
		expect(hashlineGrammar).not.toContain("$bunfs");
	}, 130_000);
});
