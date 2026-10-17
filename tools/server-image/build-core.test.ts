import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { entrypointTarget } from "./build-core.ts";

describe("server image entrypoint mapping", () => {
	test("resolves the entrypoint through the staged in-store relative symlink", () => {
		const server = "/nix/store/abc-compass-server";
		const stageDir = mkdtempSync(join(tmpdir(), "server-image-core-"));
		try {
			const binary = join(stageDir, "abc-compass-server", "bin");
			mkdirSync(binary, { recursive: true });
			writeFileSync(join(binary, "compass-server"), "binary");
			expect(entrypointTarget(server, stageDir)).toBe(
				"abc-compass-server/bin/compass-server",
			);
		} finally {
			rmSync(stageDir, { recursive: true, force: true });
		}
	});

	test("fails when the staged closure lacks bin/compass-server", () => {
		const stageDir = mkdtempSync(join(tmpdir(), "server-image-core-"));
		try {
			expect(() =>
				entrypointTarget("/nix/store/abc-compass-server", stageDir),
			).toThrow("closure lacks bin/compass-server");
		} finally {
			rmSync(stageDir, { recursive: true, force: true });
		}
	});
});
