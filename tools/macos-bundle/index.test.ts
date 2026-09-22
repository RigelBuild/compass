// Unit tests for the macos-bundle pure core (index.ts).
//
// These defend the bundler's pure contract: the Info.plist template carries
// every required CFBundle key with the passed values (name/executable/
// identifier), both version keys carry the clean semver (GC4), the package type
// is APPL, and the arg parser accepts the release-grammar flags (including the
// repeatable --sidecar) + fails loud on missing/duplicate/unknown input
// (build.sh sanity posture).
//
// Only the PURE core is exercised — the edge (codesign / hdiutil / staging) is
// import.meta.main-guarded, so importing index.ts never runs it.

import { describe, expect, test } from "bun:test";
import {
	formatBusyDiagnosis,
	parseArgs,
	renderInfoPlist,
	staleMountPoints,
} from "./index.ts";

/** The canonical render inputs used across the plist cases. */
function plistOpts() {
	return {
		name: "Compass",
		executable: "compass-app",
		identifier: "build.rigel.compass",
		version: "1.4.2",
	};
}

/** Extract the <string> value following a given <key> from the plist XML. */
function plistValue(xml: string, key: string): string | undefined {
	const re = new RegExp(`<key>${key}</key>\\s*<string>([^<]*)</string>`);
	return re.exec(xml)?.[1];
}

describe("renderInfoPlist — required CFBundle keys carry the passed values", () => {
	test("CFBundleName is the display name", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleName")).toBe(
			"Compass",
		);
	});

	test("CFBundleExecutable is the binary name", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleExecutable")).toBe(
			"compass-app",
		);
	});

	test("CFBundleIdentifier is the reverse-DNS id", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleIdentifier")).toBe(
			"build.rigel.compass",
		);
	});

	test("CFBundlePackageType is APPL", () => {
		expect(
			plistValue(renderInfoPlist(plistOpts()), "CFBundlePackageType"),
		).toBe("APPL");
	});

	test("CFBundleInfoDictionaryVersion is 6.0", () => {
		expect(
			plistValue(renderInfoPlist(plistOpts()), "CFBundleInfoDictionaryVersion"),
		).toBe("6.0");
	});

	test("LSMinimumSystemVersion is present and non-empty", () => {
		const v = plistValue(
			renderInfoPlist(plistOpts()),
			"LSMinimumSystemVersion",
		);
		expect(v).toBeDefined();
		expect(v).not.toBe("");
	});
});

describe("renderInfoPlist — both version keys carry the clean semver (GC4)", () => {
	test("CFBundleShortVersionString is the passed semver", () => {
		expect(
			plistValue(
				renderInfoPlist({ ...plistOpts(), version: "2.0.0" }),
				"CFBundleShortVersionString",
			),
		).toBe("2.0.0");
	});

	test("CFBundleVersion is the passed semver", () => {
		expect(
			plistValue(
				renderInfoPlist({ ...plistOpts(), version: "2.0.0" }),
				"CFBundleVersion",
			),
		).toBe("2.0.0");
	});

	test("both version keys agree — ONE version stamp", () => {
		const xml = renderInfoPlist({ ...plistOpts(), version: "3.1.4" });
		expect(plistValue(xml, "CFBundleShortVersionString")).toBe(
			plistValue(xml, "CFBundleVersion"),
		);
	});
});

describe("renderInfoPlist — well-formed plist envelope", () => {
	test("carries the plist DOCTYPE + a single dict", () => {
		const xml = renderInfoPlist(plistOpts());
		expect(xml).toContain("<!DOCTYPE plist PUBLIC");
		expect(xml).toContain('<plist version="1.0">');
		expect(xml).toContain("<dict>");
		expect(xml).toContain("</dict>");
	});

	test("XML-escapes a value that would otherwise break the string element", () => {
		const xml = renderInfoPlist({ ...plistOpts(), name: "A & B <C>" });
		expect(xml).toContain("<string>A &amp; B &lt;C&gt;</string>");
	});
});

describe("parseArgs — accepts the release-grammar flags", () => {
	test("parses all four flags into a typed BundleArgs", () => {
		const args = parseArgs([
			"--binary",
			"/tmp/compass-app",
			"--dist",
			"apps/ui/dist",
			"--version",
			"1.4.2",
			"--out",
			"/tmp/out.dmg",
		]);
		expect(args).toEqual({
			binary: "/tmp/compass-app",
			dist: "apps/ui/dist",
			version: "1.4.2",
			out: "/tmp/out.dmg",
			sidecars: [],
		});
	});
});

