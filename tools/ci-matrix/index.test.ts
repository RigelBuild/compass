// Unit tests for the ci-matrix pure core (index.ts).
//
// These defend the generator's contract (.t5-contract.md § The generator):
// coverage, disjointness-throws-naming-id, zero-untagged-throws-naming-id, the
// placeholder anchor for unaffected groups, determinism under shuffled input,
// all four affected-flag rules, a null-ciTarget member being skipped from
// targets, and an empty affected closure still yielding a non-empty matrix.
//
// Only the PURE core is exercised — the edge (moon query / $GITHUB_OUTPUT) is
// import.meta.main-guarded, so importing index.ts never runs it.

import { describe, expect, test } from "bun:test";
import {
	type GenInput,
	generate,
	outputLines,
	type ProjectInput,
	parseTaskAffectedIds,
	unionAffectedIds,
} from "./index.ts";

/** A grouped project with a `ci` task target derived from its id. */
function proj(id: string, group: string): ProjectInput {
	return { id, tags: [`ci-group.${group}`], ciTarget: `${id}:ci` };
}

/** The canonical four-group workspace used by most tests. */
function workspace(): ProjectInput[] {
	return [
		proj("compass-go", "go"),
		proj("compass-proto", "go"),
		proj("compass-agent", "bun"),
		proj("ci-matrix", "bun"),
		proj("compass-guest-image", "nix"),
		proj("oh-my-pi-fork", "forks"),
	];
}

/** A PR-shaped input over the canonical workspace. */
function prInput(
	over: Partial<GenInput> & { affectedIds: string[] },
): GenInput {
	return {
		projects: workspace(),
		changedPaths: [],
		event: "pull_request",
		...over,
	};
}

describe("coverage — an affected member lands in exactly its group entry", () => {
	test("an affected go member fills the go entry's targets, others stay placeholders", () => {
		const out = generate(prInput({ affectedIds: ["compass-go"] }));
		const go = out.matrix.find((e) => e.group === "go");
		expect(go).toEqual({
			group: "go",
			run: "true",
			targets: ["compass-go:ci"],
		});
		// Every other group is a placeholder.
		for (const other of ["bun", "forks", "nix"]) {
			expect(out.matrix.find((e) => e.group === other)).toEqual({
				group: other,
				run: "false",
				targets: [],
			});
		}
	});

	test("two affected members of the same group both appear, sorted", () => {
		const out = generate(
			prInput({ affectedIds: ["compass-proto", "compass-go"] }),
		);
		expect(out.matrix.find((e) => e.group === "go")?.targets).toEqual([
			"compass-go:ci",
			"compass-proto:ci",
		]);
	});

	test("affected members spread across groups each land in their own entry", () => {
		const out = generate(
			prInput({
				affectedIds: ["compass-go", "compass-agent", "oh-my-pi-fork"],
			}),
		);
		const byGroup = new Map(out.matrix.map((e) => [e.group, e]));
		expect(byGroup.get("go")?.run).toBe("true");
		expect(byGroup.get("bun")?.run).toBe("true");
		expect(byGroup.get("forks")?.run).toBe("true");
		expect(byGroup.get("nix")?.run).toBe("false");
		expect(byGroup.get("bun")?.targets).toEqual(["compass-agent:ci"]);
	});
});

describe("disjointness — a project with two ci-group.* tags throws, naming it", () => {
	test("throws an Error whose message names the offending id", () => {
		const projects = workspace();
		projects.push({
			id: "double-tagged",
			tags: ["ci-group.go", "ci-group.bun"],
			ciTarget: "double-tagged:ci",
		});
		expect(() =>
			generate({
				projects,
				affectedIds: [],
				changedPaths: [],
				event: "pull_request",
			}),
		).toThrow(/double-tagged/);
	});

	test("the message includes both offending tags", () => {
		const projects: ProjectInput[] = [
			{
				id: "double-tagged",
				tags: ["ci-group.bun", "ci-group.go"],
				ciTarget: null,
			},
		];
		expect(() =>
			generate({
				projects,
				affectedIds: [],
				changedPaths: [],
				event: "pull_request",
			}),
		).toThrow(/ci-group\.bun.*ci-group\.go|ci-group\.go.*ci-group\.bun/);
	});
});

