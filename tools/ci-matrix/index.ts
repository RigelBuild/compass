#!/usr/bin/env bun
// ci-matrix (T5) — the CI concern-matrix generator.
//
// PURE CORE: `generate(input)` translates moon's affected closure + every
// project's `ci-group.*` tag into the per-group GitHub Actions matrix and the
// four special-leg affected flags. It is a pure function: no I/O, no moon
// invocation, no clock, no `process`/`env`/`Bun` access. On a structural
// violation (a project with no `ci-group.*` tag, or two) it throws an Error
// whose message names the offending project id.
//
// THE EDGE: `main()` (guarded by `import.meta.main`) runs `moon query
// projects`, computes changed paths via `git diff`, calls the pure core, and
// writes `matrix=<json>` + the four `*_affected=` lines to `$GITHUB_OUTPUT`.
// Guarding behind `import.meta.main` lets the test import the pure core without
// firing the edge.

import { $ } from "bun";
import { loadGoGraph } from "./go-graph.ts";
import {
	residuePaths,
	type SuiteSelection,
	selectGoSuites,
} from "./go-impact.ts";

// ── Pure-core types ────────────────────────────────────────────────────────

/** The input the pure core receives (parsed from `moon query projects` JSON). */
export type ProjectInput = {
	id: string;
	/** project's config.tags (includes the ci-group.* tag) */
	tags: string[];
	/** "<id>:ci" if the project defines a `ci` task, else null */
	ciTarget: string | null;
};

export type GenInput = {
	projects: ProjectInput[];
	affectedIds: string[];
	changedPaths: string[];
	/** `<project>:<task>` targets from moon's affected-task query. */
	affectedTasks?: string[];
	goSelection: Record<"pgtest" | "microvm" | "e2e", SuiteSelection>;
	event: "pull_request" | "push" | "schedule";
};

export type MatrixEntry = {
	/** "go" | "bun" | "nix" (the tag suffix) */
	group: string;
	/** 'true' iff this group has >=1 runnable target — an affected ci-task-bearing member (always 'true' on push/schedule for a group with a ci task) */
	run: "true" | "false";
	/** the group's runnable ciTargets (sorted): affected members; [] when run==='false' */
	targets: string[];
};

export type GenOutput = {
	/** ONE entry per group EXISTING in the workspace, sorted by group name */
	matrix: MatrixEntry[];
	pgtestAffected: boolean;
	pgtestPackages: string[];
	microvmAffected: boolean;
	microvmPackages: string[];
	microvmRequiresSecretspec: boolean;
	e2eAffected: boolean;
	forgeAffected: boolean;
	gtk4Affected: boolean;
	darwinAffected: boolean;
	agentFodAffected: boolean;
};

// ── Pure-core constants ────────────────────────────────────────────────────

/** The prefix stripped to get the group name: tag is `ci-group.<group>`. */
const CI_GROUP_PREFIX = "ci-group.";

/**
 * The FOD check the arm64 leg reruns natively. Keyed on moon's task-affected set,
 * so the leg tracks the task's declared inputs with no second path list. */
export const AGENT_FOD_TASK = "renovate:agent-fod";

/**
 * forge trigger (PR): a changed path under go/internal/forge/ — the forge
 * CONTRACT SURFACE the live oracle re-verifies (client code AND its committed
 * testdata fixtures). Deliberately NOT keyed on ci.yml: the live oracle is the
 * expensive, third-party-dependent EXTRA verification on top of the untagged
 * golden-replay battery (leg 1) that already asserts the client contract on
 * every PR with zero network, so it should not fire on unrelated PRs that merely
 * touch this workflow file (RIG-2909: a docs/CI-only PR editing ci.yml was
 * running the whole live oracle and flaking on a Linear API blip). Oracle-wiring
 * changes are still covered: every push to main and every schedule full-sweeps
 * the oracle unconditionally (isFullSweep below).
 * PR paths use `HEAD^1..HEAD`: GitHub's first merge parent is the base it merged,
 * so base churn or main drift cannot add files to this PR's trigger.
 */