describe("parseArgs — repeatable --sidecar collects the embedded sidecars", () => {
	/** The required grammar every sidecar case is layered onto. */
	function requiredArgs(): string[] {
		return [
			"--binary",
			"/tmp/compass-app",
			"--dist",
			"apps/ui/dist",
			"--version",
			"1.4.2",
			"--out",
			"/tmp/out.dmg",
		];
	}

	test("collects repeated --sidecar values in the order given", () => {
		const args = parseArgs([
			...requiredArgs(),
			"--sidecar",
			"/tmp/compass-stack",
			"--sidecar",
			"/tmp/compass-server",
			"--sidecar",
			"/tmp/compass-runner",
		]);
		expect(args.sidecars).toEqual([
			"/tmp/compass-stack",
			"/tmp/compass-server",
			"/tmp/compass-runner",
		]);
	});

	test("the duplicate check still fires for single-valued flags", () => {
		expect(() => parseArgs([...requiredArgs(), "--dist", "other"])).toThrow(
			/more than once/,
		);
	});

	test("no --sidecar yields an empty array, not a parse error", () => {
		expect(parseArgs(requiredArgs()).sidecars).toEqual([]);
	});

	test("a single --sidecar still collects into an array", () => {
		const args = parseArgs([...requiredArgs(), "--sidecar", "/tmp/x"]);
		expect(args.sidecars).toEqual(["/tmp/x"]);
	});

	test("--sidecar with no value fails loud like any other flag", () => {
		expect(() => parseArgs([...requiredArgs(), "--sidecar"])).toThrow(
			/'--sidecar' expects a value/,
		);
	});

	test("--sidecar swallowing the next flag as its value fails loud", () => {
		expect(() =>
			parseArgs(["--sidecar", "--binary", "/tmp/compass-app"]),
		).toThrow(/'--sidecar' expects a value/);
	});

	test("rejects a sidecar whose basename collides with the shell", () => {
		expect(() =>
			parseArgs([...requiredArgs(), "--sidecar", "/other/compass-app"]),
		).toThrow(/collides with the shell/);
	});

	test("a sidecar basenamed compass-app throws even when --binary is basenamed otherwise", () => {
		expect(() =>
			parseArgs([
				"--binary",
				"/tmp/compass-app-darwin-arm64",
				"--dist",
				"/d",
				"--version",
				"1.0.0",
				"--out",
				"/o.dmg",
				"--sidecar",
				"/tmp/compass-app",
			]),
		).toThrow(/collides with the shell executable/);
	});

	test("a --binary basenamed otherwise with a same-named sidecar does NOT throw", () => {
		expect(
			parseArgs([
				"--binary",
				"/b/shell-x",
				"--dist",
				"/d",
				"--version",
				"1.0.0",
				"--out",
				"/o.dmg",
				"--sidecar",
				"/s/shell-x",
			]).sidecars,
		).toEqual(["/s/shell-x"]);
	});

	test("rejects two sidecars sharing a basename", () => {
		expect(() =>
			parseArgs([
				...requiredArgs(),
				"--sidecar",
				"/a/compass-stack",
				"--sidecar",
				"/b/compass-stack",
			]),
		).toThrow(/duplicate sidecar basename/);
	});
});

describe("parseArgs — fails loud on malformed input", () => {
	test("throws naming a missing required flag", () => {
		expect(() =>
			parseArgs(["--binary", "/tmp/b", "--dist", "d", "--version", "1.0.0"]),
		).toThrow(/--out/);
	});

	test("throws on an unknown flag", () => {
		expect(() => parseArgs(["--nope", "x"])).toThrow(/unknown/);
	});

	test("throws on a flag missing its value", () => {
		expect(() => parseArgs(["--binary"])).toThrow(/expects a value/);
	});

	test("throws on a flag given twice", () => {
		expect(() => parseArgs(["--binary", "a", "--binary", "b"])).toThrow(
			/more than once/,
		);
	});

	test("throws when a flag's value is another flag (missing value)", () => {
		expect(() => parseArgs(["--binary", "--dist", "d"])).toThrow(
			/expects a value/,
		);
	});
});

