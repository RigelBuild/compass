import { describe, expect, test } from "bun:test";
import {
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { dockerfileViolations, entrypointTarget } from "./build-core.ts";

// The image build is not a CI task, so this is what catches Dockerfile drift.
describe("server-image Dockerfile contract", () => {
	test("the committed Dockerfile runs the staged entrypoint as nonroot", () => {
		const path = join(
			import.meta.dir,
			"..",
			"..",
			"server-image",
			"Dockerfile",
		);
		expect(dockerfileViolations(readFileSync(path, "utf8"))).toEqual([]);
	});

	test("rejects a wrong entrypoint and a root or overridden user", () => {
		expect(
			dockerfileViolations(
				'USER 0\nENTRYPOINT ["/nix/store/x/bin/compass-server"]\n',
			),
		).toEqual([
			'missing ENTRYPOINT ["/nix/store/.compass-server"]',
			"last USER must be 65532:65532",
		]);
		expect(
			dockerfileViolations(
				'USER 65532:65532\nUSER root\nENTRYPOINT ["/nix/store/.compass-server"]\n',
			),
		).toEqual(["last USER must be 65532:65532"]);
	});
});

describe("server image entrypoint mapping", () => {
	test("maps the entrypoint to the staged binary by a relative path", () => {
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
