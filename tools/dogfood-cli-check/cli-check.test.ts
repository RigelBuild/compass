import { describe, expect, test } from "bun:test";
import {
	assertCliArtifact,
	type CliCheckInput,
	parseListedInputs,
} from "./cli-check";

const ROOT = "/repo";

describe("parseListedInputs", () => {
	test("includes embedded assets, which no .go glob would see", () => {
		expect(
			parseListedInputs(
				`${ROOT}/go/store/store.go\n${ROOT}/go/store/mig/1.sql\n`,
				ROOT,
			),
		).toEqual([`${ROOT}/go/store/store.go`, `${ROOT}/go/store/mig/1.sql`]);
	});
	test("keeps a path containing spaces whole", () => {
		expect(parseListedInputs(`${ROOT}/go/a/my asset.txt\n`, ROOT)).toEqual([
			`${ROOT}/go/a/my asset.txt`,
		]);
	});
	test("skips out-of-repo deps, pinned instead by go.mod/go.sum", () => {
		expect(parseListedInputs(`/nix/store/go/src/fmt/fmt.go\n`, ROOT)).toEqual(
			[],
		);
	});
	test("ignores the trailing blank record", () => {
		expect(parseListedInputs(`${ROOT}/go/a.go\n\n`, ROOT)).toEqual([
			`${ROOT}/go/a.go`,
		]);
	});
});

const healthy: CliCheckInput = {
	binaryExists: true,
	binaryExecutable: true,
	binaryMtimeMs: 20,
	newestSourceMtimeMs: 10,
	sourceError: null,
	run: { kind: "exit", code: 0 },
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
	test("names an empty source set", () => {
		expect(
			assertCliArtifact({ ...healthy, newestSourceMtimeMs: null }),
		).toEqual({
			ok: false,
			message: expect.stringContaining("freshness cannot be checked"),
		});
	});
	test("surfaces the scan's own reason when it reports one", () => {
		expect(
			assertCliArtifact({
				...healthy,
				newestSourceMtimeMs: null,
				sourceError: "go list timed out resolving the build closure",
			}),
		).toEqual({
			ok: false,
			message: expect.stringContaining("go list timed out"),
		});
	});
	test("treats an unknown binary mtime as stale, never as current", () => {
		expect(assertCliArtifact({ ...healthy, binaryMtimeMs: null })).toEqual({
			ok: false,
			message: expect.stringContaining("stale"),
		});
	});
	test("names a signal death as a crash, not an exit code", () => {
		expect(
			assertCliArtifact({
				...healthy,
				run: { kind: "signal", signal: "SIGSEGV" },
			}),
		).toEqual({
			ok: false,
			message: expect.stringContaining("SIGSEGV"),
		});
	});
	test("names a failed --help run", () => {
		expect(
			assertCliArtifact({ ...healthy, run: { kind: "exit", code: 2 } }),
		).toEqual({
			ok: false,
			message: expect.stringContaining("exit 2"),
		});
	});
	test("names a timed out --help run", () => {
		expect(assertCliArtifact({ ...healthy, run: { kind: "timeout" } })).toEqual(
			{
				ok: false,
				message: expect.stringContaining("timed out"),
			},
		);
	});
	test("names a spawn failure", () => {
		expect(
			assertCliArtifact({ ...healthy, run: { kind: "spawn-failure" } }),
		).toEqual({
			ok: false,
			message: expect.stringContaining("corrupt or unrunnable"),
		});
	});
});
