// Pins the matching semantics of the checked-in boundary configs; the shared
// package only validates their shape. Scans run through git grep so the prefilter counts.
import { afterAll, describe, expect, test } from "bun:test";
import {
	existsSync,
	mkdirSync,
	mkdtempSync,
	readdirSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import {
	findViolations,
	loadRefGateConfig,
	type RefGateConfig,
} from "@rigelbuild/ref-gate";

const here = import.meta.dir;
const repoRoot = join(here, "../..");
const privateName = loadRefGateConfig(join(here, "private-name.json"));
const sea = loadRefGateConfig(join(here, "sea.json"));
const configPath = new Map([
	[privateName, join(here, "private-name.json")],
	[sea, join(here, "sea.json")],
]);
const scratch: string[] = [];
afterAll(() => {
	for (const dir of scratch) rmSync(dir, { recursive: true, force: true });
});

// Hook-exported GIT_* variables would aim git at the outer checkout. Bun's
// spawn ignores `delete process.env`, so every git call gets an explicit env.
const gitEnv = (env: Record<string, string | undefined>) =>
	Object.fromEntries(
		Object.entries(env).filter(([key]) => !key.startsWith("GIT_")),
	);
let inherited: Record<string, string | undefined> = process.env;

function run(cmd: string[]): string {
	const r = Bun.spawnSync({ cmd, env: gitEnv(inherited), stderr: "pipe" });
	if (r.exitCode !== 0) throw new Error(r.stderr.toString());
	return r.stdout.toString();
}

// Stages each file in a fresh repo and returns the paths the gate flags.
function flagged(cfg: RefGateConfig, files: Record<string, string>): string[] {
	const root = mkdtempSync(join(tmpdir(), "ref-gates-"));
	scratch.push(root);
	for (const [path, text] of Object.entries(files)) {
		mkdirSync(dirname(join(root, path)), { recursive: true });
		writeFileSync(join(root, path), `${text}\n`);
	}
	run(["git", "-C", root, "init", "-q"]);
	run(["git", "-C", root, "add", "-A"]);
	const scan = [process.execPath, join(here, "scan.fixture.ts"), root];
	const hits = JSON.parse(
		run([...scan, configPath.get(cfg) ?? ""]),
	) as string[];
	return [...new Set(findViolations(cfg, hits).map((v) => v.file))].sort();
}

const caseFiles = (texts: string[]) =>
	Object.fromEntries(texts.map((text, i) => [`docs/case-${i}.md`, text]));

function expectExactlyFlagged(
	cfg: RefGateConfig,
	hits: string[],
	misses: string[],
) {
	const files = caseFiles([...hits, ...misses]);
	const want = hits.map((_, i) => `docs/case-${i}.md`).sort();
	const got = flagged(cfg, files);
	expect(got.map((f) => files[f])).toEqual(want.map((f) => files[f]));
}

describe("private-name config", () => {
	test("flags private-repo references and spares look-alikes", () => {
		expectExactlyFlagged(
			privateName,
			[
				"ported from orion's config",
				"the ORION monorepo",
				"`orion tools/publish-agent-config/index.ts:79`",
				"orion-ref-gate scans for orion references",
				"See `sealed/ci/pipeline.ts:554` for the job",
				"mirrors `sealed gather.ts:25,203-205`",
				"ported from sealed flake.nix",
				"a sealed-monorepo domain",
				"deployed to sealed-docs.rigel.build",
				"following Sealed's shape",
				"this record lives in the sealed design corpus",
				"per sealed convention a later change ADDS",
			],
			[
				"orionic nebula naming",
				"categorion",
				"orion-ref-gate: 'tools/orion-ref-gate'",
				"values sealed and/or rotated",
				"Frame is a sealed sum type",
				"an egress-sealed agent holds no server token",
				"github.com/sealedsecurity/compass/go",
				"ci.sealedsecurity.com",
			],
		);
	});

	test("carves out the lockfile, its own configs and generated docsite copies", () => {
		const carved = [
			"bun.lock",
			"tools/ref-gates/private-name.json",
			"apps/eng-docs/src/content/docs/x.md",
			"apps/eng-docs/dist/x/index.html",
		];
		const files = Object.fromEntries(
			[...carved, "docs/x.md"].map((path) => [path, "from orion"]),
		);
		expect(flagged(privateName, files)).toEqual(["docs/x.md"]);
	});

	test("its remediation doc exists", () => {
		const doc = privateName.remediationDoc;
		expect(doc).toBeDefined();
		expect(existsSync(join(repoRoot, doc ?? ""))).toBe(true);
	});
});

describe("sea config", () => {
	test("flags issue keys and spares slugs and placeholders", () => {
		expectExactlyFlagged(
			sea,
			["tracked as SEA-1512 here", "(SEA-42)"],
			["branch sea-1512-fix", "a SEA-nnn placeholder", "SEA-NNN", "XSEA-12"],
		);
	});

	test("carves out the lockfile, changelog and its own configs", () => {
		const carved = ["bun.lock", "CHANGELOG.md", "tools/ref-gates/sea.json"];
		const files = Object.fromEntries(
			[...carved, "docs/x.md"].map((path) => [path, "see SEA-1512"]),
		);
		expect(flagged(sea, files)).toEqual(["docs/x.md"]);
	});
});

test("a hook's git env cannot redirect the fixture repos", () => {
	const decoy = mkdtempSync(join(tmpdir(), "ref-gates-decoy-"));
	scratch.push(decoy);
	const poison = [
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_COMMON_DIR",
	];
	inherited = {
		...process.env,
		...Object.fromEntries(poison.map((key) => [key, join(decoy, key)])),
	};
	try {
		expect(flagged(sea, { "docs/x.md": "see SEA-1512" })).toEqual([
			"docs/x.md",
		]);
	} finally {
		inherited = process.env;
	}
	expect(readdirSync(decoy)).toEqual([]);
});

// Moon reports the gates affected only through workspace-root inputs, and a
// manifest-only change must still bust the typecheck cache.
test("gate and typecheck tasks keep their cache-key inputs", () => {
	const moon = Bun.YAML.parse(readFileSync(join(here, "moon.yml"), "utf8")) as {
		tasks: Record<string, { inputs?: string[] }>;
	};
	for (const task of ["sea", "private-name"]) {
		expect(moon.tasks[task]?.inputs).toContain("/**/*");
	}
	expect(moon.tasks.typecheck?.inputs).toContain("package.json");
});