const FORGE_PATH_RE = /^go\/internal\/forge\//;
/**
 * gtk4 trigger: any changed path under go/cmd/compass-app/, one of the shared
 * GTK closure inputs, OR a Go module manifest. The e2e lane is the ONLY CI lane that compiles the
 * native shell, so a closure-only change (e.g. the T2 atk/gdk-pixbuf trim) must
 * still trigger it — keying on the app path prefix alone would skip it (F2).
 */
const GTK4_PATH_PREFIX = "go/cmd/compass-app/";
const GTK4_CLOSURE_PATHS = [
	"tools/toolchain/gtk-closure.nix",
	"tools/toolchain/gtk-e2e-env.nix",
];
/** Go module manifests: a dependency-only bump can change the Wails tree, which only the gtk4 lane vuln-scans. */
const GTK4_MODULE_PATHS = ["go/go.mod", "go/go.sum"];
/**
 * darwin trigger: the macos-14 lane compiles the SAME native shell the gtk4 lane
 * does, so it must fire on any go/cmd/compass-app/ change OR a shared GTK closure
 * input (mirror gtk4's surface), PLUS a change to the macos-bundle tool itself
 * (the bundler the lane exercises). The lane also cross-compiles the four pure-Go
 * sidecars, whose first-party package surface is enumerated in DARWIN_SIDECAR_PREFIXES below.
 * Per compass-distribution DL-263: affected on PR, full-sweep on push/schedule (isFullSweep below).
 */
const MACOS_BUNDLE_PATH_PREFIX = "tools/macos-bundle/";
/**
 * darwin sidecar surface: the mac lane now cross-compiles the four pure-Go
 * sidecars (compass-stack/server/runner/clear-token) beside the shell, so a change to any
 * first-party package they compile in must fire the darwin lane — else a darwin
 * cross-compile break (e.g. a go/internal/runtime change like #847) first
 * surfaces at the release cut. `go list -deps ./cmd/<sidecar>` resolves the
 * bundled binaries' non-cmd first-party roots to exactly {events, gen, internal,
 * server} (go/e2e is tests-only and excluded). We enumerate those package roots
 * as prefixes rather than a hand-maintained transitive file set (which would
 * silently drift and re-open the gap); each root is itself a deliberately broad
 * superset over its actual imported files. The module manifest (go/go.mod,
 * go/go.sum) is included alongside the roots: a dependency bump or `replace`
 * directive is a compile input to all four binaries that can break the darwin
 * cross-compile the same #847 way.
 */
const DARWIN_SIDECAR_PREFIXES = [
	// The .app stages the secretspec release binary this file pins.
	"tools/toolchain/secretspec-env.nix",
	"go/cmd/compass-stack/",
	"go/cmd/compass-server/",
	"go/cmd/compass-runner/",
	"go/cmd/compass-clear-token/",
	"go/internal/",
	"go/server/",
	"go/events/",
	"go/gen/",
	"go/go.mod",
	"go/go.sum",
];

// ── Pure core ──────────────────────────────────────────────────────────────

/**
 * Translate the affected closure + tags into the concern matrix + flags.
 * Pure; throws an Error naming the offending project id on assertion failure.
 */
function groupByProject(projects: readonly ProjectInput[]): {
	groupOf: Map<string, string>;
	groupUniverse: Set<string>;
	projectById: Map<string, ProjectInput>;
} {
	const groupOf = new Map<string, string>();
	const groupUniverse = new Set<string>();
	const projectById = new Map<string, ProjectInput>();
	for (const project of projects) {
		const groupTags = project.tags.filter((tag) =>
			tag.startsWith(CI_GROUP_PREFIX),
		);
		if (groupTags.length === 0)
			throw new Error(
				`ci-matrix: project '${project.id}' carries no ci-group.* tag`,
			);
		if (groupTags.length > 1) {
			throw new Error(
				`ci-matrix: project '${project.id}' carries multiple ci-group.* tags: ${groupTags.slice().sort().join(", ")}`,
			);
		}
		const groupTag = groupTags[0];
		if (groupTag === undefined)
			throw new Error(
				`ci-matrix: project '${project.id}' has no ci-group.* tag`,
			);
		const group = groupTag.slice(CI_GROUP_PREFIX.length);
		groupOf.set(project.id, group);
		groupUniverse.add(group);
		projectById.set(project.id, project);
	}
	return { groupOf, groupUniverse, projectById };
}

