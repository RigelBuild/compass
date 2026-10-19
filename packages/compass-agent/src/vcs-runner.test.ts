import { describe, expect, test } from "bun:test";
import { runCommand } from "./vcs-runner";

describe("runCommand", () => {
	test("stops a command past its timeout and reports it", async () => {
		const started = Date.now();
		const result = await runCommand("sleep", ["30"], "/", { timeoutMs: 100 });
		expect(result.timedOut).toBe(true);
		expect(Date.now() - started).toBeLessThan(5_000);
	});

	test("a timeout also stops children that hold the output pipes", async () => {
		const started = Date.now();
		const result = await runCommand("sh", ["-c", "sleep 30; echo late"], "/", {
			timeoutMs: 100,
		});
		expect(result.timedOut).toBe(true);
		expect(Date.now() - started).toBeLessThan(5_000);
	});

	test("returns the exit code and output of a command that finishes in time", async () => {
		const result = await runCommand(
			"sh",
			["-c", "echo out; echo err >&2; exit 3"],
			"/",
			{
				timeoutMs: 5_000,
			},
		);
		expect(result).toEqual({ code: 3, stdout: "out\n", stderr: "err\n" });
	});
});
