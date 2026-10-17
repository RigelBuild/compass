import { describe, expect, test } from "bun:test";
import { assertSuiteResults, parsePackageList } from "./suite-assertions.ts";

describe("parsePackageList", () => {
	test("normalizes selected Go package arguments", () => {
		expect(parsePackageList("./server ./internal/store")).toEqual([
			"server",
			"internal/store",
		]);
	});

	test("rejects an empty or non-root package argument", () => {
		expect(parsePackageList(undefined)).toEqual([]);
		expect(() => parsePackageList("./...")).toThrow(
			"invalid Go package argument",
		);
		expect(() => parsePackageList("../outside")).toThrow(
			"invalid Go package argument",
		);
		expect(() => parsePackageList("./server ./server")).toThrow("duplicates");
	});
});

describe("assertSuiteResults", () => {
	const base = {
		suite: "pgtest" as const,
		modulePath: "github.com/RigelBuild/compass/go",
		selectedPackages: ["server", "internal/store"],
		callerPackages: ["server", "internal/store", "internal/usage"],
		skipMessage: "no database",
		testOutput:
			"ok  github.com/RigelBuild/compass/go/server 1.0s\nok  github.com/RigelBuild/compass/go/internal/store 2.0s\n",
	};

	test("requires ok from each selected caller and ignores unselected callers", () => {
		expect(assertSuiteResults(base)).toEqual({
			checkedPackages: ["internal/store", "server"],
			failures: [],
		});
	});

	test("fails when selection is empty", () => {
		const result = assertSuiteResults({ ...base, selectedPackages: [] });
		expect(result.failures).toContain("pgtest: selected package list is empty");
	});

	test("fails when no package calls the harness", () => {
		const result = assertSuiteResults({ ...base, callerPackages: [] });
		expect(result.failures).toContain("pgtest: no package calls the harness");
	});

	test("fails when a selected caller did not report ok", () => {
		const result = assertSuiteResults({
			...base,
			testOutput: "ok  github.com/RigelBuild/compass/go/server 1.0s\n",
		});
		expect(result.failures).toContain(
			"pgtest: selected package 'internal/store' did not report ok",
		);
	});

	test("keeps the harness skip-text check", () => {
		const result = assertSuiteResults({
			...base,
			testOutput: "no database\n",
		});
		expect(result.failures).toContain("pgtest: test harness reported a skip");
	});
});