describe("zero-untagged — a project with no ci-group.* tag throws, naming it", () => {
	test("throws an Error whose message names the offending id", () => {
		const projects = workspace();
		projects.push({ id: "untagged", tags: ["bun"], ciTarget: "untagged:ci" });
		expect(() =>
			generate({
				projects,
				affectedIds: [],
				changedPaths: [],
				event: "pull_request",
			}),
		).toThrow(/untagged/);
	});

	test("a project with an empty tags list throws too", () => {
		const projects: ProjectInput[] = [{ id: "bare", tags: [], ciTarget: null }];
		expect(() =>
			generate({
				projects,
				affectedIds: [],
				changedPaths: [],
				event: "pull_request",
			}),
		).toThrow(/bare/);
	});
});

describe("placeholder anchor — one entry per existing group, unaffected are placeholders", () => {
	test("every existing group is present even with no affected members", () => {
		const out = generate(prInput({ affectedIds: [] }));
		expect(out.matrix.map((e) => e.group)).toEqual([
			"bun",
			"forks",
			"go",
			"nix",
		]);
		for (const e of out.matrix) {
			expect(e.run).toBe("false");
			expect(e.targets).toEqual([]);
		}
	});

	test("a group that appears on no project is absent from the matrix", () => {
		// No 'forks' project here → no forks entry.
		const projects = [proj("compass-go", "go"), proj("compass-agent", "bun")];
		const out = generate({
			projects,
			affectedIds: ["compass-go"],
			changedPaths: [],
			event: "pull_request",
		});
		expect(out.matrix.map((e) => e.group)).toEqual(["bun", "go"]);
	});
});

describe("determinism — shuffled input yields identical sorted output", () => {
	test("matrix sorted by group; targets sorted; byte-identical output", () => {
		const ordered = generate(
			prInput({
				affectedIds: ["compass-go", "compass-proto", "compass-agent"],
			}),
		);

		const shuffledProjects = [...workspace()].reverse();
		const shuffled = generate({
			projects: shuffledProjects,
			affectedIds: ["compass-agent", "compass-proto", "compass-go"],
			changedPaths: [],
			event: "pull_request",
		});

		expect(JSON.stringify(shuffled)).toBe(JSON.stringify(ordered));
		expect(ordered.matrix.map((e) => e.group)).toEqual([
			"bun",
			"forks",
			"go",
			"nix",
		]);
		expect(ordered.matrix.find((e) => e.group === "go")?.targets).toEqual([
			"compass-go:ci",
			"compass-proto:ci",
		]);
	});
});

