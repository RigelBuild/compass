import { describe, expect, test } from "bun:test";
import {
	EXPECTED_ARCH,
	EXPECTED_OS,
	evaluateImage,
	extractSpec,
} from "./index.ts";

describe("extractSpec", () => {
	test("picks the last non-empty line (devenv trace precedes the spec)", () => {
		expect(extractSpec("trace line\n/nix/store/abc-x")).toBe(
			"/nix/store/abc-x",
		);
	});

	test("trims a trailing newline", () => {
		expect(extractSpec("/nix/store/abc\n")).toBe("/nix/store/abc");
	});

	test("trims surrounding whitespace on the spec line", () => {
		expect(extractSpec("trace\n  /nix/store/abc  ")).toBe("/nix/store/abc");
	});

	test("empty input yields the empty string", () => {
		expect(extractSpec("")).toBe("");
	});

	test("whitespace-only input yields the empty string", () => {
		expect(extractSpec("   \n  ")).toBe("");
	});
});

describe("evaluateImage", () => {
	test("a clean image config has no problems", () => {
		expect(
			evaluateImage(
				{
					Os: EXPECTED_OS,
					Architecture: EXPECTED_ARCH,
					Env: ["HOME=/home/agent", "USER=agent"],
				},
				{ builderHome: "/home/mattw" },
			),
		).toEqual([]);
	});

	test("Os drift is a problem", () => {
		const problems = evaluateImage(
			{
				Os: "darwin",
				Architecture: EXPECTED_ARCH,
				Env: ["HOME=/home/agent"],
			},
			{ builderHome: "/home/mattw" },
		);
		expect(problems).toContain(`image Os is darwin, expected ${EXPECTED_OS}`);
	});

	test("Architecture drift is a problem", () => {
		const problems = evaluateImage(
			{
				Os: EXPECTED_OS,
				Architecture: "arm64",
				Env: ["HOME=/home/agent"],
			},
			{ builderHome: "/home/mattw" },
		);
		expect(problems).toContain(
			`image Architecture is arm64, expected ${EXPECTED_ARCH}`,
		);
	});

	test("a DEVENV_ store-path leak in Env is a problem", () => {
		const problems = evaluateImage(
			{
				Os: EXPECTED_OS,
				Architecture: EXPECTED_ARCH,
				Env: [
					"HOME=/home/agent",
					"DEVENV_PROFILE=/nix/store/xxx-devenv-profile",
				],
			},
			{ builderHome: "/home/mattw" },
		);
		expect(problems.some((p) => p.includes("config.Env carries"))).toBe(true);
	});

	test("a DEVENV_ var with a non-store container path is NOT a problem", () => {
		// The fork forces these off store paths during a build; they expand no
		// closure, so they must not trip the gate.
		expect(
			evaluateImage(
				{
					Os: EXPECTED_OS,
					Architecture: EXPECTED_ARCH,
					Env: [
						"HOME=/home/agent",
						"DEVENV_ROOT=/home/agent",
						"DEVENV_RUNTIME=/tmp/devenv",
					],
				},
				{ builderHome: "/home/mattw" },
			),
		).toEqual([]);
	});

	test("an absent Env is a wrong-image signal", () => {
		const problems = evaluateImage(
			{ Os: EXPECTED_OS, Architecture: EXPECTED_ARCH },
			{ builderHome: "/home/mattw" },
		);
		expect(problems.some((p) => p.includes("no Env"))).toBe(true);
	});

	test("an empty Env is the same wrong-image signal", () => {
		const problems = evaluateImage(
			{ Os: EXPECTED_OS, Architecture: EXPECTED_ARCH, Env: [] },
			{ builderHome: "/home/mattw" },
		);
		expect(problems.some((p) => p.includes("no Env"))).toBe(true);
	});

	test("an unset Os is reported as 'image Os is unset'", () => {
		const problems = evaluateImage(
			{ Architecture: EXPECTED_ARCH, Env: ["HOME=/home/agent"] },
			{ builderHome: "/home/mattw" },
		);
		expect(problems).toContain(`image Os is unset, expected ${EXPECTED_OS}`);
	});
});
