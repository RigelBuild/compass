import { describe, expect, test } from "bun:test";
import {
	CLOSURE_TARGETS,
	diffVersions,
	MAX_ARTIFACT_BYTES,
	parseArtifact,
	parseDevenvBuild,
	parseDevenvEval,
	parsePathInfo,
	parsePathInfoVersions,
	REVIEWABLE_MARKER,
	renderSection,
	sectionHeadSha,
	splitDrvName,
	targetsFor,
	upsertSection,
} from "./nix-package-diff.core.ts";

const HASH = "0123456789abcdfghijklmnpqrsvwxyz";
const HEAD_SHA = "c".repeat(40);

const mkPath = (storeName: string) => `/nix/store/${HASH}-${storeName}`;

const artifactJson = (
	changes = [{ name: "hello", from: ["1.0"], to: ["2.0"] }],
) =>
	JSON.stringify({
		headSha: HEAD_SHA,
		skip: false,
		results: [
			{
				label: "dev shell",
				direct: changes,
				transitive: changes,
			},
		],
	});

describe("nix package diff targets", () => {
	test("maps each lock and trigger file to its target", () => {
		expect(CLOSURE_TARGETS.map((target) => target.label)).toEqual([
			"dev shell",
			"agent image",
		]);
		expect(targetsFor(["devenv.lock"]).map((target) => target.label)).toEqual([
			"dev shell",
		]);
		expect(
			targetsFor(["agent-image/devenv.lock"]).map((target) => target.label),
		).toEqual(["agent image"]);
		expect(
			targetsFor(["tools/toolchain/versions/go.nix"]).map(
				(target) => target.label,
			),
		).toEqual(["dev shell"]);
		expect(
			targetsFor(["tools/toolchain/versions/bun.nix"]).map(
				(target) => target.label,
			),
		).toEqual(["dev shell", "agent image"]);
		expect(targetsFor(["tools/toolchain/versions/node.nix.bak"])).toEqual([]);
	});
});

describe("nix package name parsing", () => {
	test.each([
		[
			mkPath("protoc-gen-go-1.36.12"),
			{ name: "protoc-gen-go", version: "1.36.12" },
		],
		[mkPath("hello-2.12.3-bin"), { name: "hello", version: "2.12.3" }],
		[mkPath("jq-1.8.2"), { name: "jq", version: "1.8.2" }],
	])("splits %s", (storeName, expected) => {
		expect(splitDrvName(storeName)).toEqual(expected);
	});

	test("parses JSON eval output and raw path-info keys", () => {
		expect(
			parseDevenvEval('["hello-2.12.3","jq-1.8.2"]', "packageDiff.direct"),
		).toEqual(["hello-2.12.3", "jq-1.8.2"]);
		const pathInfo = JSON.stringify({
			[mkPath("hello-2.12.3")]: { narSize: 123 },
			[mkPath("jq-1.8.2")]: { narSize: 456 },
		});
		expect(parsePathInfo(pathInfo).sort()).toEqual([
			mkPath("hello-2.12.3"),
			mkPath("jq-1.8.2"),
		]);
	});
	test("reads the store path for an attribute from devenv build JSON", () => {
		const output = JSON.stringify(
			{ "packageDiff.closure": mkPath("nix-package-diff.json") },
			null,
			2,
		);
		expect(parseDevenvBuild(output, "packageDiff.closure")).toBe(
			mkPath("nix-package-diff.json"),
		);
		expect(() =>
			parseDevenvBuild(mkPath("nix-package-diff.json"), "packageDiff.closure"),
		).toThrow("not valid JSON");
		expect(() =>
			parseDevenvBuild('{"other": "/nix/store/x"}', "packageDiff.closure"),
		).toThrow("must map packageDiff.closure");
	});
	test("diffs path-info store keys with hash prefixes as package versions", () => {
		const base = parsePathInfoVersions(
			JSON.stringify({ [mkPath("hello-1.0")]: { narSize: 123 } }),
		);
		const head = parsePathInfoVersions(
			JSON.stringify({ [mkPath("hello-2.0")]: { narSize: 456 } }),
		);
		expect(diffVersions(base, head)).toEqual([
			{ name: "hello", from: ["1.0"], to: ["2.0"] },
		]);
	});
});