describe("flags — pgtest / microvm / forge / gtk4 rules", () => {
	test("pgtestAffected iff compass-go in closure", () => {
		expect(
			generate(prInput({ affectedIds: ["compass-go"] })).pgtestAffected,
		).toBe(true);
		expect(
			generate(prInput({ affectedIds: ["compass-agent"] })).pgtestAffected,
		).toBe(false);
	});

	test("microvmAffected iff compass-go OR compass-guest-image in closure", () => {
		expect(
			generate(prInput({ affectedIds: ["compass-go"] })).microvmAffected,
		).toBe(true);
		expect(
			generate(prInput({ affectedIds: ["compass-guest-image"] }))
				.microvmAffected,
		).toBe(true);
		expect(
			generate(prInput({ affectedIds: ["compass-agent"] })).microvmAffected,
		).toBe(false);
	});

	test("forgeAffected on go/internal/forge/ path match", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/internal/forge/oracle.go"],
				}),
			).forgeAffected,
		).toBe(true);
	});

	test("forgeAffected on a testdata fixture change (still the forge surface)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/internal/forge/testdata/linear/create_issue.json"],
				}),
			).forgeAffected,
		).toBe(true);
	});

	test("forgeAffected NOT triggered by a ci.yml-only PR (RIG-2909)", () => {
		// The live oracle is the expensive extra verification on top of the
		// untagged golden-replay battery; a PR that only touches this workflow
		// file (or any other CI/docs-only change) must not run it — that
		// over-trigger flaked unrelated PRs on a Linear API blip. Oracle-wiring
		// changes are re-verified by the push/schedule full sweep instead.
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: [".github/workflows/ci.yml"],
				}),
			).forgeAffected,
		).toBe(false);
		// A different workflow file must not trigger forge either.
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: [".github/workflows/release.yml"],
				}),
			).forgeAffected,
		).toBe(false);
	});

	test("forgeAffected false when no path matches on a PR", () => {
		expect(
			generate(prInput({ affectedIds: [], changedPaths: ["docs/readme.md"] }))
				.forgeAffected,
		).toBe(false);
	});

	test("gtk4Affected on any go/cmd/compass-app/ path", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/cmd/compass-app/main.go"],
				}),
			).gtk4Affected,
		).toBe(true);
		expect(
			generate(
				prInput({ affectedIds: [], changedPaths: ["go/cmd/other/main.go"] }),
			).gtk4Affected,
		).toBe(false);
	});

	test("gtk4Affected on a shared GTK closure input (F2 trigger extension)", () => {
		// A closure-only PR (the T2 atk/gdk-pixbuf trim) touches no
		// go/cmd/compass-app/ path, but the e2e lane is the ONLY lane that
		// compiles the shell against that closure — so it MUST still trigger.
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/toolchain/gtk-closure.nix"],
				}),
			).gtk4Affected,
		).toBe(true);
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/toolchain/gtk-e2e-env.nix"],
				}),
			).gtk4Affected,
		).toBe(true);
		// A different toolchain nix file must NOT trigger it.
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/toolchain/gate-tools.nix"],
				}),
			).gtk4Affected,
		).toBe(false);
	});

	test("darwinAffected on any go/cmd/compass-app/ path (same shell as gtk4)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/cmd/compass-app/main.go"],
				}),
			).darwinAffected,
		).toBe(true);
		expect(
			generate(
				prInput({ affectedIds: [], changedPaths: ["go/cmd/other/main.go"] }),
			).darwinAffected,
		).toBe(false);
	});

	test("darwinAffected on a shared GTK closure input (mac lane compiles the same shell)", () => {
		// The macos-14 lane compiles the SAME native shell the gtk4 lane does, so
		// a closure-only PR must trigger it too (mirror gtk4's surface).
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/toolchain/gtk-closure.nix"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a tools/macos-bundle/ change (the bundler the lane exercises)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/macos-bundle/index.ts"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a sidecar cmd path (the mac lane now compiles the sidecars)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/cmd/compass-stack/main.go"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a shared go/internal change (transitive sidecar input — the #847 class)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/internal/runtime/microvm/launch.go"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a go/server change (unix-tagged sidecar input compiled into compass-stack/server)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/server/socket.go"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a go/events change (sidecar input)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/events/bus.go"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected on a go/gen change (generated sidecar input)", () => {
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["go/gen/db/models.go"],
				}),
			).darwinAffected,
		).toBe(true);
	});

	test("darwinAffected false when the PR touches neither the shell nor the bundler", () => {
		expect(
			generate(prInput({ affectedIds: [], changedPaths: ["docs/readme.md"] }))
				.darwinAffected,
		).toBe(false);
		// A different toolchain nix file must NOT trigger it.
		expect(
			generate(
				prInput({
					affectedIds: [],
					changedPaths: ["tools/toolchain/gate-tools.nix"],
				}),
			).darwinAffected,
		).toBe(false);
	});

	test("push: forge + gtk4 + darwin unconditionally true, changedPaths ignored", () => {
		const out = generate({
			projects: workspace(),
			affectedIds: workspace().map((p) => p.id),
			changedPaths: [],
			event: "push",
		});
		expect(out.forgeAffected).toBe(true);
		expect(out.gtk4Affected).toBe(true);
		expect(out.darwinAffected).toBe(true);
	});

	test("schedule: forge + gtk4 + darwin unconditionally true", () => {
		const out = generate({
			projects: workspace(),
			affectedIds: workspace().map((p) => p.id),
			changedPaths: [],
			event: "schedule",
		});
		expect(out.forgeAffected).toBe(true);
		expect(out.gtk4Affected).toBe(true);
		expect(out.darwinAffected).toBe(true);
	});

	test("push: every non-empty group runs (full sweep)", () => {
		const out = generate({
			projects: workspace(),
			affectedIds: workspace().map((p) => p.id),
			changedPaths: [],
			event: "push",
		});
		for (const e of out.matrix) {
			expect(e.run).toBe("true");
		}
		expect(out.pgtestAffected).toBe(true);
		expect(out.microvmAffected).toBe(true);
	});
});