/**
 * Build one `hdiutil info -plist` image block for an image at `imagePath` with
 * the given mount points. The real plist is larger; these are the only keys
 * staleMountPoints reads, in the order hdiutil emits them.
 */
function imageBlock(imagePath: string, mountPoints: string[]): string {
	const entities = mountPoints
		.map((mp) => `<dict><key>mount-point</key><string>${mp}</string></dict>`)
		.join("");
	return `<key>image-path</key><string>${imagePath}</string><key>system-entities</key><array>${entities}</array>`;
}

const TARGET = {
	imagePath: "/tmp/compass-app-darwin-arm64.dmg",
	volumeName: "Compass",
};

describe("staleMountPoints — selects only this build's leaked attachment", () => {
	test("no attachments → empty", () => {
		expect(staleMountPoints("<dict></dict>", TARGET)).toEqual([]);
	});

	test("empty input → empty, does not throw", () => {
		expect(staleMountPoints("", TARGET)).toEqual([]);
	});

	test("malformed input → empty, does not throw", () => {
		expect(staleMountPoints("not a plist <<< >>>", TARGET)).toEqual([]);
	});

	test("matches by image path", () => {
		const info = imageBlock(TARGET.imagePath, ["/Volumes/Compass"]);
		expect(staleMountPoints(info, TARGET)).toEqual(["/Volumes/Compass"]);
	});

	test("matches by Compass volume name even when the image path differs", () => {
		const info = imageBlock("/tmp/some-other-run.dmg", [
			"/private/tmp/Compass",
		]);
		expect(staleMountPoints(info, TARGET)).toEqual(["/private/tmp/Compass"]);
	});

	test("does NOT select an unrelated volume", () => {
		const info = imageBlock("/tmp/unrelated.dmg", ["/Volumes/SomethingElse"]);
		expect(staleMountPoints(info, TARGET)).toEqual([]);
	});

	test("selects only the matching block when both are attached", () => {
		const info =
			imageBlock("/tmp/unrelated.dmg", ["/Volumes/SomethingElse"]) +
			imageBlock(TARGET.imagePath, ["/Volumes/Compass"]);
		expect(staleMountPoints(info, TARGET)).toEqual(["/Volumes/Compass"]);
	});
});

describe("formatBusyDiagnosis — names the holder of a busy staging tree", () => {
	const probes = {
		stageRoot: "/tmp/macos-bundle-stage",
		lsof: {
			exitCode: 0,
			stdout: "COMMAND   PID  USER\ncodesign 4711 runner",
			stderr: "",
		},
		hdiutilInfo: { exitCode: 0, stdout: "no image attached", stderr: "" },
	};

	test("the banner states the failure and the tree it probed", () => {
		expect(formatBusyDiagnosis(probes)).toContain(
			"hdiutil create failed; probing what holds /tmp/macos-bundle-stage",
		);
	});

	test("carries the lsof holder, which is the whole point of the probe", () => {
		expect(formatBusyDiagnosis(probes)).toContain("codesign 4711 runner");
	});

	test("carries the hdiutil attachment state alongside it", () => {
		expect(formatBusyDiagnosis(probes)).toContain("no image attached");
	});

	test("an empty probe reads as no output, never as a blank section", () => {
		const out = formatBusyDiagnosis({
			...probes,
			lsof: { exitCode: 1, stdout: "   \n  ", stderr: "" },
		});
		expect(out).toContain("(no output)");
	});

	test("a silent lsof exit 1 is readable as no holder, not as a failed probe", () => {
		const out = formatBusyDiagnosis({
			...probes,
			lsof: { exitCode: 1, stdout: "", stderr: "" },
		});
		expect(out).toContain("lsof +D /tmp/macos-bundle-stage (exit 1)");
	});

	test("a timed-out probe says so instead of reporting an exit code", () => {
		const out = formatBusyDiagnosis({
			...probes,
			lsof: { exitCode: "timed out", stdout: "", stderr: "" },
		});
		expect(out).toContain("(exit timed out)");
	});

	test("stderr keeps its own line when stdout has no trailing newline", () => {
		const out = formatBusyDiagnosis({
			...probes,
			lsof: { exitCode: 1, stdout: "NOTRAILING", stderr: "WARN" },
		});
		expect(out).toContain("NOTRAILING\nWARN");
	});
});