function targetsByGroup(
	input: GenInput,
	groupOf: ReadonlyMap<string, string>,
	groupUniverse: ReadonlySet<string>,
	projectById: ReadonlyMap<string, ProjectInput>,
): Map<string, string[]> {
	const targets = new Map<string, string[]>(
		[...groupUniverse].map((group) => [group, []]),
	);
	for (const id of input.affectedIds) {
		const group = groupOf.get(id);
		if (group === undefined)
			throw new Error(
				`ci-matrix: affected project '${id}' has no ci-group.* mapping (not in projects list)`,
			);
		const ciTarget = projectById.get(id)?.ciTarget ?? null;
		if (ciTarget !== null) targets.get(group)?.push(ciTarget);
	}
	return targets;
}

function matrixForGroups(
	groups: ReadonlySet<string>,
	targetsByGroup: ReadonlyMap<string, readonly string[]>,
): MatrixEntry[] {
	return [...groups].sort().map((group) => {
		const targets = [...(targetsByGroup.get(group) ?? [])].sort();
		return { group, run: targets.length > 0 ? "true" : "false", targets };
	});
}

function affectedFlags(
	input: GenInput,
): Pick<
	GenOutput,
	| "pgtestAffected"
	| "microvmAffected"
	| "e2eAffected"
	| "forgeAffected"
	| "gtk4Affected"
	| "darwinAffected"
	| "agentFodAffected"
> {
	const isFullSweep = input.event === "push" || input.event === "schedule";
	return {
		pgtestAffected: input.goSelection.pgtest.run,
		microvmAffected: input.goSelection.microvm.run,
		e2eAffected: input.goSelection.e2e.run,
		forgeAffected:
			isFullSweep ||
			input.changedPaths.some((path) => FORGE_PATH_RE.test(path)),
		gtk4Affected:
			isFullSweep ||
			input.changedPaths.some(
				(path) =>
					path.startsWith(GTK4_PATH_PREFIX) ||
					GTK4_CLOSURE_PATHS.includes(path) ||
					GTK4_MODULE_PATHS.includes(path),
			),
		darwinAffected:
			isFullSweep ||
			input.changedPaths.some(
				(path) =>
					path.startsWith(GTK4_PATH_PREFIX) ||
					GTK4_CLOSURE_PATHS.includes(path) ||
					path.startsWith(MACOS_BUNDLE_PATH_PREFIX) ||
					DARWIN_SIDECAR_PREFIXES.some((prefix) => path.startsWith(prefix)),
			),
		agentFodAffected:
			isFullSweep || (input.affectedTasks ?? []).includes(AGENT_FOD_TASK),
	};
}

/** Translate selections into the concern matrix and special-leg outputs. */
export function generate(input: GenInput): GenOutput {
	for (const suite of ["pgtest", "microvm"] as const) {
		if (
			input.goSelection[suite].run &&
			input.goSelection[suite].roots.length === 0
		) {
			throw new Error(`${suite} selection is running with no roots`);
		}
	}
	const { groupOf, groupUniverse, projectById } = groupByProject(
		input.projects,
	);
	const taskTargets = targetsByGroup(
		input,
		groupOf,
		groupUniverse,
		projectById,
	);
	return {
		matrix: matrixForGroups(groupUniverse, taskTargets),
		...affectedFlags(input),
		pgtestPackages: input.goSelection.pgtest.roots.map((root) => `./${root}`),
		microvmPackages: input.goSelection.microvm.roots.map((root) => `./${root}`),
		microvmRequiresSecretspec: input.goSelection.microvm.requiresSecretspec,
	};
}

// ── The edge (impure) ──────────────────────────────────────────────────────

