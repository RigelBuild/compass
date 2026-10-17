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
	/** ALL workspace projects (for the group universe + assertions) */
	projects: ProjectInput[];
	/** closure members (PR) or all ids (push/schedule) */
	affectedIds: string[];
	/** PR changed paths (for forge/gtk4 detection); [] on push/schedule */
	changedPaths: string[];
	/** `<project>:<task>` targets from `moon query tasks --affected` (PR); unused on push/schedule */
	affectedTasks?: string[];
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
	/** closure contains compass-go */
	pgtestAffected: boolean;
	/** closure contains compass-go OR compass-guest-image */
	microvmAffected: boolean;
	/** push/schedule OR changedPaths matches forge trigger */
	forgeAffected: boolean;
	/** push/schedule OR changedPaths has any path under go/cmd/compass-app/, a shared GTK closure input, or go/go.mod / go/go.sum */
	gtk4Affected: boolean;
	/** push/schedule OR changedPaths touches the darwin shell surface (the gtk4 closure — the mac lane compiles the SAME shell), the macos-bundle tool, or the sidecar surface (four sidecars plus shared go/internal/) */
	darwinAffected: boolean;
	/** push/schedule OR the PR affects AGENT_FOD_TASK (its declared inputs changed) */
	agentFodAffected: boolean;
};

// ── Pure-core constants ────────────────────────────────────────────────────

/** The prefix stripped to get the group name: tag is `ci-group.<group>`. */
const CI_GROUP_PREFIX = "ci-group.";