describe("null ciTarget member does not make its group run", () => {
	test("a group whose only affected member has no ci task emits run:false", () => {
		const projects: ProjectInput[] = [
			{ id: "compass-go", tags: ["ci-group.go"], ciTarget: "compass-go:ci" },
			{ id: "no-ci", tags: ["ci-group.go"], ciTarget: null },
		];
		const out = generate({
			projects,
			affectedIds: ["no-ci"],
			changedPaths: [],
			event: "pull_request",
		});
		const go = out.matrix.find((e) => e.group === "go");
		// The only affected member has no ci task → no runnable target, so the
		// leg must not run (a running leg always has >=1 target; a bare
		// `moon run` with no target is the bug this guards against).
		expect(go?.run).toBe("false");
		expect(go?.targets).toEqual([]);
	});

	test("a mixed group emits only the non-null ciTargets of affected members", () => {
		const projects: ProjectInput[] = [
			{ id: "compass-go", tags: ["ci-group.go"], ciTarget: "compass-go:ci" },
			{ id: "no-ci", tags: ["ci-group.go"], ciTarget: null },
		];
		const out = generate({
			projects,
			affectedIds: ["compass-go", "no-ci"],
			changedPaths: [],
			event: "pull_request",
		});
		expect(out.matrix.find((e) => e.group === "go")?.targets).toEqual([
			"compass-go:ci",
		]);
	});
});

describe("empty affected closure — matrix still non-empty (fromJSON safe)", () => {
	test("a docs-only PR touching no grouped project yields all placeholders", () => {
		const out = generate(
			prInput({ affectedIds: [], changedPaths: ["docs/x.md"] }),
		);
		expect(out.matrix.length).toBeGreaterThan(0);
		expect(out.matrix.every((e) => e.run === "false")).toBe(true);
		expect(out.pgtestAffected).toBe(false);
		expect(out.microvmAffected).toBe(false);
		expect(out.forgeAffected).toBe(false);
		expect(out.gtk4Affected).toBe(false);
		expect(out.darwinAffected).toBe(false);
	});
});

