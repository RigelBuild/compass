import { describe, expect, test } from "bun:test";
import { assertCliArtifact, type CliCheckInput } from "./cli-check";

const healthy: CliCheckInput = {
	binaryExists: true,
	binaryExecutable: true,
	binaryMtimeMs: 20,
	newestSourceMtimeMs: 10,
	runExitCode: 0,
};

describe("assertCliArtifact", () => {
	test("passes a current executable that runs", () => {
		expect(assertCliArtifact(healthy)).toEqual({ ok: true });
	});
	test("names a missing binary as a task that never ran", () => {
		expect(assertCliArtifact({ ...healthy, binaryExists: false })).toEqual({
			ok: false,
			message: expect.stringContaining("task never ran"),
		});
	});
	test("names a non-executable binary", () => {
		expect(assertCliArtifact({ ...healthy, binaryExecutable: false })).toEqual({
			ok: false,
			message: expect.stringContaining("not executable"),
		});
	});
	test("names a stale binary", () => {
		expect(assertCliArtifact({ ...healthy, binaryMtimeMs: 10 })).toEqual({
			ok: false,
			message: expect.stringContaining("stale"),
		});
	});
	test("names a failed --help run", () => {
		expect(assertCliArtifact({ ...healthy, runExitCode: 2 })).toEqual({
			ok: false,
			message: expect.stringContaining("exit 2"),
		});
	});
});