describe("package version diff", () => {
	test("reports only changed version sets and keeps multiple outputs", () => {
		expect(
			diffVersions(
				[
					{ name: "chromium", version: "150" },
					{ name: "protoc-gen-go", version: "1.36.11" },
					{ name: "steady", version: "3.0" },
				],
				[
					{ name: "chromium", version: "153" },
					{ name: "protoc-gen-go", version: "1.36.12" },
					{ name: "steady", version: "3.0" },
				],
			),
		).toEqual([
			{ name: "chromium", from: ["150"], to: ["153"] },
			{ name: "protoc-gen-go", from: ["1.36.11"], to: ["1.36.12"] },
		]);
	});
});

describe("untrusted package diff artifacts", () => {
	test("accepts a valid artifact", () => {
		expect(parseArtifact(artifactJson()).headSha).toBe(HEAD_SHA);
	});

	test.each([
		["oversize", () => " ".repeat(MAX_ARTIFACT_BYTES + 1)],
		["wrong SHA length", () => artifactJson().replace(HEAD_SHA, "abc")],
		["non-hex SHA", () => artifactJson().replace(HEAD_SHA, "g".repeat(40))],
		[
			"unknown label",
			() => artifactJson().replace("dev shell", "unknown target"),
		],
		[
			"unknown error code",
			() => artifactJson().replace('"direct"', '"error":"other","direct"'),
		],
		[
			"unsafe package name",
			() =>
				artifactJson([{ name: "hello\nunsafe", from: ["1.0"], to: ["2.0"] }]),
		],
		[
			"unsafe package version",
			() => artifactJson([{ name: "hello", from: ["<script>"], to: ["2.0"] }]),
		],
	])("rejects %s", (_name, create) => {
		expect(() => parseArtifact(create())).toThrow();
	});
});

describe("package diff section rendering", () => {
	test("starts at the marker and renders direct and collapsed closure changes", () => {
		const artifact = parseArtifact(artifactJson());
		const section = renderSection(
			artifact,
			65_536,
			"https://github.com/RigelBuild/compass/actions/runs/1",
		);
		expect(section.split("\n")[0]).toBe(REVIEWABLE_MARKER);
		expect(section).toContain("## Packages that change");
		expect(section).toContain("- hello: 1.0 → 2.0");
		expect(section).toContain(
			"<details><summary>Transitive closure: 1 changes</summary>",
		);
		expect(sectionHeadSha(section)).toBe(HEAD_SHA);
	});

	test("truncates only closure rows and reports the omitted count", () => {
		const changes = Array.from({ length: 30 }, (_, index) => ({
			name: `package-${index}`,
			from: ["1.0"],
			to: ["2.0"],
		}));
		const artifact = parseArtifact(
			JSON.stringify({
				headSha: HEAD_SHA,
				skip: false,
				results: [{ label: "dev shell", direct: [], transitive: changes }],
			}),
		);
		const section = renderSection(
			artifact,
			500,
			"https://github.com/RigelBuild/compass/actions/runs/1",
		);
		expect(section.length).toBeLessThanOrEqual(500);
		expect(section).toContain(
			"lines omitted to fit the 500-character body limit.",
		);
	});
	test("fits closure rows within the available body tail budget", () => {
		const changes = Array.from({ length: 30 }, (_, index) => ({
			name: `package-${index}`,
			from: ["1.0"],
			to: ["2.0"],
		}));
		const artifact = parseArtifact(
			JSON.stringify({
				headSha: HEAD_SHA,
				skip: false,
				results: [{ label: "dev shell", direct: [], transitive: changes }],
			}),
		);
		const body = `Renovate body ${"x".repeat(65_000)}`;
		const prefixLength = `${body}\n\n`.length;
		const section = renderSection(
			artifact,
			65_536 - prefixLength,
			"https://github.com/RigelBuild/compass/actions/runs/1",
		);
		const updated = upsertSection(body, section);
		expect(updated.length).toBeLessThanOrEqual(65_536);
		expect(section).toContain(
			"lines omitted to fit the 520-character body limit.",
		);
	});

	test("replaces only an existing Reviewable tail", () => {
		const section = `${REVIEWABLE_MARKER}\nnew section`;
		expect(
			upsertSection(
				`Renovate body\n${REVIEWABLE_MARKER}\nold section`,
				section,
			),
		).toBe(`Renovate body\n${section}`);
	});
});
