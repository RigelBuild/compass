import { describe, expect, test } from "bun:test";
import { imageLaneConfig } from "./image-lane-core.ts";

describe("imageLaneConfig", () => {
	test("maps runner and server to their paths, projects and closure vars", () => {
		expect(imageLaneConfig("runner")).toEqual({
			lane: "runner",
			imageDir: "runner-image",
			projectName: "compass-runner-image",
			closureEnv: "RUNNER_IMAGE_CLOSURE_PATHS",
		});
		expect(imageLaneConfig("server")).toEqual({
			lane: "server",
			imageDir: "server-image",
			projectName: "compass-server-image",
			closureEnv: "SERVER_IMAGE_CLOSURE_PATHS",
		});
	});

	test("rejects missing and unknown lane names", () => {
		expect(imageLaneConfig(undefined)).toBeUndefined();
		expect(imageLaneConfig("guest")).toBeUndefined();
	});
});
