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
	event: "pull_request" | "push" | "schedule";
};

export type MatrixEntry = {
	/** "go" | "bun" | "nix" | "forks" (the tag suffix) */
	group: string;
	/** 'true' iff >=1 affected member in this group (always 'true' on push/schedule) */
	run: "true" | "false";
	/** affected members' ciTarget (sorted); [] when run==='false' */
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
	/** push/schedule OR changedPaths has any path under go/cmd/compass-app/ or a shared GTK closure input */
	gtk4Affected: boolean;
	/** push/schedule OR changedPaths touches the darwin shell surface (the gtk4 closure — the mac lane compiles the SAME shell), the macos-bundle tool, or the sidecar surface (three sidecars plus shared go/internal/) */
	darwinAffected: boolean;
};

// ── Pure-core constants ────────────────────────────────────────────────────

/** The prefix stripped to get the group name: tag is `ci-group.<group>`. */
const CI_GROUP_PREFIX = "ci-group.";

/** Special-leg project ids the flags key off. */
const PGTEST_PROJECT = "compass-go";
const GUEST_IMAGE_PROJECT = "compass-guest-image";

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
 */
const FORGE_PATH_RE = /^go\/internal\/forge\//;
/**
 * gtk4 trigger: any changed path under go/cmd/compass-app/, OR one of the
 * shared GTK closure inputs. The e2e lane is the ONLY CI lane that compiles the
 * native shell, so a closure-only change (e.g. the T2 atk/gdk-pixbuf trim) must
 * still trigger it — keying on the app path prefix alone would skip it (F2).
 */
const GTK4_PATH_PREFIX = "go/cmd/compass-app/";
const GTK4_CLOSURE_PATHS = [
	"tools/toolchain/gtk-closure.nix",
	"tools/toolchain/gtk-e2e-env.nix",
];
/**
 * darwin trigger: the macos-14 lane compiles the SAME native shell the gtk4 lane
 * does, so it must fire on any go/cmd/compass-app/ change OR a shared GTK closure
 * input (mirror gtk4's surface), PLUS a change to the macos-bundle tool itself
 * (the bundler the lane exercises). The lane also cross-compiles the three pure-Go
 * sidecars, whose first-party package surface is enumerated in DARWIN_SIDECAR_PREFIXES below.
 * Per compass-distribution DL-263: affected on PR, full-sweep on push/schedule (isFullSweep below).
 */
const MACOS_BUNDLE_PATH_PREFIX = "tools/macos-bundle/";
/**
 * darwin sidecar surface: the mac lane now cross-compiles the three pure-Go
 * sidecars (compass-stack/server/runner) beside the shell, so a change to any
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
	"go/cmd/compass-stack/",
	"go/cmd/compass-server/",
	"go/cmd/compass-runner/",
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
			(p) => p.startsWith(GTK4_PATH_PREFIX) || GTK4_CLOSURE_PATHS.includes(p),
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

	return {
		matrix,
		pgtestAffected,
		microvmAffected,
		forgeAffected,
		gtk4Affected,
		darwinAffected,
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
 * projects' trees is therefore invisible to it: `orion-ref-gate` scans the
 * whole repo via `/**` + `/*`, `design-ledger-gate` reads `/docs/designs/**`,
 * yet neither is selected unless its own handful of files change, so a PR can
 * introduce exactly what the gate exists to catch and never run it.
 *
 * `tasks --affected` intersects the PR's changed files with each task's real
 * `inputs`, so it sees those gates. It is not a blanket widening — it is
 * narrower than the project walk on most paths, because it keys on declared
 * inputs rather than on graph reachability. Unioning the two closures keeps
 * the project graph's dependents while adding the input-declared gates.
 *
 * `moon query tasks` prints its JSON envelope unconditionally: an unaffected
 * workspace yields `{"tasks": {}, …}`, not empty output, so an empty `.tasks`
 * is a real "nothing affected" rather than a failed query.
 */
export function parseTaskAffectedIds(json: string): string[] {
	const parsed = JSON.parse(json) as { tasks?: Record<string, unknown> };
	return Object.keys(parsed.tasks ?? {});
}

async function main(): Promise<void> {
	const eventNameRaw = process.env.GITHUB_EVENT_NAME ?? "push";
	const event: GenInput["event"] =
		eventNameRaw === "pull_request"
			? "pull_request"
			: eventNameRaw === "schedule"
				? "schedule"
				: "push";

	// Full set: group universe + tags + ci-task presence.
	const fullJson = await $`moon query projects`.quiet().text();
	const projects = parseProjects(fullJson);

	// Affected set: on a PR, the union of the project closure and the
	// task-level closure (see parseTaskAffectedIds for why the project walk
	// alone misses cross-tree gates); else the full set.
	let affectedIds: string[];
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
		affectedIds = [
			...new Set([
				...(affectedParsed.projects ?? []).map((p) => p.id),
				// Intersected with the known set: the pure core throws on an id
				// it cannot resolve to a ci-group tag, so a task-only id from a
				// project missing from `moon query projects` must not leak in.
				...parseTaskAffectedIds(taskJson).filter((id) => known.has(id)),
			]),
		];
	} else {
		affectedIds = projects.map((p) => p.id);
	}

	// Changed paths for forge/gtk4 detection (PR only; unused on push/schedule
	// where the flags are unconditionally true).
	let changedPaths: string[] = [];
	if (event === "pull_request") {
		const baseRef = process.env.GITHUB_BASE_REF ?? "";
		if (baseRef !== "") {
			const diff = await $`git diff --name-only origin/${baseRef}...HEAD`
				.nothrow()
				.quiet()
				.text();
			changedPaths = diff
				.split("\n")
				.map((l) => l.trim())
				.filter((l) => l !== "");
		}
	}

	try {
		const out = generate({ projects, affectedIds, changedPaths, event });

		const lines = [
			`matrix=${JSON.stringify(out.matrix)}`,
			`pgtest_affected=${out.pgtestAffected ? "true" : "false"}`,
			`microvm_affected=${out.microvmAffected ? "true" : "false"}`,
			`forge_affected=${out.forgeAffected ? "true" : "false"}`,
			`gtk4_affected=${out.gtk4Affected ? "true" : "false"}`,
			`darwin_affected=${out.darwinAffected ? "true" : "false"}`,
		];

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
			`  flags: pgtest=${out.pgtestAffected} microvm=${out.microvmAffected} forge=${out.forgeAffected} gtk4=${out.gtk4Affected} darwin=${out.darwinAffected}`,
		);
	} catch (err) {
		const message = err instanceof Error ? err.message : String(err);
		console.error(`::error::${message}`);
		process.exit(1);
	}
}

if (import.meta.main) {
	await main();
}
