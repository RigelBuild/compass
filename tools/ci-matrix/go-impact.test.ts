import { beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, extname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { loadGoGraph } from "./go-graph.ts";
import {
	E2E_BUILT_BINARIES,
	E2E_ROOTS,
	GATE_PATHS,
	type GoGraph,
	type GoSuite,
	residuePaths,
	SUITE_OWNER_TASKS,
	type SuiteSelection,
	selectGoSuites,
} from "./go-impact.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const goDir = join(repoRoot, "go");
const allSuites: readonly GoSuite[] = ["pgtest", "microvm", "e2e"];

function suiteSelection(
	roots: readonly string[],
	reason = "none",
): SuiteSelection {
	return { run: roots.length > 0, reason, roots };
}

const fixtureGraph: GoGraph = {
	packages: [
		"cmd/compass-postgres",
		"cmd/compass-runner",
		"cmd/compass-server",
		"cmd/compass-stack",
		"e2e",
		"internal/auth",
		"internal/board",
		"internal/comms",
		"internal/delivery",
		"internal/gatewaycred",
		"internal/mirror",
		"internal/pgshare",
		"internal/runnerhub",
		"internal/secrets",
		"internal/stack/adapters",
		"internal/store",
		"internal/usage",
		"internal/runner",
		"internal/runtime",
		"server",
		"internal/microvmtest",
		"internal/runtime/microvm",
	],
	closure: {
		pgtest: {
			"internal/auth": ["internal/auth", "internal/store"],
			"internal/board": ["internal/board", "internal/store"],
			"internal/comms": ["internal/comms", "internal/store"],
			"internal/delivery": ["internal/delivery", "internal/store"],
			"internal/gatewaycred": ["internal/gatewaycred", "internal/store"],
			"internal/mirror": ["internal/mirror", "internal/store"],
			"internal/pgshare": ["internal/pgshare", "internal/store"],
			"internal/runnerhub": ["internal/runnerhub", "internal/store"],
			"internal/secrets": ["internal/secrets", "internal/store"],
			"internal/stack/adapters": ["internal/stack/adapters", "internal/store"],
			"internal/store": ["internal/store"],
			"internal/usage": ["internal/usage"],
			server: ["server"],
		},
		microvm: {
			"internal/microvmtest": ["internal/microvmtest"],
			"internal/runner": ["internal/runner", "internal/runtime"],
			"internal/runtime": ["internal/runtime"],
			"internal/runtime/microvm": ["internal/runtime/microvm"],
		},
		e2e: {
			e2e: ["e2e", "internal/store", "server"],
			"cmd/compass-stack": ["cmd/compass-stack", "internal/runtime"],
			"cmd/compass-postgres": ["cmd/compass-postgres", "internal/store"],
			"cmd/compass-server": ["cmd/compass-server", "internal/store", "server"],
			"cmd/compass-runner": ["cmd/compass-runner", "internal/runtime"],
		},
	},
	erroredRoots: { pgtest: [], microvm: [], e2e: [] },
};

function select(
	changedPaths: readonly string[],
	options: {
		event?: "pull_request" | "push" | "schedule";
		affectedIds?: readonly string[];
		residueTasks?: readonly string[];
		graph?: GoGraph;
	} = {},
): Record<GoSuite, SuiteSelection> {
	return selectGoSuites({
		event: options.event ?? "pull_request",
		changedPaths,
		affectedIds: options.affectedIds ?? [],
		residueTasks: options.residueTasks ?? [],
		graph: options.graph ?? fixtureGraph,
	});
}

let liveGraph: GoGraph;

function goListTestFiles(
	tag?: "pgtest" | "microvm" | "podman",
): Map<string, string[]> {
	const args = ["list", "-e"];
	if (tag !== undefined) args.push("-tags", tag);
	args.push(
		"-f",
		`{{.Dir}}|{{join .TestGoFiles ","}}|{{join .XTestGoFiles ","}}`,
		"./...",
	);
	const result = Bun.spawnSync(["go", ...args], {
		cwd: goDir,
		env: {
			...process.env,
			GOFLAGS: "-mod=mod",
			GOPROXY: "off",
			CGO_ENABLED: "1",
		},
		stdout: "pipe",
		stderr: "pipe",
	});
	if (result.exitCode !== 0)
		throw new Error(`go list ${args.join(" ")}: ${result.stderr.toString()}`);
	const filesByRoot = new Map<string, string[]>();
	for (const line of result.stdout.toString().trim().split("\n")) {
		const [directory, internal, external] = line.split("|");
		if (
			directory === undefined ||
			internal === undefined ||
			external === undefined
		)
			continue;
		const root = relative(goDir, directory).split(sep).join("/");
		filesByRoot.set(
			root,
			[...internal.split(","), ...external.split(",")]
				.filter((file) => file !== "")
				.sort(),
		);
	}
	return filesByRoot;
}

function changedTestRootsFromGoList(suite: GoSuite): string[] {
	const tag = suite === "e2e" ? "podman" : suite;
	const untagged = goListTestFiles();
	const tagged = goListTestFiles(tag);
	return [...new Set([...untagged.keys(), ...tagged.keys()])]
		.filter(
			(root) =>
				JSON.stringify(untagged.get(root) ?? []) !==
				JSON.stringify(tagged.get(root) ?? []),
		)
		.sort();
}

// The live checks spawn go list and moon; a loaded pre-push run blows bun's 5 s default.
const LIVE_TIMEOUT_MS = 60_000;

beforeAll(async () => {
	liveGraph = await loadGoGraph(goDir);
}, LIVE_TIMEOUT_MS);

describe("Go suite selection", () => {
	test("a server-only change selects only the pgtest server root", () => {
		const selected = select(["go/server/service_test.go"]);
		expect(selected.pgtest.roots).toEqual(["server"]);
		expect(selected.pgtest.reason).toBe("closure: server");
		expect(selected.microvm.run).toBe(false);
		expect(selected.e2e.run).toBe(true);
	});

	test("a store change selects every pgtest root that imports store", () => {
		const selected = select(["go/internal/store/store_test.go"]);
		expect(selected.pgtest.roots).toEqual(
			Object.entries(fixtureGraph.closure.pgtest)
				.filter(([, closure]) => closure.includes("internal/store"))
				.map(([root]) => root)
				.sort(),
		);
		expect(selected.pgtest.roots).toHaveLength(11);
		expect(selected.pgtest.run).toBe(true);
	});

	test("two changed packages select the union of their roots", () => {
		const selected = select([
			"go/server/service.go",
			"go/internal/usage/usage.go",
		]);
		expect(selected.pgtest.roots).toEqual(["internal/usage", "server"]);
		expect(selected.pgtest.reason).toBe("closure: internal/usage, server");
	});

	test("devenv.lock widens suites through the changed owner task", () => {
		const selected = select(["devenv.lock"], {
			residueTasks: ["compass-go:test"],
		});
		for (const suite of allSuites) {
			expect(selected[suite].run).toBe(true);
			expect(selected[suite].reason).toBe("owner: compass-go:test");
		}
	});

	test("a design-only change leaves e2e skipped", () => {
		expect(select(["docs/designs/notes.md"]).e2e).toEqual(
			suiteSelection([], "none"),
		);
	});

	test("a ci.yml-only change widens every suite", () => {
		const selected = select([".github/workflows/ci.yml"]);
		for (const suite of allSuites) {
			expect(selected[suite].run).toBe(true);
			expect(selected[suite].reason).toBe("wide: .github/workflows/ci.yml");
		}
	});

	test.each(["push", "schedule"] as const)(
		"%s selects every test root",
		(event) => {
			const selected = select([], { event });
			for (const suite of allSuites) {
				expect(selected[suite].run).toBe(true);
				expect(selected[suite].roots).toEqual(
					suite === "e2e"
						? [...E2E_ROOTS].sort()
						: Object.keys(fixtureGraph.closure[suite]).sort(),
				);
			}
		},
	);

	test("an errored root widens its suite", () => {
		const graph: GoGraph = {
			...fixtureGraph,
			erroredRoots: { pgtest: ["internal/store"], microvm: [], e2e: [] },
		};
		const selected = select(["go/server/service_test.go"], { graph });
		expect(selected.pgtest).toEqual({
			run: true,
			reason: "wide: go list error",
			roots: [
				"internal/auth",
				"internal/board",
				"internal/comms",
				"internal/delivery",
				"internal/gatewaycred",
				"internal/mirror",
				"internal/pgshare",
				"internal/runnerhub",
				"internal/secrets",
				"internal/stack/adapters",
				"internal/store",
				"internal/usage",
				"server",
			],
		});
	});

	test("unmapped Go paths widen instead of narrowing", () => {
		const selected = select(["go/moon.yml"]);
		for (const suite of allSuites) {
			expect(selected[suite].run).toBe(true);
			expect(selected[suite].reason).toBe("wide: go/moon.yml");
		}
	});

	test("guest image changes widen microvm regardless of its Go closure", () => {
		const selected = select(["guest-image/default.nix"], {
			affectedIds: ["compass-guest-image"],
		});
		expect(selected.microvm.run).toBe(true);
		expect(selected.microvm.reason).toBe("wide: compass-guest-image");
	});
});

describe("Go graph discovered from the module", () => {
	test("tagged test file changes determine pgtest and microvm roots", () => {
		for (const suite of ["pgtest", "microvm"] as const) {
			expect(Object.keys(liveGraph.closure[suite])).toEqual(
				changedTestRootsFromGoList(suite),
			);
		}
	});

	test("store changes select every pgtest root whose live closure contains store", () => {
		const selected = selectGoSuites({
			event: "pull_request",
			changedPaths: ["go/internal/store/store_test.go"],
			affectedIds: ["compass-go"],
			residueTasks: [],
			graph: liveGraph,
		});
		expect(selected.pgtest.roots).toEqual(
			Object.entries(liveGraph.closure.pgtest)
				.filter(([, closure]) => closure.includes("internal/store"))
				.map(([root]) => root)
				.sort(),
		);
	});

	test(
		"a cold module cache yields the same graph as the warm one",
		async () => {
			// CI setup has no module cache, and GOPROXY=off leaves external records without a Dir.
			const cold = mkdtempSync(join(tmpdir(), "ci-matrix-gomodcache-"));
			const previous = process.env.GOMODCACHE;
			process.env.GOMODCACHE = cold;
			try {
				expect(await loadGoGraph(goDir)).toEqual(liveGraph);
			} finally {
				if (previous === undefined) delete process.env.GOMODCACHE;
				else process.env.GOMODCACHE = previous;
				rmSync(cold, { recursive: true, force: true });
			}
		},
		LIVE_TIMEOUT_MS,
	);

	test(
		"E2E roots and built binaries match their source",
		async () => {
			const builtBinaries = (source: string): string[] =>
				[...source.matchAll(/for _, name := range \[\]string\{([^}]*)\}/g)]
					.flatMap((match) => [...(match[1] ?? "").matchAll(/"([^"]+)"/g)])
					.map((match) => match[1])
					.filter((name): name is string => name !== undefined)
					.sort();
			const workflow = await Bun.file(
				join(repoRoot, ".github/workflows/ci.yml"),
			).text();
			const e2eMain = await Bun.file(join(goDir, "e2e/main_test.go")).text();
			const stackIntegration = await Bun.file(
				join(goDir, "cmd/compass-stack/integration_podman_test.go"),
			).text();
			const e2eCommand = workflow.match(/go test -tags podman[^\n]*/);
			expect(e2eCommand).not.toBeNull();
			const workflowRoots = (e2eCommand?.[0] ?? "")
				.split(/\s+/)
				.filter(
					(argument) => argument.startsWith("./") && argument.endsWith("/..."),
				)
				.map((argument) => argument.slice(2, -4))
				.sort();
			expect(workflowRoots).toEqual([...E2E_ROOTS].sort());
			const expectedBinaries = [...E2E_BUILT_BINARIES]
				.map((binary) => binary.slice("cmd/".length))
				.sort();
			expect(builtBinaries(e2eMain)).toEqual(expectedBinaries);
			expect(builtBinaries(stackIntegration)).toEqual(expectedBinaries);
			expect(e2eMain).toMatch(
				/cmd := exec\.CommandContext\(ctx, "go", "build",[\s\S]*?"\.\/cmd\/"\+name\)/,
			);
			expect(stackIntegration).toMatch(
				/cmd := exec\.CommandContext\(t\.Context\(\), "go", "build",[\s\S]*?"\.\/cmd\/"\+name\)/,
			);
		},
		LIVE_TIMEOUT_MS,
	);
});

describe("gate constants and path mapping", () => {
	test("the workflow, matrix, and Moon configuration paths are gate inputs", () => {
		expect(GATE_PATHS).toEqual([
			".github/workflows/ci.yml",
			"tools/ci-matrix/",
			".moon/",
		]);
	});

	test("residue contains paths that do not map to a Go package", () => {
		expect(
			residuePaths(
				["go/server/service.go", "devenv.lock", "go/go.mod"],
				fixtureGraph,
			),
		).toEqual(["devenv.lock", "go/go.mod"]);
	});
});

type MoonInput =
	| { readonly kind: "file"; readonly path: string }
	| { readonly kind: "glob"; readonly path: string };

function moon(args: readonly string[], stdin?: string): string {
	const result = Bun.spawnSync(["moon", ...args], {
		cwd: repoRoot,
		stdin: stdin === undefined ? "ignore" : Buffer.from(stdin),
		stdout: "pipe",
		stderr: "pipe",
	});
	if (result.exitCode !== 0) {
		throw new Error(
			`moon ${args.join(" ")} exited ${result.exitCode}: ${result.stderr.toString()}`,
		);
	}
	return result.stdout.toString();
}

async function moonAsync(
	args: readonly string[],
	stdin: string,
): Promise<string> {
	const proc = Bun.spawn(["moon", ...args], {
		cwd: repoRoot,
		stdin: Buffer.from(stdin),
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	if (exitCode !== 0)
		throw new Error(`moon ${args.join(" ")} exited ${exitCode}: ${stderr}`);
	return stdout;
}

function taskProjectDir(target: string): string {
	const separator = target.indexOf(":");
	if (separator < 1) throw new Error(`invalid Moon task target ${target}`);
	const payload: unknown = JSON.parse(
		moon(["query", "projects", "--id", target.slice(0, separator)]),
	);
	if (
		typeof payload !== "object" ||
		payload === null ||
		!("projects" in payload) ||
		!Array.isArray(payload.projects)
	) {
		throw new Error(`moon query projects returned no project for ${target}`);
	}
	const project: unknown = payload.projects[0];
	if (
		typeof project !== "object" ||
		project === null ||
		!("source" in project) ||
		typeof project.source !== "string"
	) {
		throw new Error(`moon query projects returned no source for ${target}`);
	}
	return project.source;
}

function taskInputs(target: string): MoonInput[] {
	const payload: unknown = JSON.parse(moon(["task", target, "--json"]));
	if (
		typeof payload !== "object" ||
		payload === null ||
		!("inputs" in payload) ||
		!Array.isArray(payload.inputs)
	) {
		throw new Error(`moon task ${target} did not return inputs`);
	}
	return payload.inputs.flatMap((input): MoonInput[] => {
		if (typeof input !== "object" || input === null) return [];
		if ("file" in input && typeof input.file === "string")
			return [{ kind: "file", path: input.file }];
		if ("glob" in input && typeof input.glob === "string")
			return [{ kind: "glob", path: input.glob }];
		return [];
	});
}

function qualifyInput(input: MoonInput, projectDir: string): MoonInput {
	const path = input.path.startsWith("/")
		? input.path.slice(1)
		: join(projectDir, input.path);
	return input.kind === "file"
		? { kind: "file", path }
		: { kind: "glob", path };
}

function sampleFiles(inputs: readonly MoonInput[]): string[] {
	const samples = new Set<string>();
	for (const input of inputs) {
		if (input.kind === "file") {
			const absolutePath = join(repoRoot, input.path);
			if (existsSync(absolutePath) && statSync(absolutePath).isFile())
				samples.add(input.path);
			continue;
		}
		const byExtension = new Map<string, string>();
		for (const file of [
			...new Bun.Glob(input.path).scanSync({ cwd: repoRoot, dot: true }),
		].sort()) {
			if (!file.includes("node_modules/"))
				byExtension.set(extname(file), byExtension.get(extname(file)) ?? file);
		}
		for (const file of byExtension.values()) samples.add(file);
	}
	return [...samples].sort();
}

async function affectedTasks(file: string): Promise<Set<string>> {
	const payload: unknown = JSON.parse(
		await moonAsync(["query", "tasks", "--affected"], `${file}\n`),
	);
	if (
		typeof payload !== "object" ||
		payload === null ||
		!("options" in payload)
	) {
		throw new Error(`moon query tasks returned no options for ${file}`);
	}
	const options = payload.options;
	if (
		typeof options !== "object" ||
		options === null ||
		!("affected" in options)
	) {
		throw new Error(
			`moon query tasks returned no affected options for ${file}`,
		);
	}
	const affected = options.affected;
	if (
		typeof affected !== "object" ||
		affected === null ||
		!("tasks" in affected)
	) {
		throw new Error(`moon query tasks returned no affected tasks for ${file}`);
	}
	const tasks = affected.tasks;
	if (typeof tasks !== "object" || tasks === null || Array.isArray(tasks)) {
		throw new Error(
			`moon query tasks returned an invalid affected map for ${file}`,
		);
	}
	return new Set(Object.keys(tasks));
}

async function ownerInputCovered(
	suite: GoSuite,
	file: string,
): Promise<boolean> {
	const isGoPath =
		file.startsWith("go/") &&
		liveGraph.packages.some(
			(pkg) => file === `go/${pkg}` || file.startsWith(`go/${pkg}/`),
		);
	if (isGoPath) return true;
	const residueTasks = await affectedTasks(file);
	return selectGoSuites({
		event: "pull_request",
		changedPaths: [file],
		affectedIds: [],
		residueTasks: [...residueTasks],
		graph: liveGraph,
	})[suite].run;
}

function ownerInputSamples(): {
	suite: GoSuite;
	target: string;
	file: string;
}[] {
	return allSuites.flatMap((suite) =>
		SUITE_OWNER_TASKS[suite].flatMap((target) => {
			const projectDir = taskProjectDir(target);
			const inputs = taskInputs(target).map((input) =>
				qualifyInput(input, projectDir),
			);
			return sampleFiles(inputs).map((file) => ({ suite, target, file }));
		}),
	);
}

function loadOwnerInputs(): MoonInput[] {
	const targets = new Set(Object.values(SUITE_OWNER_TASKS).flat());
	return [...targets].flatMap((target) => {
		const projectDir = taskProjectDir(target);
		return taskInputs(target).map((input) => qualifyInput(input, projectDir));
	});
}

function parentPathReferences(source: string): string[] {
	const references = new Set<string>();
	for (const match of source.matchAll(
		/(?:filepath\.Join|os\.ReadFile|os\.Stat)\(([\s\S]*?)\)/g,
	)) {
		const argumentsText = match[1];
		if (argumentsText === undefined) continue;
		const segments = [...argumentsText.matchAll(/"([^"]*)"/g)]
			.map((segment) => segment[1])
			.filter((segment): segment is string => segment !== undefined);
		if (
			!segments.some((segment) => segment === ".." || segment.startsWith("../"))
		)
			continue;
		references.add(segments.join("/"));
	}
	return [...references];
}

function packageForFixturePath(modulePath: string): string | undefined {
	const exactPackage = liveGraph.packages.find((pkg) => modulePath === pkg);
	if (exactPackage !== undefined) return exactPackage;
	return liveGraph.packages
		.filter((pkg) => modulePath.startsWith(`${pkg}/`))
		.sort((left, right) => right.length - left.length)[0];
}

function referenceIsCovered(
	file: string,
	reference: string,
	directories: ReadonlySet<string>,
	ownerInputs: readonly MoonInput[],
): boolean {
	const resolved = resolve(dirname(file), reference);
	const resolvedPath = relative(repoRoot, resolved).split(sep).join("/");
	const modulePath = relative(goDir, resolved).split(sep).join("/");
	const packageDir = packageForFixturePath(modulePath);
	const inClosure = packageDir !== undefined && directories.has(packageDir);
	const inOwnerInput = ownerInputs.some((input) => {
		if (input.kind === "file") return input.path === resolvedPath;
		const glob = new Bun.Glob(input.path);
		return glob.match(resolvedPath) || glob.match(`${resolvedPath}/`);
	});
	const isModuleRootNavigation =
		existsSync(resolved) &&
		statSync(resolved).isDirectory() &&
		resolvedPath === "go";
	return inClosure || inOwnerInput || isModuleRootNavigation;
}

async function findTaggedFixtureEscapes(): Promise<string[]> {
	const rootsBySuite: Record<GoSuite, readonly string[]> = {
		pgtest: Object.keys(liveGraph.closure.pgtest),
		microvm: Object.keys(liveGraph.closure.microvm),
		e2e: E2E_ROOTS,
	};
	const ownerInputs = loadOwnerInputs();
	const escapes: string[] = [];
	for (const suite of allSuites) {
		const closureDirs = new Set(Object.values(liveGraph.closure[suite]).flat());
		for (const root of rootsBySuite[suite]) {
			for (const testFile of moonGoTestFiles(root, suiteTag(suite))) {
				const file = join(goDir, root, testFile);
				const source = await Bun.file(file).text();
				for (const reference of parentPathReferences(source)) {
					if (!referenceIsCovered(file, reference, closureDirs, ownerInputs)) {
						escapes.push(`${relative(goDir, file)}: ${reference}`);
					}
				}
			}
		}
	}
	return escapes;
}

describe("owner-task input coverage", () => {
	test(
		"each sampled owner-task input maps to Go or widens its suite",
		async () => {
			const samples = ownerInputSamples();
			const covered = await Promise.all(
				samples.map(({ suite, file }) => ownerInputCovered(suite, file)),
			);
			const uncovered = samples
				.filter((_, i) => !covered[i])
				.map(({ file }) => file);
			expect(uncovered).toEqual([]);
		},
		LIVE_TIMEOUT_MS,
	);
});

describe("tagged-test path escapes", () => {
	test(
		"every parent-relative test fixture is in a Go closure or owner input",
		async () => {
			expect(await findTaggedFixtureEscapes()).toEqual([]);
		},
		LIVE_TIMEOUT_MS,
	);
});

function suiteTag(suite: GoSuite): "pgtest" | "microvm" | "podman" {
	return suite === "e2e" ? "podman" : suite;
}

function moonGoTestFiles(
	root: string,
	tag: "pgtest" | "microvm" | "podman",
): string[] {
	const result = Bun.spawnSync(
		[
			"go",
			"list",
			"-e",
			"-tags",
			tag,
			"-f",
			'{{join .TestGoFiles ","}} {{join .XTestGoFiles ","}}',
			`./${root}`,
		],
		{
			cwd: goDir,
			env: {
				...process.env,
				GOFLAGS: "-mod=mod",
				GOPROXY: "off",
				CGO_ENABLED: "1",
			},
			stdout: "pipe",
			stderr: "pipe",
		},
	);
	if (result.exitCode !== 0)
		throw new Error(`go list ${root}: ${result.stderr.toString()}`);
	const [internal = "", external = ""] = result.stdout
		.toString()
		.trim()
		.split(" ");
	return [...internal.split(","), ...external.split(",")].filter(
		(file) => file !== "",
	);
}