describe("parseTaskAffectedIds — the cross-tree gate closure", () => {
	test("returns the project ids that have affected tasks", () => {
		const json = JSON.stringify({
			tasks: {
				"orion-ref-gate": { check: {} },
				"compass-go": { test: {} },
			},
		});
		expect(parseTaskAffectedIds(json).sort()).toEqual([
			"compass-go",
			"orion-ref-gate",
		]);
	});

	test("an unaffected workspace yields no ids, not a throw", () => {
		// `moon query tasks` prints its envelope unconditionally, so `{}` is a
		// real "nothing affected" and must be distinguishable from a failure.
		expect(parseTaskAffectedIds(JSON.stringify({ tasks: {} }))).toEqual([]);
	});

	test("a payload with no tasks key throws", () => {
		expect(() => parseTaskAffectedIds(JSON.stringify({ options: {} }))).toThrow(
			"missing the expected tasks key",
		);
	});

	test("a null tasks value throws the named diagnostic, not a TypeError", () => {
		// Guarding `undefined` alone let `null` reach Object.keys, which threw a
		// raw TypeError — fail-closed either way, but unreadable in the log.
		expect(() => parseTaskAffectedIds(JSON.stringify({ tasks: null }))).toThrow(
			"missing the expected tasks key",
		);
	});

	test("a task id containing a colon throws", () => {
		expect(() =>
			parseTaskAffectedIds(
				JSON.stringify({ tasks: { "orion-ref-gate:check": {} } }),
			),
		).toThrow("orion-ref-gate:check");
	});

	test("a task-only project joins the closure a project walk would miss", () => {
		// The defect this closes: a gate whose subject lives in other projects'
		// trees is absent from `projects --affected`, so unioning the two
		// closures is what puts its `ci` target in the matrix.
		const projects = workspace();
		const gate = proj("orion-ref-gate", "bun");
		const projectWalk = ["compass-go"];
		const taskLevel = parseTaskAffectedIds(
			JSON.stringify({ tasks: { "orion-ref-gate": { check: {} } } }),
		);
		const union = unionAffectedIds(
			projectWalk,
			taskLevel,
			new Set([...projects.map((project) => project.id), gate.id]),
		);
		expect(
			unionAffectedIds(
				projectWalk,
				["not-a-project"],
				new Set(projects.map((p) => p.id)),
			),
		).toEqual(projectWalk);

		const out = generate({
			projects: [...projects, gate],
			affectedIds: union,
			changedPaths: ["go/internal/board/x.go"],
			event: "pull_request",
		});
		const bun = out.matrix.find((l) => l.group === "bun");
		expect(bun?.targets).toContain("orion-ref-gate:ci");

		// Control: without the task-level half the gate is absent, so the
		// assertion above is defending the union and not the fixture.
		const without = generate({
			projects: [...projects, gate],
			affectedIds: projectWalk,
			changedPaths: ["go/internal/board/x.go"],
			event: "pull_request",
		});
		expect(
			without.matrix.find((l) => l.group === "bun")?.targets ?? [],
		).not.toContain("orion-ref-gate:ci");
	});

	test("the ledger gate joins the closure on a docs-only change", () => {
		// The sibling instance, and the one the design corpus depends on:
		// design-ledger-gate's `check` declares a workspace-root input glob
		// over docs/designs, which no project owns, so a docs-only diff leaves
		// it out of the project walk entirely. Measured on moon 2.5.3 against
		// docs/designs/DECISIONS.md: the walk yields flake-gate and root; the
		// task half yields design-ledger-gate. A later change that narrowed the
		// closure back for the design corpus specifically would leave the
		// orion-ref-gate cases above green, so this fixture names its own gate.
		// flake-gate and root are the universal floor of every moon closure, so
		// they must exist as grouped members for the walk half to be
		// well-formed input to the generator.
		const projects = [
			...workspace(),
			proj("flake-gate", "nix"),
			proj("root", "bun"),
		];
		const gate = proj("design-ledger-gate", "bun");
		const projectWalk = ["flake-gate", "root"];
		const union = unionAffectedIds(
			projectWalk,
			parseTaskAffectedIds(
				JSON.stringify({ tasks: { "design-ledger-gate": { check: {} } } }),
			),
			new Set([...projects.map((project) => project.id), gate.id]),
		);

		const out = generate({
			projects: [...projects, gate],
			affectedIds: union,
			changedPaths: ["docs/designs/DECISIONS.md"],
			event: "pull_request",
		});
		expect(out.matrix.find((l) => l.group === "bun")?.targets).toContain(
			"design-ledger-gate:ci",
		);

		// Control: the project walk alone leaves the ledger unchecked, which is
		// the pre-union behaviour a docs-only PR actually got.
		const without = generate({
			projects: [...projects, gate],
			affectedIds: projectWalk,
			changedPaths: ["docs/designs/DECISIONS.md"],
			event: "pull_request",
		});
		expect(
			without.matrix.find((l) => l.group === "bun")?.targets ?? [],
		).not.toContain("design-ledger-gate:ci");
	});
});