/** Special-leg project ids the flags key off. */
const PGTEST_PROJECT = "compass-go";
const GUEST_IMAGE_PROJECT = "compass-guest-image";

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
export function generate(input: GenInput): GenOutput {
	const isFullSweep = input.event === "push" || input.event === "schedule";

	// The group of each project, keyed by id. Enforces zero-untagged +
	// disjointness while building it (each throws, naming the offending id).
	const groupOf = new Map<string, string>();
	// The group universe: every group appearing as a ci-group.* tag on any
	// project. Placeholder anchor — the matrix has one entry per existing group.
	const groupUniverse = new Set<string>();

	for (const project of input.projects) {
		const groupTags = project.tags.filter((t) => t.startsWith(CI_GROUP_PREFIX));
		if (groupTags.length === 0) {
			throw new Error(
				`ci-matrix: project '${project.id}' carries no ci-group.* tag`,
			);
		}
		if (groupTags.length > 1) {
			throw new Error(
				`ci-matrix: project '${project.id}' carries multiple ci-group.* tags: ${groupTags
					.slice()
					.sort()
					.join(", ")}`,
			);
		}
		// biome-ignore lint/style/noNonNullAssertion: length checked === 1 above.
		const group = groupTags[0]!.slice(CI_GROUP_PREFIX.length);
		groupOf.set(project.id, group);
		groupUniverse.add(group);
	}

	// Index projects by id for ciTarget lookup during target collection.
	const projectById = new Map<string, ProjectInput>();
	for (const project of input.projects) {
		projectById.set(project.id, project);
	}

	const affectedSet = new Set(input.affectedIds);

	// Collect, per group, the ci targets of affected members that have a
	// non-null ciTarget. A null-ciTarget member is skipped (never emit a bare
	// `<id>:ci` for a project without the task).
	const targetsByGroup = new Map<string, string[]>();
	for (const group of groupUniverse) {
		targetsByGroup.set(group, []);
	}
	for (const id of input.affectedIds) {
		const group = groupOf.get(id);
		// coverage: an affected id with no group means it is not in `projects`.
		// The group universe is built from all projects, so this can only happen
		// if the caller passed an affectedId absent from `projects`.
		if (group === undefined) {
			throw new Error(
				`ci-matrix: affected project '${id}' has no ci-group.* mapping (not in projects list)`,
			);
		}
		const project = projectById.get(id);
		const ciTarget = project?.ciTarget ?? null;
		if (ciTarget !== null) {
			// biome-ignore lint/style/noNonNullAssertion: group is in groupUniverse.
			targetsByGroup.get(group)!.push(ciTarget);
		}
	}

	// Build the matrix: one entry per existing group, sorted by group name.
	// group == tag-suffix and groupUniverse is a Set, so group names are unique
	// by construction — the emitted check names cannot collide.
	const matrix: MatrixEntry[] = [];
	for (const group of [...groupUniverse].sort()) {
		// biome-ignore lint/style/noNonNullAssertion: group is in groupUniverse.
		const targets = targetsByGroup.get(group)!.slice().sort();
		// A group runs iff it has >=1 runnable target. On a full sweep every id
		// is affected, so every group with a ci task runs; on a PR only groups
		// with an affected, ci-task-bearing member run. Deriving `run` from
		// `targets` (not mere membership) upholds the ci.yml invariant that a
		// running leg always has >=1 target, so `moon run` is never targetless:
		// a group whose only affected member has no ci task emits run:'false'.
		const run: "true" | "false" = targets.length > 0 ? "true" : "false";
		matrix.push({
			group,
			run,
			targets,
		});
	}

	const pgtestAffected = affectedSet.has(PGTEST_PROJECT);
	const microvmAffected =
		affectedSet.has(PGTEST_PROJECT) || affectedSet.has(GUEST_IMAGE_PROJECT);
	const forgeAffected =
		isFullSweep || input.changedPaths.some((p) => FORGE_PATH_RE.test(p));
	const gtk4Affected =
		isFullSweep ||
		input.changedPaths.some(
			(p) =>
				p.startsWith(GTK4_PATH_PREFIX) ||
				GTK4_CLOSURE_PATHS.includes(p) ||
				GTK4_MODULE_PATHS.includes(p),
		);
	const darwinAffected =
		isFullSweep ||
		input.changedPaths.some(
			(p) =>
				p.startsWith(GTK4_PATH_PREFIX) ||
				GTK4_CLOSURE_PATHS.includes(p) ||
				p.startsWith(MACOS_BUNDLE_PATH_PREFIX) ||
				DARWIN_SIDECAR_PREFIXES.some((prefix) => p.startsWith(prefix)),
		);
	const agentFodAffected =
		isFullSweep || (input.affectedTasks ?? []).includes(AGENT_FOD_TASK);

	return {
		matrix,
		pgtestAffected,
		microvmAffected,
		forgeAffected,
		gtk4Affected,
		darwinAffected,
		agentFodAffected,
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
 * project's cross-tree task `inputs`. `ref-gates` uses workspace-root inputs, so
 * only `tasks --affected` selects it when other files change.
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
 * Every `*_affected` flag is required: `ci.yml`'s rollup accepts a skipped
 * work job only when its paired flag reads exactly `false`, so an omitted key
 * arrives as the empty string and reds the rollup with `result=skipped,
 * affected flag=`. That is fail-closed, but it fails on the *next* PR rather
 * than on the change that dropped the key, so the set is built here and
 * asserted in the tests rather than inlined at the callsite.
 */
export function outputLines(out: GenOutput): string[] {
	return [
		`matrix=${JSON.stringify(out.matrix)}`,
		`pgtest_affected=${out.pgtestAffected ? "true" : "false"}`,
		`microvm_affected=${out.microvmAffected ? "true" : "false"}`,
		`forge_affected=${out.forgeAffected ? "true" : "false"}`,
		`gtk4_affected=${out.gtk4Affected ? "true" : "false"}`,
		`darwin_affected=${out.darwinAffected ? "true" : "false"}`,
		`agent_fod_affected=${out.agentFodAffected ? "true" : "false"}`,
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

async function main(): Promise<void> {
	const eventNameRaw = process.env.GITHUB_EVENT_NAME ?? "push";
	const event: GenInput["event"] =
		eventNameRaw === "pull_request"
			? "pull_request"
			: eventNameRaw === "schedule"
				? "schedule"
				: "push";

	try {
		// Full set: group universe + tags + ci-task presence.
		const fullJson = await $`moon query projects`.quiet().text();
		const projects = parseProjects(fullJson);

		// Affected set: on a PR, the union of the project closure and the
		// task-level closure (see parseTaskAffectedIds for why the project walk
		// alone misses cross-tree gates); else the full set.
		let affectedIds: string[];
		let affectedTasks: string[] = [];
		if (event === "pull_request") {
			const [affectedJson, taskJson] = await Promise.all([
				$`moon query projects --affected --upstream deep --downstream direct`
					.quiet()
					.text(),
				$`moon query tasks --affected`.quiet().text(),
			]);
			const affectedParsed = JSON.parse(affectedJson) as {
				projects?: MoonProject[];
			};
			const known = new Set(projects.map((p) => p.id));
			affectedIds = unionAffectedIds(
				(affectedParsed.projects ?? []).map((p) => p.id),
				parseTaskAffectedIds(taskJson),
				known,
			);
			affectedTasks = parseAffectedTaskTargets(taskJson);
		} else {
			affectedIds = projects.map((p) => p.id);
		}

		// Changed paths for forge/gtk4 detection (PR only; unused on push/schedule
		// where the flags are unconditionally true).
		const changedPaths = event === "pull_request" ? await prChangedPaths() : [];
		await assertNoDesignWrites();

		const out = generate({
			projects,
			affectedIds,
			changedPaths,
			affectedTasks,
			event,
		});

		const lines = outputLines(out);

		const githubOutput = process.env.GITHUB_OUTPUT;
		if (githubOutput != null && githubOutput !== "") {
			const existing = (await Bun.file(githubOutput).exists())
				? await Bun.file(githubOutput).text()
				: "";
			await Bun.write(
				Bun.file(githubOutput),
				`${existing}${lines.join("\n")}\n`,
			);
		}

		// Human summary.
		console.log(`ci-matrix (${event}):`);
		for (const entry of out.matrix) {
			console.log(
				`  [${entry.run === "true" ? "run " : "skip"}] ${entry.group}: ${
					entry.targets.length > 0 ? entry.targets.join(" ") : "(none)"
				}`,
			);
		}
		console.log(
			`  flags: pgtest=${out.pgtestAffected} microvm=${out.microvmAffected} forge=${out.forgeAffected} gtk4=${out.gtk4Affected} darwin=${out.darwinAffected} agent-fod=${out.agentFodAffected}`,
		);
	} catch (err) {
		// A failed `moon query` throws a Bun ShellError whose `message` is only
		// "Failed with exit code N" — moon's own diagnostic (the part naming
		// the cause) rides on a separate `stderr` Buffer. Two queries now run
		// concurrently, so the bare message cannot even say which one failed;
		// append the stderr when present so a CI operator sees the cause.
		const message = err instanceof Error ? err.message : String(err);
		const stderr = String(
			(err as { stderr?: unknown } | null)?.stderr ?? "",
		).trim();
		console.error(
			`::error::${stderr === "" ? message : `${message}: ${stderr}`}`,
		);
		process.exit(1);
	}
}

if (import.meta.main) {
	await main();
}
