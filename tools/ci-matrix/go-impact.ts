import type { GenInput } from "./index.ts";

export type GoSuite = "pgtest" | "microvm" | "e2e";

export type GoGraph = {
	/** Module-relative dirs of every package in `go list ./...`. */
	packages: readonly string[];
	/** Per suite and root: module-relative dirs in its test dependency closure. */
	closure: Readonly<
		Record<GoSuite, Readonly<Record<string, readonly string[]>>>
	>;
	/** Roots whose `go list` entry carried a non-empty `.Error`. */
	erroredRoots: Readonly<Record<GoSuite, readonly string[]>>;
};

export type SuiteSelection = {
	run: boolean;
	reason: string;
	roots: readonly string[];
	requiresSecretspec: boolean;
};

export const GATE_PATHS: readonly string[] = [
	".github/workflows/ci.yml",
	"tools/ci-matrix/",
	".moon/",
];

export const E2E_ROOTS: readonly string[] = ["e2e", "cmd/compass-stack"];

export const E2E_BUILT_BINARIES: readonly string[] = [
	// Both go build loops compile only these three; compass-stack remains an E2E root.
	"cmd/compass-postgres",
	"cmd/compass-runner",
	"cmd/compass-server",
];

export const SUITE_OWNER_TASKS: Readonly<Record<GoSuite, readonly string[]>> = {
	pgtest: ["compass-go:test", "toolchain-parity:parity", "compass-go:sqlc-vet"],
	microvm: [
		"compass-go:test",
		"toolchain-parity:parity",
		"compass-guest-image:build",
	],
	e2e: [
		"compass-go:test",
		"toolchain-parity:parity",
		"compass-agent-image:build",
		"toolchain-parity:test",
	],
};

function packageForPath(
	changedPath: string,
	graph: GoGraph,
): string | undefined {
	if (!changedPath.startsWith("go/")) return undefined;
	const modulePath = changedPath.slice("go/".length);
	return graph.packages
		.filter(
			(pkg) =>
				pkg === "." || modulePath === pkg || modulePath.startsWith(`${pkg}/`),
		)
		.sort((left, right) => right.length - left.length)[0];
}

function allRoots(suite: GoSuite, graph: GoGraph): string[] {
	if (suite === "e2e") return [...E2E_ROOTS].sort();
	return Object.keys(graph.closure[suite]).sort();
}

function requiresSecretspec(
	suite: GoSuite,
	roots: readonly string[],
	graph: GoGraph,
): boolean {
	return roots.some((root) =>
		graph.closure[suite][root]?.includes("internal/secrets"),
	);
}

function selectedSuite(
	suite: GoSuite,
	graph: GoGraph,
	selection: Omit<SuiteSelection, "requiresSecretspec">,
): SuiteSelection {
	return {
		...selection,
		requiresSecretspec: requiresSecretspec(suite, selection.roots, graph),
	};
}

function wideSelection(
	suite: GoSuite,
	graph: GoGraph,
	reason: string,
): SuiteSelection {
	return selectedSuite(suite, graph, {
		run: true,
		reason,
		roots: allRoots(suite, graph),
	});
}

/** Changed paths that do not map to a Go package. */
export function residuePaths(
	changedPaths: readonly string[],
	graph: GoGraph,
): string[] {
	return [
		...new Set(
			changedPaths.filter(
				(changedPath) => packageForPath(changedPath, graph) === undefined,
			),
		),
	].sort();
}

function selectSuite(
	suite: GoSuite,
	input: {
		readonly changedPaths: readonly string[];
		readonly affectedIds: readonly string[];
		readonly residueTasks: readonly string[];
		readonly graph: GoGraph;
	},
): SuiteSelection {
	if (input.graph.erroredRoots[suite].length > 0) {
		return wideSelection(suite, input.graph, "wide: go list error");
	}
	if (
		suite === "microvm" &&
		input.affectedIds.includes("compass-guest-image")
	) {
		return wideSelection(suite, input.graph, "wide: compass-guest-image");
	}
	const owner = SUITE_OWNER_TASKS[suite].find((target) =>
		input.residueTasks.includes(target),
	);
	if (owner !== undefined)
		return wideSelection(suite, input.graph, `owner: ${owner}`);
	const changedPackages = [
		...new Set(
			input.changedPaths
				.map((changedPath) => packageForPath(changedPath, input.graph))
				.filter((pkg) => pkg !== undefined),
		),
	].sort();
	const hits = Object.entries(input.graph.closure[suite]).flatMap(
		([root, closure]) =>
			changedPackages
				.filter((pkg) => closure.includes(pkg))
				.map((pkg) => ({ root, pkg })),
	);
	if (hits.length === 0)
		return selectedSuite(suite, input.graph, {
			run: false,
			reason: "none",
			roots: [],
		});
	const reason = `closure: ${[...new Set(hits.map((hit) => hit.pkg))].sort().join(", ")}`;
	const roots =
		suite === "e2e"
			? [...E2E_ROOTS].sort()
			: [...new Set(hits.map((hit) => hit.root))].sort();
	return selectedSuite(suite, input.graph, { run: true, reason, roots });
}

function gateChange(changedPaths: readonly string[]): string | undefined {
	return changedPaths.find((changedPath) =>
		GATE_PATHS.some((gate) =>
			gate.endsWith("/") ? changedPath.startsWith(gate) : changedPath === gate,
		),
	);
}

function unmappedGoChange(
	changedPaths: readonly string[],
	graph: GoGraph,
): string | undefined {
	return changedPaths.find(
		(changedPath) =>
			changedPath.startsWith("go/") &&
			packageForPath(changedPath, graph) === undefined,
	);
}

function selectAll(
	graph: GoGraph,
	reason: string,
): Record<GoSuite, SuiteSelection> {
	const selection = (suite: GoSuite): SuiteSelection =>
		wideSelection(suite, graph, reason);
	return {
		pgtest: selection("pgtest"),
		microvm: selection("microvm"),
		e2e: selection("e2e"),
	};
}

export function selectGoSuites(input: {
	event: GenInput["event"];
	changedPaths: readonly string[];
	affectedIds: readonly string[];
	residueTasks: readonly string[];
	graph: GoGraph;
}): Record<GoSuite, SuiteSelection> {
	if (input.event === "push" || input.event === "schedule")
		return selectAll(input.graph, "wide: full sweep");
	const gate = gateChange(input.changedPaths);
	if (gate !== undefined) return selectAll(input.graph, `wide: ${gate}`);
	const unmappedGo = unmappedGoChange(input.changedPaths, input.graph);
	if (unmappedGo !== undefined)
		return selectAll(input.graph, `wide: ${unmappedGo}`);
	return {
		pgtest: selectSuite("pgtest", input),
		microvm: selectSuite("microvm", input),
		e2e: selectSuite("e2e", input),
	};
}