describe("outputLines — the $GITHUB_OUTPUT key contract", () => {
	// Every key ci.yml reads via `needs.setup.outputs.*`. The rollup accepts a
	// skipped work job only when its paired flag is exactly "false", so a
	// dropped key arrives empty and reds the rollup — on a later PR, not on the
	// one that dropped it. Enumerated so a deletion fails here instead.
	const REQUIRED_KEYS = [
		"matrix",
		"pgtest_affected",
		"microvm_affected",
		"forge_affected",
		"gtk4_affected",
		"darwin_affected",
	] as const;

	function linesFor(): string[] {
		return outputLines(
			generate({
				projects: [proj("a", "bun")],
				affectedIds: ["a"],
				changedPaths: [],
				event: "pull_request",
			}),
		);
	}

	test("emits exactly the keys ci.yml consumes, once each", () => {
		const keys = linesFor().map((l) => l.split("=")[0]);
		expect(keys.sort()).toEqual([...REQUIRED_KEYS].sort());
	});

	test("every flag is a literal true or false, never empty", () => {
		// An empty value is the shape that reds the rollup, so assert the
		// rendered text rather than the boolean it came from.
		for (const line of linesFor()) {
			const [key, ...rest] = line.split("=");
			const value = rest.join("=");
			expect(value).not.toBe("");
			if (key !== "matrix") {
				expect(["true", "false"]).toContain(value);
			}
		}
	});

	test("the affected-set flags track the generated output, not a constant", () => {
		// pgtest/microvm derive from the affected set. The path-derived three
		// are covered by the next test, which needs its own fixtures to tell
		// them apart. Negative control: nothing affected must render "false",
		// otherwise the assertions above would pass on a hardcoded "true".
		const none = outputLines(
			generate({
				projects: [proj("a", "bun")],
				affectedIds: [],
				changedPaths: [],
				event: "pull_request",
			}),
		);
		expect(none).toContain("pgtest_affected=false");
		expect(none).toContain("microvm_affected=false");

		// Positive control on the same keys: naming the pgtest project flips
		// both, since each is derived from the affected set.
		const some = outputLines(
			generate({
				projects: [proj("compass-go", "go")],
				affectedIds: ["compass-go"],
				changedPaths: [],
				event: "pull_request",
			}),
		);
		expect(some).toContain("pgtest_affected=true");
		expect(some).toContain("microvm_affected=true");
	});

	test("each path-derived flag renders its own boolean, not a sibling's", () => {
		// forge/gtk4/darwin are the three flags a full sweep forces true, but
		// on a pull_request they are purely changedPaths-derived, so each can
		// be driven independently. Asserting them together on one broad path
		// would not distinguish a cross-wire (gtk4 rendered from
		// darwinAffected, say) because compass-app paths set gtk4 and darwin
		// as a pair. So each case below selects ONE flag and pins the other
		// two to false. Without this the keys are covered by name only, and a
		// wrong "false" is worse than a dropped key: an empty value reds the
		// rollup, whereas "false" reads as a legitimate skip and greens it.
		const flags = (paths: string[]): string[] =>
			outputLines(
				generate({
					projects: [proj("a", "bun")],
					affectedIds: [],
					changedPaths: paths,
					event: "pull_request",
				}),
			).filter((line) => /^(forge|gtk4|darwin)_affected=/.test(line));

		// forge + darwin, gtk4 false. `go/internal/**` is a darwin sidecar
		// prefix (the mac lane cross-compiles the pure-Go sidecars), so a
		// forge path necessarily fires darwin too — asserting darwin=false
		// here would be asserting against the shipped path sets. gtk4 staying
		// false is what discriminates: a gtk4 rendered from darwinAffected
		// flips it.
		expect(flags(["go/internal/forge/oracle.go"]).sort()).toEqual([
			"darwin_affected=true",
			"forge_affected=true",
			"gtk4_affected=false",
		]);

		// darwin alone: the macos-bundle tree is darwin-only.
		expect(flags(["tools/macos-bundle/index.ts"]).sort()).toEqual([
			"darwin_affected=true",
			"forge_affected=false",
			"gtk4_affected=false",
		]);

		// gtk4's tree implies darwin (the mac app bundles the same binary),
		// so this case pins the pair together and leaves forge false.
		expect(flags(["go/cmd/compass-app/main.go"]).sort()).toEqual([
			"darwin_affected=true",
			"forge_affected=false",
			"gtk4_affected=true",
		]);

		// Negative control: a path in none of the three sets renders all
		// false, so the assertions above are not passing on a constant.
		expect(flags(["docs/readme.md"]).sort()).toEqual([
			"darwin_affected=false",
			"forge_affected=false",
			"gtk4_affected=false",
		]);
	});
});