/** The subset of `moon query projects` JSON the edge reads. */
type MoonProject = {
	id: string;
	config?: { tags?: string[] };
	taskTargets?: string[];
};

/** Parse a `moon query projects` payload into ProjectInput[]. */
function parseProjects(json: string): ProjectInput[] {
	const parsed = JSON.parse(json) as { projects?: MoonProject[] };
	const projects = parsed.projects ?? [];
	return projects.map((p) => {
		const tags = p.config?.tags ?? [];
		const ciTarget =
			(p.taskTargets ?? []).find((t) => t.endsWith(":ci")) ?? null;
		return { id: p.id, tags, ciTarget };
	});
}

/**
 * Project ids with at least one affected TASK.
 *
 * `projects --affected` walks the PROJECT graph — a project is affected when
 * its own sources or a project it `dependsOn` changed — and never consults a
 * project's cross-tree task `inputs`. A gate whose subject lives in other
 * projects' trees is therefore invisible to it: `orion-ref-gate`'s `check`
 * task declares a workspace-root recursive input glob so it scans the whole
 * repo, yet it is not selected unless its own handful of files change, so a
 * PR can introduce exactly what the gate exists to catch and never run it.
 *
 * `tasks --affected` intersects the PR's changed files with each task's real
 * `inputs`, so it sees those gates. The two closures are genuinely different
 * shapes rather than one containing the other: on an `apps/ui` path the task
 * closure is the narrower of the two, while on `/go/**` it additionally
 * selects `compass-app-bundle` — which the project walk can never reach
 * (`dependsOn: null`) even though the bundle declares `/go/**` as an input
 * precisely so a Go change reschedules it. A `.moon/**` edit widens furthest,
 * from two projects to all of them, since every task inherits that config.
 * That cost is the point: each addition is a project whose own declared
 * inputs the PR changed, and the pre-union behaviour was silently not
 * honouring those declarations. Unioning keeps the project graph's dependents
 * while adding the input-declared projects.
 *
 * `moon query tasks` prints its JSON envelope unconditionally: an unaffected
 * workspace yields `{"tasks": {}, …}`, not empty output, so an empty `.tasks`
 * is a real "nothing affected" rather than a failed query.
 */
export function parseTaskAffectedIds(json: string): string[] {
	const parsed = JSON.parse(json) as { tasks?: Record<string, unknown> | null };
	// `null` is checked alongside `undefined` so a null payload reports this
	// named diagnostic rather than a raw TypeError out of `Object.keys`.
	if (parsed.tasks == null) {
		throw new Error(
			"moon query tasks payload is missing the expected tasks key",
		);
	}
	const ids = Object.keys(parsed.tasks);
	const invalidId = ids.find((id) => id.includes(":"));
	if (invalidId !== undefined) {
		throw new Error(
			`moon query tasks returned an unexpected composite id: ${invalidId}`,
		);
	}
	return ids;
}

/** The `<project>:<task>` targets in a `moon query tasks --affected` payload. */
export function parseAffectedTaskTargets(json: string): string[] {
	const parsed = JSON.parse(json) as {
		tasks?: Record<string, Record<string, unknown>> | null;
	};
	return Object.entries(parsed.tasks ?? {}).flatMap(([project, tasks]) =>
		Object.keys(tasks).map((task) => `${project}:${task}`),
	);
}

/** Return the project closure plus known projects with affected tasks. */
export function unionAffectedIds(
	projectIds: readonly string[],
	taskIds: readonly string[],
	known: ReadonlySet<string>,
): string[] {
	return [
		...new Set([...projectIds, ...taskIds.filter((id) => known.has(id))]),
	];
}

/**
 * The `$GITHUB_OUTPUT` lines, one per key the `setup` job publishes.
 *
 * Every `*_affected` flag is required. Package values remain JSON arrays for
 * GitHub Actions' `fromJSON` consumer.
 */
export function outputLines(out: GenOutput): string[] {
	return [
		`matrix=${JSON.stringify(out.matrix)}`,
		`pgtest_affected=${out.pgtestAffected ? "true" : "false"}`,
		`pgtest_packages=${JSON.stringify(out.pgtestPackages)}`,
		`microvm_affected=${out.microvmAffected ? "true" : "false"}`,
		`microvm_packages=${JSON.stringify(out.microvmPackages)}`,
		`microvm_requires_secretspec=${out.microvmRequiresSecretspec ? "true" : "false"}`,
		`e2e_affected=${out.e2eAffected ? "true" : "false"}`,
		`forge_affected=${out.forgeAffected ? "true" : "false"}`,
		`gtk4_affected=${out.gtk4Affected ? "true" : "false"}`,
		`darwin_affected=${out.darwinAffected ? "true" : "false"}`,
		`agent_fod_affected=${out.agentFodAffected ? "true" : "false"}`,
	];
}

async function taskTargetsForPaths(
	paths: readonly string[],
): Promise<string[]> {
	if (paths.length === 0) return [];
	const child = Bun.spawn(["moon", "query", "tasks", "--affected"], {
		stdin: new Blob([`${paths.join("\n")}\n`]),
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(child.stdout).text(),
		new Response(child.stderr).text(),
		child.exited,
	]);
	if (exitCode !== 0) {
		throw new Error(`moon query tasks --affected failed: ${stderr.trim()}`);
	}
	return parseAffectedTaskTargets(stdout);
}

function suiteReasonLines(
	selection: Record<"pgtest" | "microvm" | "e2e", SuiteSelection>,
): string[] {
	return [
		`  pgtest: ${selection.pgtest.reason}`,
		`  microvm: ${selection.microvm.reason}`,
		`  e2e: ${selection.e2e.reason}`,
	];
}

/** Resolve PR-changed paths from GitHub's merge commit. */
export async function prChangedPaths(cwd = process.cwd()): Promise<string[]> {
	const mergeParent = await $`git rev-parse --verify --quiet HEAD^2`
		.cwd(cwd)
		.nothrow()
		.quiet();
	if (mergeParent.exitCode !== 0) {
		throw new Error(
			"pull_request checkout is not GitHub's merge ref: HEAD^2 does not resolve",
		);
	}
	const diff = await $`git diff --name-only HEAD^1 HEAD`
		.cwd(cwd)
		.quiet()
		.text();
	return diff
		.split("\n")
		.map((path) => path.trim())
		.filter((path) => path !== "");
}

/** Design records live in RigelBuild/docs; a PR may delete records here, never add or edit one. */
export async function prDesignWrites(cwd = process.cwd()): Promise<string[]> {
	const diff =
		await $`git diff --name-only --no-renames --diff-filter=d HEAD^1 HEAD -- docs/designs/`
			.cwd(cwd)
			.quiet()
			.text();
	return diff
		.split("\n")
		.map((path) => path.trim())
		.filter((path) => path !== "");
}

/** True when the checkout is a PR merge ref: a pull_request run or a base-repoint dispatch. */
export function checksPrMergeRef(
	eventName: string | undefined,
	dispatchPr: string | undefined,
): boolean {
	return (
		eventName === "pull_request" ||
		(eventName === "workflow_dispatch" && (dispatchPr ?? "") !== "")
	);
}

async function assertNoDesignWrites(): Promise<void> {
	if (
		!checksPrMergeRef(process.env.GITHUB_EVENT_NAME, process.env.CI_MATRIX_PR)
	)
		return;
	const writes = await prDesignWrites();
	if (writes.length > 0) {
		throw new Error(
			`design records live in RigelBuild/docs; this PR adds or edits ${writes.join(", ")}`,
		);
	}
}

async function prSelection(projects: readonly ProjectInput[]): Promise<{
	affectedIds: string[];
	affectedTasks: string[];
	changedPaths: string[];
	goSelection: Record<"pgtest" | "microvm" | "e2e", SuiteSelection>;
}> {
	const [affectedJson, taskJson, changedPaths] = await Promise.all([
		$`moon query projects --affected --upstream deep --downstream direct`
			.quiet()
			.text(),
		$`moon query tasks --affected`.quiet().text(),
		prChangedPaths(),
	]);
	const affectedParsed = JSON.parse(affectedJson) as {
		projects?: MoonProject[];
	};
	const projectsById = new Set(projects.map((project) => project.id));
	const affectedTasks = parseAffectedTaskTargets(taskJson);
	const affectedIds = unionAffectedIds(
		(affectedParsed.projects ?? []).map((project) => project.id),
		parseTaskAffectedIds(taskJson),
		projectsById,
	);
	const graph = await loadGoGraph(`${process.cwd()}/go`);
	const residueTasks = await taskTargetsForPaths(
		residuePaths(changedPaths, graph),
	);
	const goSelection = selectGoSuites({
		event: "pull_request",
		changedPaths,
		affectedIds,
		residueTasks,
		graph,
	});
	return { affectedIds, affectedTasks, changedPaths, goSelection };
}

async function fullSweepSelection(
	projects: readonly ProjectInput[],
	event: "push" | "schedule",
): Promise<{
	affectedIds: string[];
	affectedTasks: string[];
	changedPaths: string[];
	goSelection: Record<"pgtest" | "microvm" | "e2e", SuiteSelection>;
}> {
	const affectedIds = projects.map((project) => project.id);
	const changedPaths: string[] = [];
	const affectedTasks: string[] = [];
	const graph = await loadGoGraph(`${process.cwd()}/go`);
	const goSelection = selectGoSuites({
		event,
		changedPaths,
		affectedIds,
		residueTasks: [],
		graph,
	});
	return { affectedIds, affectedTasks, changedPaths, goSelection };
}
function reportMainError(err: unknown): void {
	const message = err instanceof Error ? err.message : String(err);
	const stderr = String(
		(err as { stderr?: unknown } | null)?.stderr ?? "",
	).trim();
	console.error(
		`::error::${stderr === "" ? message : `${message}: ${stderr}`}`,
	);
	process.exit(1);
}

function eventFromEnvironment(value: string | undefined): GenInput["event"] {
	if (value === "pull_request" || value === "schedule") return value;
	return "push";
}

async function runSelection(
	event: GenInput["event"],
	projects: ProjectInput[],
): Promise<{
	affectedIds: string[];
	affectedTasks: string[];
	changedPaths: string[];
	goSelection: GenInput["goSelection"];
}> {
	return event === "pull_request"
		? prSelection(projects)
		: fullSweepSelection(projects, event);
}

async function main(): Promise<void> {
	const event = eventFromEnvironment(process.env.GITHUB_EVENT_NAME);
	try {
		const projects = parseProjects(await $`moon query projects`.quiet().text());
		const selection = await runSelection(event, projects);
		const { affectedIds, affectedTasks, changedPaths, goSelection } = selection;
		await assertNoDesignWrites();
		const out = generate({
			projects,
			affectedIds,
			changedPaths,
			affectedTasks,
			goSelection,
			event,
		});
		const lines = outputLines(out);
		const githubOutput = process.env.GITHUB_OUTPUT;
		if (githubOutput != null && githubOutput !== "") {
			const file = Bun.file(githubOutput);
			const existing = (await file.exists()) ? await file.text() : "";
			await Bun.write(file, `${existing}${lines.join("\n")}\n`);
		}
		console.log(`ci-matrix (${event}):`);
		for (const entry of out.matrix) {
			const targets =
				entry.targets.length > 0 ? entry.targets.join(" ") : "(none)";
			console.log(
				`  [${entry.run === "true" ? "run " : "skip"}] ${entry.group}: ${targets}`,
			);
		}
		for (const reason of suiteReasonLines(goSelection)) console.log(reason);
		console.log(
			`  flags: pgtest=${out.pgtestAffected} microvm=${out.microvmAffected} e2e=${out.e2eAffected} forge=${out.forgeAffected} gtk4=${out.gtk4Affected} darwin=${out.darwinAffected} agent-fod=${out.agentFodAffected}`,
		);
	} catch (err) {
		reportMainError(err);
	}
}

if (import.meta.main) {
	await main();
}
