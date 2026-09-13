import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import botConfig from "./bot-config.json5";
import config from "./config.json5";
// The shipped FOD table. Imported (not restated) so the coverage guard at the
// end re-derives its requirement from the declaration. Side-effect-safe: that
// module does no I/O at import (main() is behind import.meta.main).
import { FOD_ENTRIES } from "./refresh-fod-hashes.ts";

// Guard suite for compass's self-hosted Renovate config (RIG-2432). Ported from
// the internal monorepo's config.test.ts and adapted to compass's config +
// ecosystems (bun catalog, devenv-nixpkgs channel, toolchain pins, gomod, GitHub
// Actions; no rust/pulumi/woodpecker). The .json5 configs load via Bun's loader.

type PostUpgradeTasks = {
	commands?: string[];
	fileFilters?: string[];
	executionMode?: string;
};
type PackageRule = {
	matchManagers?: string[];
	matchDatasources?: string[];
	matchUpdateTypes?: string[];
	matchFileNames?: string[];
	matchDepTypes?: string[];
	matchPackageNames?: string[];
	matchDepNames?: string[];
	excludeDepNames?: string[];
	allowedVersions?: string;
	groupName?: string | null;
	schedule?: string[];
	minimumReleaseAge?: string | null;
	dependencyDashboardApproval?: boolean;
	enabled?: boolean;
	force?: { enabled?: boolean };
	postUpgradeTasks?: PostUpgradeTasks;
};
type CustomManager = {
	customType?: string;
	managerFilePatterns?: string[];
	matchStrings?: string[];
	matchStringsStrategy?: string;
	datasourceTemplate?: string;
	depTypeTemplate?: string;
	depNameTemplate?: string;
	packageNameTemplate?: string;
	currentValueTemplate?: string;
	currentDigestTemplate?: string;
	versioningTemplate?: string;
	extractVersionTemplate?: string;
};
type RenovateConfig = {
	extends: string[];
	timezone?: string;
	rebaseWhen?: string;
	packageRules: PackageRule[];
	osvVulnerabilityAlerts?: boolean;
	vulnerabilityAlerts?: { enabled?: boolean };
	enabledManagers?: string[];
	customManagers?: CustomManager[];
	postUpgradeTasks?: PostUpgradeTasks;
};

const cfg = config as RenovateConfig;
const bot = botConfig as {
	allowedCommands?: string[];
	customEnvVariables?: Record<string, string>;
	executionTimeout?: number;
};
// tools/renovate/ → repo root is two levels up.
const repoRoot = join(import.meta.dir, "..", "..");

// The FOD-hash refresh command, declared once. It rides FIVE task sites in
// config.json5 and is asserted from several describes below; a rename must be a
// single edit here, not one per assertion (a missed copy degrades quietly).
const FOD_COMMAND = "bun tools/renovate/refresh-fod-hashes.ts";

// Every postUpgradeTasks.commands entry declared anywhere in config.json5: the
// top-level task plus every packageRule-level task.
const allDeclaredCommands = (): string[] => {
	const out: string[] = [...(cfg.postUpgradeTasks?.commands ?? [])];
	for (const rule of cfg.packageRules) {
		out.push(...(rule.postUpgradeTasks?.commands ?? []));
	}
	return out;
};

// Regex metacharacters that must be backslash-escaped when a literal glob char
// is emitted into the RegExp source below. A static char→true lookup table (not
// a string literal) so the `{`/`}` members can't read as a template placeholder.
const REGEX_METACHARS: Record<string, true> = {
	".": true,
	"+": true,
	"^": true,
	$: true,
	"{": true,
	"}": true,
	"(": true,
	")": true,
	"|": true,
	"[": true,
	"]": true,
	"\\": true,
};

// Minimal Renovate-style glob → RegExp (matchFileNames uses minimatch). Supports
// the two shapes this config uses: `*` (one path segment) and `**` (any depth).
const globToRegExp = (glob: string): RegExp => {
	let re = "";
	for (let i = 0; i < glob.length; i++) {
		const c = glob[i];
		if (c === "*") {
			if (glob[i + 1] === "*") {
				re += ".*";
				i++;
			} else {
				re += "[^/]*";
			}
		} else if (c !== undefined && REGEX_METACHARS[c]) {
			re += `\\${c}`;
		} else {
			re += c;
		}
	}
	return new RegExp(`^${re}$`);
};

type SyntheticDep = {
	manager: string;
	fileName?: string;
	depName?: string;
	packageName?: string;
	updateType?: string;
	depType?: string;
};

// Renovate applies packageRules top-to-bottom, last-match-wins, so a later rule
// can override groupName set by an earlier one. Resolve the effective groupName
// for a synthetic dep by replaying that semantics over the real rule array.
const resolveGroupName = (dep: SyntheticDep): string | null | undefined => {
	let group: string | null | undefined;
	for (const rule of cfg.packageRules) {
		if (rule.matchManagers && !rule.matchManagers.includes(dep.manager)) {
			continue;
		}
		if (
			rule.matchUpdateTypes &&
			!(dep.updateType && rule.matchUpdateTypes.includes(dep.updateType))
		) {
			continue;
		}
		if (
			rule.matchDepTypes &&
			!(dep.depType && rule.matchDepTypes.includes(dep.depType))
		) {
			continue;
		}
		if (
			rule.matchDepNames &&
			!(dep.depName && rule.matchDepNames.includes(dep.depName))
		) {
			continue;
		}
		if (
			rule.matchPackageNames &&
			!(dep.packageName && rule.matchPackageNames.includes(dep.packageName))
		) {
			continue;
		}
		if (
			rule.matchFileNames &&
			!(
				dep.fileName &&
				rule.matchFileNames.some((g) =>
					globToRegExp(g).test(dep.fileName as string),
				)
			)
		) {
			continue;
		}
		if (
			rule.excludeDepNames &&
			dep.depName &&
			rule.excludeDepNames.includes(dep.depName)
		) {
			continue;
		}
		// A rule with no groupName key (e.g. the TS <7 cap) does not touch grouping.
		if ("groupName" in rule) group = rule.groupName;
	}
	return group;
};

describe("tools/renovate postUpgradeTasks ↔ allowedCommands (RIG-2432)", () => {
	// postUpgradeTasks.commands are gated by the BOT config's allowedCommands
	// allowlist, matched UNANCHORED via regEx(pattern).test(cmd) — so each entry's
	// ^…$ IS the security property. Seven DISTINCT commands (the FOD refresh rides
	// five sites under one entry, the devenv-fork relock both fork rules under one).
	const commands = allDeclaredCommands();
	const distinctCommands = [...new Set(commands)];
	const allowed = bot.allowedCommands ?? [];

	test("declares seven DISTINCT postUpgrade commands and seven allowlist entries", () => {
		expect(distinctCommands).toHaveLength(7);
		expect(allowed).toHaveLength(7);
	});

	test("the fod-hash refresh is declared at all six task sites", () => {
		// The command must ride every task shape that can own a bump moving a pinned
		// FOD, because a rule-level task REPLACES the top-level one. The six sites:
		// 1. top-level (gomod + bun/TS); 2. devenv-nixpkgs channel (moves pkgs.bun);
		// 3. devenv fork root; 4. go↔go-overlay; 5. catalog; 6. fork agent-image.

		// Sites 3, 4, 6 carry it fail-safe: each relocks ONE non-nixpkgs input, so
		// none moves pkgs.bun today, but each writes a declared trigger of the
		// entrypoint.nix entry, so the refresh is a no-op when nothing moved. The
		// end-of-file guard keeps this true: a trigger site must run refresh LAST.
		expect(commands.filter((c) => c === FOD_COMMAND)).toHaveLength(6);
		const topLevel = cfg.postUpgradeTasks?.commands ?? [];
		expect(topLevel).toContain(FOD_COMMAND);
		const catalogRule = cfg.packageRules.find(
			(r) =>
				r.matchDepTypes?.includes("workspaces.catalog") && r.postUpgradeTasks,
		);
		expect(catalogRule?.postUpgradeTasks?.commands).toContain(FOD_COMMAND);
	});

	test("every declared command is permitted by an anchored allowlist entry", () => {
		for (const command of commands) {
			expect(allowed.some((a) => new RegExp(a).test(command))).toBe(true);
		}
	});

	test("no allowlist entry is an orphan — each matches a declared command", () => {
		for (const entry of allowed) {
			expect(commands.some((c) => new RegExp(entry).test(c))).toBe(true);
		}
	});

	test("every allowlist entry is fully anchored (^…$), refusing substring exec", () => {
		for (const entry of allowed) {
			expect(entry.startsWith("^")).toBe(true);
			expect(entry.endsWith("$")).toBe(true);
		}
		// The anchoring really does refuse an appended-metacharacter variant.
		expect(
			allowed.some((a) =>
				new RegExp(a).test("bun install --lockfile-only; id"),
			),
		).toBe(false);
		// …including on the FOD command (a `; rm -rf` tail must not slip through).
		expect(
			allowed.some((a) =>
				new RegExp(a).test("bun tools/renovate/refresh-fod-hashes.ts; id"),
			),
		).toBe(false);
	});

	test("permits exactly the seven declared commands", () => {
		expect(distinctCommands.sort()).toEqual(
			[
				"bun install --lockfile-only",
				"bun tools/renovate/refresh-agent-image-nixpkgs.ts",
				"bun tools/renovate/refresh-devenv-lock.ts",
				"bun tools/renovate/refresh-devenv-nixpkgs.ts",
				"bun tools/renovate/refresh-fod-hashes.ts",
				"bun tools/renovate/refresh-go-overlay.ts",
				"bun tools/renovate/refresh-toolchain-hashes.ts",
			].sort(),
		);
	});
});

describe("tools/renovate FOD-hash refresh wiring (PR #579)", () => {
	// A dep bump moves a pinned Nix FOD hash; left stale the image build fails
	// "hash mismatch". Renovate only COMMITS files a task's fileFilters name, so a
	// task rewriting a FOD file without listing it silently drops the fix. This pins
	// the two eviction-critical sites; the end-of-file guard covers every site.
	const topLevel = cfg.postUpgradeTasks;
	const catalogRule = cfg.packageRules.find(
		(r) =>
			r.matchDepTypes?.includes("workspaces.catalog") && r.postUpgradeTasks,
	);

	test("the top-level branch-mode task runs the fod refresh and is branch mode", () => {
		expect(topLevel?.commands).toContain(FOD_COMMAND);
		expect(topLevel?.executionMode).toBe("branch");
	});

	test("the top-level task commits ALL THREE Go/bun FOD files (fileFilters cover them)", () => {
		// gomod + bun/npm-first branches inherit this slot; it must commit the Go
		// vendorHash file, its flake.nix mirror (identical hash, refresh mirrors the
		// value in), and the bun outputHash file. A missing flake.nix would drop the
		// mirror edit → a gomod bump lands with flake.nix stale (RIG-2852 Gap 1).
		expect(topLevel?.fileFilters).toContain("guest-image/default.nix");
		expect(topLevel?.fileFilters).toContain("flake.nix");
		expect(topLevel?.fileFilters).toContain("agent-image/entrypoint.nix");
	});

	test("the catalog rule runs the fod refresh in UPDATE mode (eviction-proof)", () => {
		// A catalog-first rollup branch evicts the top-level branch task, so the
		// refresh must also ride the catalog rule's per-upgrade update pass.
		expect(catalogRule?.postUpgradeTasks?.commands).toContain(FOD_COMMAND);
		expect(catalogRule?.postUpgradeTasks?.executionMode).toBe("update");
	});

	test("the catalog task commits the bun outputHash file it can move", () => {
		// A catalog bump moves the bun outputHash (compass-agent consumes catalog:
		// deps); it never touches the Go module set, so only entrypoint.nix is listed.
		expect(catalogRule?.postUpgradeTasks?.fileFilters).toContain(
			"agent-image/entrypoint.nix",
		);
	});
});

describe("tools/renovate OSV vuln source honors the disable rules", () => {
	// Renovate owns security remediation. OSV is the config-driven vuln source
	// that respects the `enabled: false` packageRules, unlike a repo-wide toggle.
	test("osvVulnerabilityAlerts is enabled", () => {
		expect(cfg.osvVulnerabilityAlerts).toBe(true);
	});

	// INVARIANT: a vuln fix is a packageRule carrying force: {...vulnerabilityAlerts},
	// and applyPackageRules clears a prior skipReason when force.enabled is truthy —
	// cancelling every enabled:false rule (postgres pin, gomod go, biome). The
	// default object has no enabled key; assert it is absent.
	test("does NOT set vulnerabilityAlerts.enabled (would re-open disabled bumps)", () => {
		expect(cfg.vulnerabilityAlerts?.enabled).toBeUndefined();
		expect(cfg.vulnerabilityAlerts?.enabled).not.toBe(true);
	});
});

describe("tools/renovate extends", () => {
	// SHA-pin maintenance: helpers:pinGitHubActionDigests pins every workflow
	// `uses:` to a commit SHA on sight (RIG-2432), going beyond Dependabot.
	test("extends includes helpers:pinGitHubActionDigests", () => {
		expect(cfg.extends).toContain("helpers:pinGitHubActionDigests");
	});

	test("schedules daily, not weekly", () => {
		expect(cfg.extends).toContain("schedule:daily");
		expect(cfg.extends).not.toContain("schedule:weekly");
	});
});

describe("tools/renovate root bun catalog manager", () => {
	// Find the catalog manager by behavior (npm-backed regex), not by index — a
	// second customManager (the devenv git-refs one) shares the array.
	const catalogManager = cfg.customManagers?.find(
		(m) =>
			m.datasourceTemplate === "npm" && m.matchStringsStrategy === "recursive",
	);

	test("custom.regex is allowlisted, or the whole block is inert", () => {
		expect(cfg.enabledManagers).toContain("custom.regex");
	});

	test("declares an npm-backed recursive regex catalog manager", () => {
		expect(catalogManager).toBeDefined();
		expect(catalogManager?.customType).toBe("regex");
		expect(catalogManager?.datasourceTemplate).toBe("npm");
		expect(catalogManager?.matchStringsStrategy).toBe("recursive");
		expect(catalogManager?.versioningTemplate).toBe("npm");
	});

	// Real-manifest extraction: the shipped scope/entry matchStrings must recover
	// EXACTLY the set of catalog keys the JSON parser sees in the real root
	// package.json — set equality both ways (truncation drops names, a runaway
	// scope adds them).
	const extractFrom = (text: string): string[] => {
		const [scope, entry] = catalogManager?.matchStrings ?? [];
		return [...text.matchAll(new RegExp(scope as string, "g"))].flatMap((m) =>
			[...m[0].matchAll(new RegExp(entry as string, "g"))].map(
				(e) => e.groups?.depName as string,
			),
		);
	};
	const truthKeys = (): string[] => {
		const parsed = JSON.parse(
			readFileSync(join(repoRoot, "package.json"), "utf8"),
		) as { workspaces?: { catalog?: Record<string, string> } };
		return Object.keys(parsed.workspaces?.catalog ?? {});
	};

	test("regex extraction recovers every pin the JSON parser sees", () => {
		const [scope, entry] = catalogManager?.matchStrings ?? [];
		expect(catalogManager?.matchStrings).toHaveLength(2);
		expect(scope).toBeDefined();
		expect(entry).toBeDefined();

		const raw = readFileSync(join(repoRoot, "package.json"), "utf8");
		const truth = truthKeys();
		expect(truth.length).toBeGreaterThan(1);
		const scoped = [...raw.matchAll(new RegExp(scope as string, "g"))];
		expect(scoped).toHaveLength(1);

		expect(extractFrom(raw).sort()).toEqual(truth.slice().sort());
	});

	// The scope pattern bounds the catalog with `[^}]*`, so it stops at the FIRST
	// `}`. These two mutate the real manifest into each truncating shape and show
	// the bound really bites — making the guard above meaningful, not incidental.
	test("a nested object inside the catalog truncates extraction", () => {
		const raw = readFileSync(join(repoRoot, "package.json"), "utf8");
		const mutated = raw.replace(
			/"catalog"\s*:\s*\{/,
			'"catalog": {\n\t\t\t"catalogs": { "react19": { "react": "^19.0.0" } },',
		);
		expect(mutated).not.toBe(raw);

		const extracted = extractFrom(mutated);
		expect(extracted.length).toBeLessThan(truthKeys().length);
		expect(extracted.slice().sort()).not.toEqual(truthKeys().slice().sort());
	});

	test("a `}` inside a version value truncates extraction", () => {
		const raw = readFileSync(join(repoRoot, "package.json"), "utf8");
		const [first] = truthKeys();
		const needle = `"${first}": "`;
		const at = raw.indexOf(needle);
		expect(at).toBeGreaterThan(-1);
		const close = raw.indexOf('"', at + needle.length);
		expect(close).toBeGreaterThan(-1);
		const mutated = `${raw.slice(0, close)}}${raw.slice(close)}`;

		const extracted = extractFrom(mutated);
		expect(extracted.length).toBeLessThan(truthKeys().length);
		expect(extracted.slice().sort()).not.toEqual(truthKeys().slice().sort());
	});

	// The catalog folds into the TypeScript rollup (one PR per language) alongside
	// the native bun/npm managers.
	test("catalog deps fold into the 'TypeScript dependencies' rollup", () => {
		const rollup = cfg.packageRules.find(
			(r) => r.groupName === "TypeScript dependencies",
		);
		expect(rollup?.matchManagers).toContain("custom.regex");
		expect(rollup?.matchManagers).toContain("bun");
		expect(rollup?.matchManagers).toContain("npm");
	});
});

describe("tools/renovate devenv nixpkgs lockstep", () => {
	// The customManager surfacing devenv.lock's channel rev as a git-refs digest;
	// find it by the dep it stamps, not index — and NOT by file pattern: the
	// RIG-2815 devenv-FORK managers also match a devenv lock, so includes("devenv")
	// would be ambiguous between three managers over the same two files.
	const devenvManager = cfg.customManagers?.find(
		(m) => m.depNameTemplate === "cachix/devenv-nixpkgs",
	);
	const devenvRule = cfg.packageRules.find(
		(r) => r.groupName === "devenv nixpkgs channel",
	);

	test("declares a git-refs regex manager scoped to devenv.lock", () => {
		expect(devenvManager).toBeDefined();
		expect(devenvManager?.customType).toBe("regex");
		expect(devenvManager?.datasourceTemplate).toBe("git-refs");
		expect(devenvManager?.depNameTemplate).toBe("cachix/devenv-nixpkgs");
		expect(devenvManager?.currentValueTemplate).toBe("rolling");
		const pattern = devenvManager?.managerFilePatterns?.[0];
		const delimited = /^\/(.*)\/$/.exec(pattern as string);
		expect(delimited).not.toBeNull();
		const re = new RegExp(delimited?.[1] as string);
		expect(re.test("devenv.lock")).toBe(true);
		expect(re.test("package.json")).toBe(false);
		expect(re.test("a/devenv.lock")).toBe(false); // anchored to root
	});

	// The matchString must recover EXACTLY ONE 40-hex rev from the REAL
	// devenv.lock, and it must be the OUTER devenv-nixpkgs channel rev
	// (nodes.nixpkgs.locked.rev), not the inner nixpkgs-src rev, and not zero.
	test("matchString extracts the channel rev from the real devenv.lock", () => {
		const lockText = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const matchString = devenvManager?.matchStrings?.[0];
		expect(matchString).toBeDefined();
		const matches = [
			...lockText.matchAll(new RegExp(matchString as string, "g")),
		];
		expect(matches).toHaveLength(1);
		const rev = matches[0]?.groups?.currentDigest;
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
		const parsed = JSON.parse(lockText);
		expect(rev).toBe(parsed.nodes.nixpkgs.locked.rev);
		expect(rev).not.toBe(parsed.nodes["nixpkgs-src"].locked.rev);
	});

	// Solo-branched: its own unique groupName so the branch-mode lockstep task owns
	// the single per-branch task slot; scheduled; cooldown-nulled (a git-refs
	// digest carries no release age, so a strict cooldown would defer it forever).
	test("the devenv rule is solo-grouped, scheduled, and cooldown-exempt", () => {
		expect(devenvRule).toBeDefined();
		expect(devenvRule?.matchDepNames).toContain("cachix/devenv-nixpkgs");
		expect(devenvRule?.groupName).toBe("devenv nixpkgs channel");
		const sharing = cfg.packageRules.filter(
			(r) => r.groupName === "devenv nixpkgs channel",
		);
		expect(sharing).toHaveLength(1);
		expect(devenvRule?.schedule?.length).toBeGreaterThan(0);
		expect(devenvRule?.minimumReleaseAge).toBeNull();
	});

	// Branch-mode lockstep task over the files the script writes: devenv.lock +
	// package.json (biome catalog) + bun.lock (steps 2/4/5), flake.nix + flake.lock
	// (step 6), and agent-image/entrypoint.nix (the FOD outputHash).

	// The FOD refresh is required: a channel bump moves pkgs.bun (the FOD builder)
	// and, when the biome pin moves, re-resolves the bun.lock closure — either can
	// move the outputHash (PR #580 failed on this). refresh-fod-hashes.ts runs AFTER
	// the relock and gates on bun.lock OR devenv.lock.

	// Order is load-bearing and silent when wrong: the devenv.lock trigger makes the
	// FOD gate in either order, so a reversed order realises the FOD against the
	// still-at-base bun.lock, then step 5 rewrites bun.lock underneath — committing a
	// pin over the OLD closure. The pinned toEqual below turns that reversal red.
	test("the lockstep postUpgradeTask is branch-mode, runs relock-then-FOD, and commits every written file", () => {
		const task = devenvRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.fileFilters).toEqual([
			"devenv.lock",
			"package.json",
			"bun.lock",
			"flake.nix",
			"flake.lock",
			"agent-image/entrypoint.nix",
		]);
		// Silent-drop guard (mirrors the top-level rule's flake.nix guard): step 6
		// writes flake.nix + flake.lock, and fileFilters is an INCLUDE allowlist.
		// Drop either and a channel bump ships with the flake skewed from devenv.lock
		// → flake-parity reds while the script's own tests stay green.
		expect(task?.fileFilters).toContain("flake.nix");
		expect(task?.fileFilters).toContain("flake.lock");
		expect(task?.commands).toEqual([
			"bun tools/renovate/refresh-devenv-nixpkgs.ts",
			"bun tools/renovate/refresh-fod-hashes.ts",
		]);
		expect(task?.fileFilters).toContain("agent-image/entrypoint.nix");
	});

	// The digest-excludes-rollup seam: the TS rollup ALSO matches custom.regex, so
	// the ONLY thing keeping the devenv digest out of that shared branch is that the
	// rollup admits only patch/minor and a git-refs update is type `digest`.
	test("the TypeScript rollup cannot capture a digest update", () => {
		const rollup = cfg.packageRules.find(
			(r) =>
				r.groupName === "TypeScript dependencies" &&
				r.matchManagers?.includes("custom.regex"),
		);
		expect(rollup).toBeDefined();
		expect(rollup?.matchUpdateTypes).toBeDefined();
		expect(rollup?.matchUpdateTypes).not.toContain("digest");
		for (const t of rollup?.matchUpdateTypes ?? []) {
			expect(["patch", "minor"]).toContain(t);
		}
	});
});

describe("tools/renovate devenv fork currency (RIG-2815, RIG-2546 T7)", () => {
	// Both compass devenv scopes resolve github:RigelBuild/devenv by DEFAULT BRANCH,
	// so the rev lives only in devenv.lock. Two customManagers surface the two locks'
	// fork revs, each paired with its solo-branched relock rule. RD-1 keeps the locks
	// on INDEPENDENT cadences, so this is two managers + two rules, not one pair.
	const RELOCK = "bun tools/renovate/refresh-devenv-lock.ts";
	const forkScopes: {
		label: string;
		depName: string;
		lock: string;
		groupName: string;
		patternLiteral: string;
		// The rule's WHOLE declared task, pinned literally per scope. Both locks are
		// declared triggers of the entrypoint.nix FOD entry — each supplies a
		// `pkgs.bun` to one of that file's two importers — so both rules carry the
		// FOD refresh and name the FOD file.
		taskCommands: string[];
		taskFileFilters: string[];
	}[] = [
		{
			label: "root",
			depName: "RigelBuild/devenv",
			lock: "devenv.lock",
			groupName: "devenv fork (root)",
			patternLiteral: "/^devenv\\.lock$/",
			taskCommands: [RELOCK, FOD_COMMAND],
			taskFileFilters: ["devenv.lock", "agent-image/entrypoint.nix"],
		},
		{
			label: "agent-image",
			depName: "RigelBuild/devenv-agent-image",
			lock: "agent-image/devenv.lock",
			groupName: "devenv fork (agent-image)",
			patternLiteral: "/^agent-image\\/devenv\\.lock$/",
			taskCommands: [RELOCK, FOD_COMMAND],
			taskFileFilters: [
				"agent-image/devenv.lock",
				"agent-image/entrypoint.nix",
			],
		},
	];
	const managerFor = (depName: string) =>
		cfg.customManagers?.find((m) => m.depNameTemplate === depName);
	const ruleFor = (depName: string) =>
		cfg.packageRules.find((r) => r.matchDepNames?.includes(depName));

	test.each(forkScopes)(
		"declares a git-refs regex manager for the $label lock's fork rev",
		({ depName, lock, patternLiteral }) => {
			const manager = managerFor(depName);
			expect(manager).toBeDefined();
			expect(manager?.customType).toBe("regex");
			expect(manager?.datasourceTemplate).toBe("git-refs");
			// Both scopes point at the SAME fork repo (RD-1's unified source); only
			// the depName differs, which is what keeps the two rules independently
			// governed and in separate branches.
			expect(manager?.packageNameTemplate).toBe(
				"https://github.com/RigelBuild/devenv",
			);
			// The locks name no ref, so the tracked value is the default branch.
			expect(manager?.currentValueTemplate).toBe("main");

			// The file pattern must be ANCHORED to exactly this lock: a loose pattern
			// would make the root manager extract from the agent-image lock too,
			// collapsing the two scopes into one dep with conflicting digests. Pin the
			// LITERAL first so a delimiter-semantics drift fails as a changed literal.
			expect(manager?.managerFilePatterns).toEqual([patternLiteral]);
			const pattern = manager?.managerFilePatterns?.[0];
			const delimited = /^\/(.*)\/$/.exec(pattern as string);
			expect(delimited).not.toBeNull();
			const re = new RegExp(delimited?.[1] as string);
			expect(re.test(lock)).toBe(true);
			expect(re.test("package.json")).toBe(false);
			expect(re.test(`a/${lock}`)).toBe(false); // anchored, no arbitrary prefix
			const otherLock = forkScopes.find((s) => s.lock !== lock)?.lock as string;
			expect(re.test(otherLock)).toBe(false); // and never the sibling scope
		},
	);

	// The matchString must recover EXACTLY ONE 40-hex rev from the REAL lock — the
	// fork's own nodes.devenv.locked.rev. The anchor's safety is that "repo":
	// "devenv", is followed by "rev" ONLY in the locked block (original is followed
	// by "type"). Assert uniqueness so a lock-format change fails HERE.
	test.each(forkScopes)(
		"matchString extracts the fork rev from the real $label lock",
		({ depName, lock }) => {
			const lockText = readFileSync(join(repoRoot, lock), "utf8");
			const matchString = managerFor(depName)?.matchStrings?.[0];
			expect(matchString).toBeDefined();
			const matches = [
				...lockText.matchAll(new RegExp(matchString as string, "g")),
			];
			expect(matches).toHaveLength(1);
			const rev = matches[0]?.groups?.currentDigest;
			expect(rev).toMatch(/^[a-f0-9]{40}$/);
			const parsed = JSON.parse(lockText);
			expect(rev).toBe(parsed.nodes.devenv.locked.rev);
			// Not the devenv-nixpkgs channel rev — a `"repo": "devenv"` prefix match
			// against `"devenv-nixpkgs"` is the exact mis-bind the trailing quote in
			// the anchor prevents.
			expect(rev).not.toBe(parsed.nodes.nixpkgs?.locked?.rev);
		},
	);

	// Solo-branched, scheduled, cooldown-nulled — the devenv-nixpkgs rule's shape.
	// The groupName must be UNIQUE to this rule: it is what makes the branch-mode
	// relock task safe (one branch-mode task slot per branch, so the dep must own
	// its branch) AND what keeps the two locks on independent cadences.
	test.each(forkScopes)(
		"the $label fork rule is solo-grouped, scheduled, and cooldown-exempt",
		({ depName, groupName }) => {
			const rule = ruleFor(depName);
			expect(rule).toBeDefined();
			expect(rule?.matchManagers).toContain("custom.regex");
			expect(rule?.groupName).toBe(groupName);
			expect(
				cfg.packageRules.filter((r) => r.groupName === groupName),
			).toHaveLength(1);
			expect(rule?.schedule?.length).toBeGreaterThan(0);
			// A git-refs digest on a moving branch HEAD carries no release age, so
			// the repo-wide strict cooldown would peg it permanently `pending` and
			// cut zero PRs (the RIG-1220 silent-no-updates shape).
			expect(rule?.minimumReleaseAge).toBeNull();
		},
	);

	// Branch-mode relock over exactly the files the rule writes. fileFilters is an
	// INCLUDE allowlist, so naming the sibling lock is dead surface and naming LESS
	// silent-drops the relock. The root scope also carries the FOD refresh,
	// relock-FIRST (its lock is a declared trigger of the entrypoint.nix pin).
	test.each(forkScopes)(
		"the $label relock postUpgradeTask is branch-mode over the files it writes",
		({ depName, taskCommands, taskFileFilters }) => {
			const task = ruleFor(depName)?.postUpgradeTasks;
			expect(task?.executionMode).toBe("branch");
			expect(task?.fileFilters).toEqual(taskFileFilters);
			expect(task?.commands).toEqual(taskCommands);
		},
	);

	// ONE command string serves both rules — the script self-gates on WHICH lock
	// changed — so a single anchored allowlist entry covers both. Assert the two
	// rules share the string rather than drifting into two near-identical scripts
	// (which would silently need a second allowlist entry).
	test("both fork rules declare the SAME relock command (one allowlist entry)", () => {
		const declaring = cfg.packageRules.filter((r) =>
			r.postUpgradeTasks?.commands?.includes(RELOCK),
		);
		expect(declaring).toHaveLength(2);
		expect(
			bot.allowedCommands?.filter((a) => new RegExp(a).test(RELOCK)),
		).toEqual(["^bun tools/renovate/refresh-devenv-lock\\.ts$"]);
	});

	// The two locks must never land in ONE branch: two branch-mode relock tasks on a
	// shared branch means Renovate builds only one and the other ships unrelocked.
	// Replay last-match-wins semantics and assert each digest resolves to its own
	// group, never the TS rollup.
	test.each(forkScopes)(
		"the $label fork digest resolves to its own solo branch, not the TS rollup",
		({ depName, lock, groupName }) => {
			const group = resolveGroupName({
				manager: "custom.regex",
				fileName: lock,
				depName,
				updateType: "digest",
			});
			expect(group).toBe(groupName);
			expect(group).not.toBe("TypeScript dependencies");
		},
	);

	test("the two fork scopes carry DISTINCT groupNames (independent cadences)", () => {
		const groups = forkScopes.map((s) => ruleFor(s.depName)?.groupName);
		expect(new Set(groups).size).toBe(forkScopes.length);
	});

	// M1 (RIG-2815 review): the relock nix runs the fork flakeref, which publishes
	// no cache, so every rev is a from-source build — plausibly past Renovate's
	// 15-min default timeout on a cold runner, which would kill the child and commit
	// the regex bump unrelocked. Pin the raised ceiling. globalOnly, so bot-config.
	test("bot-config sets an executionTimeout covering a cold fork build", () => {
		expect(typeof bot.executionTimeout).toBe("number");
		// Comfortably above the 15-min default; a cold from-source fork build can
		// exceed 15 min on a hosted runner.
		expect(bot.executionTimeout).toBeGreaterThanOrEqual(30);
	});

	// M2 (RIG-2815 review): the relock's nix run needs the runner's nix.conf naming
	// the devenv + cachix substituters and keys (the fork closure is not on
	// cache.nixos.org). renovate.yml's extra_nix_config provides them; assert it
	// still names both caches + keys so trimming them fails a test.
	test("renovate.yml wires the substituters the relock nix run needs", () => {
		const workflow = readFileSync(
			join(repoRoot, ".github", "workflows", "renovate.yml"),
			"utf8",
		);
		// Strip YAML comment lines before asserting, mirroring the self-pin
		// workflow guard below (config.test.ts): a substituter that survives only
		// in a commented-out or rationale-narrated block still leaves the runner
		// without it, so the guard must read live config, not prose.
		const liveConfig = workflow
			.split("\n")
			.filter((l) => !/^\s*#/.test(l))
			.join("\n");
		// Order-independent: both caches must sit on a LIVE extra-substituters
		// line, in either order.
		expect(
			/^\s*extra-substituters = .*https:\/\/devenv\.cachix\.org/m.test(
				liveConfig,
			),
		).toBe(true);
		expect(liveConfig).toContain("https://cachix.cachix.org");
		// Both trusted public keys must accompany their substituters on a live
		// extra-trusted-public-keys line, or nix rejects the cached paths and
		// falls back to a from-source build.
		expect(
			/^\s*extra-trusted-public-keys = .*devenv\.cachix\.org-1:/m.test(
				liveConfig,
			),
		).toBe(true);
		expect(liveConfig).toContain("cachix.cachix.org-1:");
	});

	// L3 (RIG-2815 review): the two fork managers carry the SAME matchStrings
	// literal (RD-1 forbids reconciling the rules; JSON5 has no anchor). Nothing else
	// pins that they stay in sync, so a one-sided edit would drift silently. Assert
	// they share one literal, mirroring the "same relock command" guard above.
	test("both fork managers declare the IDENTICAL matchString literal", () => {
		const literals = forkScopes.map(
			(s) => managerFor(s.depName)?.matchStrings?.[0],
		);
		expect(literals.every((l) => typeof l === "string")).toBe(true);
		expect(new Set(literals).size).toBe(1);
	});
});

describe("tools/renovate devenv nixpkgs channel: agent base image", () => {
	// The fourth devenv pin. Three of the four were tracked; agent-image's CHANNEL
	// rev was governed by no manager, so it only advanced by hand. This manager +
	// rule pair gives it the same solo-branched, self-relocking shape as its
	// siblings. Found by the dep it stamps: TWO managers now match this lock.
	const REFRESH = "bun tools/renovate/refresh-agent-image-nixpkgs.ts";
	const DEP = "cachix/devenv-nixpkgs-agent-image";
	const LOCK = "agent-image/devenv.lock";
	const GROUP = "devenv nixpkgs channel (agent-image)";
	const manager = cfg.customManagers?.find((m) => m.depNameTemplate === DEP);
	const rule = cfg.packageRules.find((r) => r.matchDepNames?.includes(DEP));

	test("declares a git-refs regex manager scoped to the agent-image lock", () => {
		expect(manager).toBeDefined();
		expect(manager?.customType).toBe("regex");
		expect(manager?.datasourceTemplate).toBe("git-refs");
		// Same upstream channel repo as the root manager; only the depName
		// differs, which is what keeps the two rules independently governed and
		// in separate branches.
		expect(manager?.packageNameTemplate).toBe(
			"https://github.com/cachix/devenv-nixpkgs",
		);
		expect(manager?.currentValueTemplate).toBe("rolling");

		// Pin the LITERAL first (a Renovate delimiter-semantics drift then fails
		// as a changed literal rather than silently changing what the re-parse
		// below tests), then re-parse it behaviourally. The escaped interior
		// slash matches the agent-image fork manager's literal.
		expect(manager?.managerFilePatterns).toEqual([
			"/^agent-image\\/devenv\\.lock$/",
		]);
		const delimited = /^\/(.*)\/$/.exec(
			manager?.managerFilePatterns?.[0] as string,
		);
		expect(delimited).not.toBeNull();
		const re = new RegExp(delimited?.[1] as string);
		expect(re.test(LOCK)).toBe(true);
		// NEVER the root lock: a loose pattern would collapse the two
		// independently-cadenced scopes into one dep carrying two conflicting
		// digests.
		expect(re.test("devenv.lock")).toBe(false);
		expect(re.test(`a/${LOCK}`)).toBe(false); // anchored, no arbitrary prefix
	});

	// The matchString must recover EXACTLY ONE 40-hex rev from the REAL agent-image
	// lock — the OUTER channel rev (nodes.nixpkgs.locked.rev), not the inner
	// nixpkgs-src rev, the devenv FORK rev, or the original block (which repeats the
	// repo but is followed by "ref", never "rev").
	test("matchString extracts the channel rev from the real agent-image lock", () => {
		const lockText = readFileSync(join(repoRoot, LOCK), "utf8");
		const matchString = manager?.matchStrings?.[0];
		expect(matchString).toBeDefined();
		const matches = [
			...lockText.matchAll(new RegExp(matchString as string, "g")),
		];
		expect(matches).toHaveLength(1);
		const rev = matches[0]?.groups?.currentDigest;
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
		const parsed = JSON.parse(lockText);
		expect(rev).toBe(parsed.nodes.nixpkgs.locked.rev);
		expect(rev).not.toBe(parsed.nodes["nixpkgs-src"].locked.rev);
		expect(rev).not.toBe(parsed.nodes.devenv.locked.rev);
	});

	// It must not bind the ROOT lock — the file pattern is the only thing scoping
	// it, so assert the two locks carry DIFFERENT channel revs today (RD-1) and
	// that this manager's dep is the agent-image one.
	test("the two scopes' channel revs are read independently, not reconciled", () => {
		const agent = JSON.parse(readFileSync(join(repoRoot, LOCK), "utf8"));
		const root = JSON.parse(
			readFileSync(join(repoRoot, "devenv.lock"), "utf8"),
		);
		// Both are real 40-hex channel revs…
		expect(agent.nodes.nixpkgs.locked.rev).toMatch(/^[a-f0-9]{40}$/);
		expect(root.nodes.nixpkgs.locked.rev).toMatch(/^[a-f0-9]{40}$/);
		// …and nothing in the config compares them: each scope has its own
		// manager, so a skew is legitimate rather than a drift to fix. (This
		// test states the property; it deliberately does NOT assert inequality,
		// which would fail the day the two happen to coincide.)
		const channelManagers = (cfg.customManagers ?? []).filter((m) =>
			m.matchStrings?.some((s) => s.includes("devenv-nixpkgs")),
		);
		expect(channelManagers).toHaveLength(2);
		expect(
			new Set(channelManagers.map((m) => m.managerFilePatterns?.[0])).size,
		).toBe(2);
	});

	// Solo-branched, scheduled, cooldown-nulled — its siblings' shape. The groupName
	// must be UNIQUE: it makes the branch-mode relock task safe (one slot per branch)
	// and keeps the two channel pins on independent cadences.
	test("the rule is solo-grouped, scheduled, and cooldown-exempt", () => {
		expect(rule).toBeDefined();
		expect(rule?.matchManagers).toContain("custom.regex");
		expect(rule?.groupName).toBe(GROUP);
		expect(cfg.packageRules.filter((r) => r.groupName === GROUP)).toHaveLength(
			1,
		);
		expect(rule?.schedule?.length).toBeGreaterThan(0);
		// A git-refs digest on a moving branch HEAD carries no release age, so the
		// repo-wide strict cooldown would peg it permanently `pending` and cut
		// zero PRs (the RIG-1220 silent-no-updates shape).
		expect(rule?.minimumReleaseAge).toBeNull();
	});

	// Branch-mode task over EXACTLY the two files the script writes. fileFilters is
	// an INCLUDE allowlist, so dropping entrypoint.nix would run the FOD refresh and
	// silently discard it (hash mismatch on the image build), and dropping the lock
	// would discard the relock.
	test("the postUpgradeTask is branch-mode over the lock AND the FOD file", () => {
		const task = rule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.commands).toEqual([REFRESH]);
		expect(task?.fileFilters).toEqual([LOCK, "agent-image/entrypoint.nix"]);
	});

	// A SEPARATE script from the root channel lockstep: the root script's tail
	// (biome eval, catalog, bun.lock, flake) has no counterpart here. Assert the two
	// rules do NOT share a command, so a "simplification" pointing this rule at the
	// root script (relocking the ROOT lock on an agent-image branch) fails here.
	test("does NOT reuse the root channel refresher", () => {
		expect(rule?.postUpgradeTasks?.commands).not.toContain(
			"bun tools/renovate/refresh-devenv-nixpkgs.ts",
		);
		const declaring = cfg.packageRules.filter((r) =>
			r.postUpgradeTasks?.commands?.includes(REFRESH),
		);
		expect(declaring).toHaveLength(1);
		expect(
			bot.allowedCommands?.filter((a) => new RegExp(a).test(REFRESH)),
		).toEqual(["^bun tools/renovate/refresh-agent-image-nixpkgs\\.ts$"]);
	});

	// The two locks must never land in ONE branch, and this digest must never fold
	// into the TS rollup — either puts two branch-mode tasks on one branch, where
	// Renovate builds only one and the other ships unrelocked. Replay last-match-wins
	// semantics.
	test("the digest resolves to its own solo branch, not the TS rollup", () => {
		const group = resolveGroupName({
			manager: "custom.regex",
			fileName: LOCK,
			depName: DEP,
			updateType: "digest",
		});
		expect(group).toBe(GROUP);
		expect(group).not.toBe("TypeScript dependencies");
	});

	// …and it is a DIFFERENT branch from the root channel pin and from the
	// agent-image FORK pin, the two rules it is most likely to be collapsed into.
	test("its group differs from the root channel and agent-image fork groups", () => {
		const groups = [
			GROUP,
			cfg.packageRules.find((r) =>
				r.matchDepNames?.includes("cachix/devenv-nixpkgs"),
			)?.groupName,
			cfg.packageRules.find((r) =>
				r.matchDepNames?.includes("RigelBuild/devenv-agent-image"),
			)?.groupName,
		];
		expect(new Set(groups).size).toBe(3);
	});
});

describe("tools/renovate go ↔ go-overlay lockstep (RIG-3100)", () => {
	// The packageRule coupling a go.nix toolchain bump to a go-overlay input
	// refresh — found by its command, not index.
	const goOverlayRule = cfg.packageRules.find((r) =>
		r.postUpgradeTasks?.commands?.some((c) =>
			/refresh-go-overlay\.ts$/.test(c),
		),
	);

	test("the go-overlay refresh rule matches the go dep on the custom.regex manager", () => {
		expect(goOverlayRule).toBeDefined();
		expect(goOverlayRule?.matchManagers).toContain("custom.regex");
		expect(goOverlayRule?.matchDepNames).toContain("go");
	});

	// Branch-mode task over exactly the two files it writes: devenv.lock (the devenv
	// update go-overlay re-lock) and the bun outputHash pin that re-lock invalidates
	// (devenv.lock is a trigger of the entrypoint.nix FOD entry). It must NOT rewrite
	// go.nix nor any other pin; fileFilters is an INCLUDE allowlist, so this pins it.

	// Command order is load-bearing: re-lock FIRST, so the pin is realised against
	// the written lock. Under a REVERSED order the go manager's scope never touches
	// devenv.lock, so the FOD self-gate reads CLEAN and no-ops before the re-lock
	// rewrites the lock — the refresh is silently SKIPPED, shipping the stale pin.
	test("the lockstep postUpgradeTask is branch-mode over the files it writes", () => {
		const task = goOverlayRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.fileFilters).toEqual([
			"devenv.lock",
			"agent-image/entrypoint.nix",
		]);
		expect(task?.commands).toEqual([
			"bun tools/renovate/refresh-go-overlay.ts",
			"bun tools/renovate/refresh-fod-hashes.ts",
		]);
	});

	// Solo-branch safety: the branch-mode task slot is winner-take-all per branch,
	// so this rule is safe ONLY because the go pin never shares a branch. The
	// versions/*.nix un-group rule nulls its groupName, so a go bump solo-branches
	// for BOTH minor and major (neither rule sets matchUpdateTypes).
	test.each(["minor", "major"] as const)(
		"a go pin %s bump un-groups to its own solo branch (null), not the TS rollup",
		(updateType) => {
			const group = resolveGroupName({
				manager: "custom.regex",
				fileName: "tools/toolchain/versions/go.nix",
				depName: "go",
				depType: "toolchain",
				updateType,
			});
			expect(group).toBeNull();
			expect(group).not.toBe("TypeScript dependencies");
		},
	);
});

describe("tools/renovate solo-branch grouping outcomes", () => {
	// Replay Renovate's last-match-wins packageRule semantics: a toolchain pin
	// bump and the devenv digest must NEVER resolve into the 'TypeScript
	// dependencies' rollup, or they would land in a shared branch and collide on
	// the single per-branch postUpgrade-task slot.
	test("a versions/*.nix toolchain pin minor bump un-groups (null), not the TS rollup", () => {
		const group = resolveGroupName({
			manager: "custom.regex",
			fileName: "tools/toolchain/versions/bun.nix",
			depName: "oven-sh/bun",
			depType: "toolchain",
			updateType: "minor",
		});
		expect(group).toBeNull();
		expect(group).not.toBe("TypeScript dependencies");
	});

	test("the devenv-nixpkgs digest resolves to its own solo branch, not the TS rollup", () => {
		const group = resolveGroupName({
			manager: "custom.regex",
			fileName: "devenv.lock",
			depName: "cachix/devenv-nixpkgs",
			updateType: "digest",
		});
		expect(group).toBe("devenv nixpkgs channel");
	});

	// Contrast: an ordinary catalog pin (npm, minor) DOES fold into the rollup —
	// proves the un-group rules are scoped, not blanket.
	test("an ordinary catalog pin minor bump folds into the TypeScript rollup", () => {
		const group = resolveGroupName({
			manager: "custom.regex",
			fileName: "package.json",
			depName: "astro",
			packageName: "astro",
			depType: "workspaces.catalog",
			updateType: "minor",
		});
		expect(group).toBe("TypeScript dependencies");
	});

	// The un-group rule itself: matchFileNames versions/*.nix, groupName null.
	test("a versions/*.nix un-group rule exists (groupName null)", () => {
		const rule = cfg.packageRules.find((r) =>
			r.matchFileNames?.includes("tools/toolchain/versions/*.nix"),
		);
		expect(rule).toBeDefined();
		expect(rule?.groupName).toBeNull();
	});
});

describe("tools/renovate typescript <7 fence", () => {
	// TS 7.0 (Project Corsa) ships without a stable programmatic API until 7.1, so
	// every library consumer of `typescript` breaks on 7.x. Cap the major.
	const tsRule = cfg.packageRules.find((r) =>
		r.matchPackageNames?.includes("typescript"),
	);

	test("a typescript-scoped packageRule exists", () => {
		expect(tsRule).toBeDefined();
	});

	test("caps typescript below 7 (allowedVersions '<7')", () => {
		expect(tsRule?.allowedVersions).toBe("<7");
	});

	test("covers the bun, npm, and catalog managers", () => {
		expect(tsRule?.matchManagers).toEqual(["bun", "npm", "custom.regex"]);
	});
});

describe("tools/renovate postgres + gomod go disables", () => {
	// Postgres service image is coupled to a Go const (pgtest.go); it moves only
	// via a manual two-file PR, so Renovate is disabled for it.
	test("the postgres-image disable rule exists (matchDepNames postgres, enabled false)", () => {
		const rule = cfg.packageRules.find(
			(r) => r.matchDepNames?.includes("postgres") && r.enabled === false,
		);
		expect(rule).toBeDefined();
		expect(rule?.matchDepNames).toEqual(["postgres"]);
	});

	// The gomod `go` directive tracks the go.nix pin minus at most one minor by
	// manual policy; disable Renovate's gomod go-directive update.
	test("the gomod go-directive disable rule exists (gomod + go, enabled false)", () => {
		const rule = cfg.packageRules.find(
			(r) =>
				r.matchManagers?.includes("gomod") &&
				r.matchDepNames?.includes("go") &&
				r.enabled === false,
		);
		expect(rule).toBeDefined();
		expect(rule?.matchManagers).toEqual(["gomod"]);
		expect(rule?.matchDepNames).toEqual(["go"]);
	});
});

describe("tools/renovate wails/v3 floor cap (RIG-2852, GTK4 migration)", () => {
	// The GTK4 migration record freezes a "Never v3.1" floor: wails v3.1 removes the
	// legacy GTK3 build tag, so an auto-opened v3.1 bump before the GTK4 flip would
	// strand the app. A gomod rule caps wails/v3 below v3.1 via a REGEX allowedVersions
	// (a semver range would wrongly reject the current v3.0.0 prerelease pin).
	const wailsRule = cfg.packageRules.find(
		(r) =>
			r.matchManagers?.includes("gomod") &&
			r.matchDepNames?.includes("github.com/wailsapp/wails/v3"),
	);

	test("a gomod cap rule exists for wails/v3 with a regex allowedVersions", () => {
		expect(wailsRule).toBeDefined();
		expect(wailsRule?.matchManagers).toEqual(["gomod"]);
		expect(wailsRule?.matchDepNames).toEqual(["github.com/wailsapp/wails/v3"]);
		const allowedVersions = wailsRule?.allowedVersions ?? "";
		// Slash-delimited regex form (matched against the raw version string),
		// mirroring the postgres-stack /^18$/ rule.
		expect(allowedVersions.startsWith("/")).toBe(true);
		expect(allowedVersions.endsWith("/")).toBe(true);
	});

	test("the cap admits the LIVE go.mod pin + future v3.0.x, rejects v3.1.x and v4+", () => {
		// Compile the shipped regex and replay it. The load-bearing assertion reads
		// the ACTUAL wails require line from go/go.mod and asserts the cap admits the
		// pin — so a future pairing that opens zero PRs fails HERE, tied to ground
		// truth. The boundary cases below pin the reject edge.
		const allowedVersions = wailsRule?.allowedVersions ?? "";
		const matcher = new RegExp(allowedVersions.slice(1, -1));

		const goMod = readFileSync(join(repoRoot, "go", "go.mod"), "utf8");
		const pin = goMod.match(
			/github\.com\/wailsapp\/wails\/v3\s+(?<version>\S+)/,
		)?.groups?.version;
		expect(pin).toBeDefined(); // the require line must exist
		expect(matcher.test(pin as string)).toBe(true); // the cap MUST admit it

		expect(matcher.test("v3.0.0-beta.7")).toBe(true); // a newer beta
		expect(matcher.test("v3.0.1")).toBe(true); // a future v3.0 patch
		expect(matcher.test("v3.1.0")).toBe(false); // the frozen floor
		expect(matcher.test("v3.1.0-beta.0")).toBe(false); // a v3.1 prerelease
		expect(matcher.test("v4.0.0")).toBe(false); // a future major
	});
});

describe("tools/renovate postgres-stack digest manager (RIG-2774, DL-260)", () => {
	// DefaultPostgresImage is a standalone Go const the native managers can't see; a
	// custom.regex manager surfaces it as a docker dep so postgres:18 digest rebuilds
	// flow through a reviewable PR. DL-260 freezes the major at 18, so the paired rule
	// pins allowedVersions to /^18$/. Find both by behavior, not index.
	const pgManager = cfg.customManagers?.find((m) =>
		m.managerFilePatterns?.some((p) => p.includes("postgres_image")),
	);
	const pgRule = cfg.packageRules.find(
		(r) =>
			r.matchManagers?.includes("custom.regex") &&
			r.matchDepNames?.includes("postgres-stack"),
	);

	test("a docker custom.regex manager surfaces the pin (postgres-stack, docker versioning)", () => {
		expect(pgManager).toBeDefined();
		expect(pgManager?.customType).toBe("regex");
		expect(pgManager?.datasourceTemplate).toBe("docker");
		expect(pgManager?.depNameTemplate).toBe("postgres-stack");
		expect(pgManager?.packageNameTemplate).toBe("docker.io/library/postgres");
		// Explicit docker versioning: a custom.regex manager defaults to
		// semver-coerced regardless of datasource, which mishandles a
		// <tag>@<digest> docker reference.
		expect(pgManager?.versioningTemplate).toBe("docker");
	});

	test("its regex extracts the tag + digest from the real postgres_image.go", () => {
		const src = readFileSync(
			join(repoRoot, "go", "internal", "stack", "postgres_image.go"),
			"utf8",
		);
		const pattern = pgManager?.matchStrings?.[0];
		expect(pattern).toBeDefined();
		// Exactly one qualifying pin: use matchAll (not exec) so a second
		// accidental postgres:NN@sha256 string in the Go file — which Renovate
		// would silently extract as a second dep — fails this build closed.
		const matches = [...src.matchAll(new RegExp(pattern as string, "g"))];
		expect(matches).toHaveLength(1);
		expect(matches[0]?.groups?.currentValue).toBe("18");
		expect(matches[0]?.groups?.currentDigest).toMatch(/^sha256:[a-f0-9]{64}$/);
	});

	test("the digest-only-within-18 rule exists (postgres-stack, allowedVersions /^18$/)", () => {
		expect(pgRule).toBeDefined();
		const allowedVersions = pgRule?.allowedVersions ?? "";
		expect(allowedVersions).toBe("/^18$/");
		expect(pgRule?.matchDepNames).toEqual(["postgres-stack"]);
		// No matchUpdateTypes: the version filter must apply to ALL update types so
		// an 18->19 major candidate is filtered too — scoping to `digest` would
		// leave a major unfiltered.
		expect(pgRule?.matchUpdateTypes).toBeUndefined();
		// Semantic teeth: derive the matcher from the configured value (strip the
		// /.../ delimiters) and assert it accepts 18 while rejecting a 19 major —
		// so a fat-fingered allowedVersions (e.g. /^1[89]$/) that still admits 19
		// fails here, not just a changed literal.
		const versionMatcher = new RegExp(allowedVersions.slice(1, -1));
		expect(versionMatcher.test("18")).toBe(true);
		expect(versionMatcher.test("19")).toBe(false);
	});

	// Load-bearing guard: the CI-service disable fence (matchDepNames ["postgres"],
	// enabled false) is unscoped by manager/file, so a "postgres" depName here would
	// inherit the disable and open ZERO PRs. Replay last-match-wins semantics for a
	// synthetic postgres-stack docker dep and confirm it resolves ENABLED.
	const resolveEnabled = (dep: SyntheticDep): boolean => {
		let enabled = true;
		for (const rule of cfg.packageRules) {
			if (rule.matchManagers && !rule.matchManagers.includes(dep.manager)) {
				continue;
			}
			if (
				rule.matchUpdateTypes &&
				!(dep.updateType && rule.matchUpdateTypes.includes(dep.updateType))
			) {
				continue;
			}
			if (
				rule.matchDepTypes &&
				!(dep.depType && rule.matchDepTypes.includes(dep.depType))
			) {
				continue;
			}
			if (
				rule.matchDepNames &&
				!(dep.depName && rule.matchDepNames.includes(dep.depName))
			) {
				continue;
			}
			if (
				rule.matchPackageNames &&
				!(dep.packageName && rule.matchPackageNames.includes(dep.packageName))
			) {
				continue;
			}
			if (
				rule.matchFileNames &&
				!(
					dep.fileName &&
					rule.matchFileNames.some((g) =>
						globToRegExp(g).test(dep.fileName as string),
					)
				)
			) {
				continue;
			}
			if (
				rule.excludeDepNames &&
				dep.depName &&
				rule.excludeDepNames.includes(dep.depName)
			) {
				continue;
			}
			if (typeof rule.enabled === "boolean") enabled = rule.enabled;
		}
		return enabled;
	};

	test("a postgres-stack docker dep resolves ENABLED (fence independence)", () => {
		expect(
			resolveEnabled({
				manager: "custom.regex",
				depName: "postgres-stack",
				packageName: "docker.io/library/postgres",
				fileName: "go/internal/stack/postgres_image.go",
				updateType: "digest",
			}),
		).toBe(true);
		// And the original `postgres` CI-service dep stays DISABLED — the two pins
		// remain independently governed.
		expect(
			resolveEnabled({
				manager: "github-actions",
				depName: "postgres",
				fileName: ".github/workflows/ci.yml",
				updateType: "digest",
			}),
		).toBe(false);
	});
});

describe("tools/renovate bun-types soak exemption ↔ bunfig excludes", () => {
	// The catalog-scoped soak-exemption rule governs ONLY catalog deps, so its
	// matchPackageNames must equal exactly the bunfig minimumReleaseAgeExcludes that
	// ARE catalog deps — the bun-types pair. Other excludes are npm/overrides pins
	// outside the catalog manager's reach; deriving from the manifest stays current.
	const soakRule = cfg.packageRules.find(
		(r) =>
			r.minimumReleaseAge === null &&
			r.matchDepTypes?.includes("workspaces.catalog") &&
			r.matchPackageNames?.includes("@types/bun"),
	);

	// bunfig.toml is TOML, not JSON; parse the array with a scoped regex over the
	// real file rather than pulling in a TOML dep.
	const bunfigExcludes = (): string[] => {
		const toml = readFileSync(join(repoRoot, "bunfig.toml"), "utf8");
		const block = /minimumReleaseAgeExcludes\s*=\s*\[([^\]]*)\]/.exec(toml);
		expect(block).not.toBeNull();
		return [...(block?.[1] ?? "").matchAll(/"([^"]+)"/g)].map(
			(m) => m[1] as string,
		);
	};

	// The catalog object in the root manifest is the single source of truth for
	// which names the catalog manager can reach. bun-types never appears in the
	// catalog itself (only its @types/bun parent does) but is soaked in lockstep,
	// so it joins the catalog set explicitly.
	const catalogExcludes = (): string[] => {
		const parsed = JSON.parse(
			readFileSync(join(repoRoot, "package.json"), "utf8"),
		) as { workspaces?: { catalog?: Record<string, string> } };
		const catalog = new Set(Object.keys(parsed.workspaces?.catalog ?? {}));
		catalog.add("bun-types");
		return bunfigExcludes().filter((e) => catalog.has(e));
	};

	test("the soak-exemption rule exists (custom.regex catalog, minimumReleaseAge null)", () => {
		expect(soakRule).toBeDefined();
		expect(soakRule?.minimumReleaseAge).toBeNull();
		expect(soakRule?.matchManagers).toEqual(["custom.regex"]);
	});

	test("its package names EQUAL the catalog-dep subset of bunfig excludes", () => {
		const excludes = bunfigExcludes();
		// @tanstack/virtual-core is the canonical non-catalog exclude (an overrides
		// pin) — it must be present AND must be filtered out as non-catalog.
		expect(excludes).toContain("@tanstack/virtual-core");
		const expected = catalogExcludes();
		expect(expected).not.toContain("@tanstack/virtual-core");
		expect(expected.slice().sort()).toEqual(["@types/bun", "bun-types"].sort());
		expect(soakRule?.matchPackageNames?.slice().sort()).toEqual(
			expected.slice().sort(),
		);
	});
});

describe("tools/renovate self-pin workflow (exact Renovate version)", () => {
	// The workflow must run `bunx renovate@<version>` at an EXACT pin, NOT a bare
	// `bunx renovate` (which resolves latest from npm every run — fresh
	// third-party code with a repo-write token, bypassing the soak).
	const workflow = readFileSync(
		join(repoRoot, ".github", "workflows", "renovate.yml"),
		"utf8",
	);
	// Strip YAML comment lines so the guard reads the real `run:` commands, not
	// the rationale comment that intentionally names the bare form.
	const runLines = workflow
		.split("\n")
		.filter((l) => !/^\s*#/.test(l))
		.join("\n");

	test("pins an exact Renovate version (bunx renovate@<version>)", () => {
		expect(/bunx renovate@\S+/.test(runLines)).toBe(true);
	});

	test("contains NO bare `bunx renovate` invocation", () => {
		expect(/bunx renovate(?!@)/.test(runLines)).toBe(false);
	});

	// The self-pin custom.regex manager tracks that pin line so a bump flows
	// through a reviewable PR under the normal soak.
	test("a self-pin custom.regex manager surfaces the workflow pin", () => {
		const selfPin = cfg.customManagers?.find(
			(m) =>
				m.datasourceTemplate === "npm" &&
				m.depNameTemplate === "renovate" &&
				m.managerFilePatterns?.some((p) => p.includes("renovate")),
		);
		expect(selfPin).toBeDefined();
		expect(
			selfPin?.matchStrings?.some((s) => s.includes("bunx renovate@")),
		).toBe(true);
	});

	// The preflight step probes with GH_TOKEN but classifies token-PRESENCE off
	// RENOVATE_TOKEN, so the step MUST set RENOVATE_TOKEN, or index.ts short-circuits
	// to reason="no-token" and exits 1 every run. Guard the env so that drop can't
	// silently regress.
	test("the preflight step sets RENOVATE_TOKEN in its env", () => {
		// Slice the preflight step: from its `- name: Preflight …` line to the
		// next step boundary (`- name:`/`- uses:` at step indent) or EOF.
		const lines = workflow.split("\n");
		const start = lines.findIndex((l) => /^\s*-\s+name:\s*Preflight/.test(l));
		expect(start).toBeGreaterThanOrEqual(0);
		const rest = lines.slice(start + 1);
		const endRel = rest.findIndex((l) => /^\s*-\s+(name|uses):/.test(l));
		const stepBody = (endRel === -1 ? rest : rest.slice(0, endRel)).join("\n");
		expect(/^\s*RENOVATE_TOKEN:\s*\S/m.test(stepBody)).toBe(true);
	});
});

describe("tools/renovate FOD trigger coverage (every task site, derived from FOD_ENTRIES)", () => {
	// THE CLASS THIS GUARDS. A pinned Nix FOD hash content-addresses a fetched
	// dependency set, so a change to any trigger manifest invalidates it. A task
	// naming a trigger in fileFilters may commit it, and Renovate commits ONLY listed
	// files — shipping the lock with the pin on the OLD closure (PR #580).

	// So the requirement is structural: any site that can write a declared trigger
	// must ALSO run the refresh and name that entry's FOD file plus every mirrorFile
	// (RIG-2852 Gap 1). The refresh self-gates on trigger change and fires here (a
	// full realise); only the resulting WRITE is a no-op — one extra realise, worth it.

	// Trigger sets are READ from FOD_ENTRIES, so adding a trigger or a new pinned FOD
	// re-derives the requirement over every site automatically.

	// DETECTION BOUNDARY. This reads fileFilters, so it misses a trigger written by
	// Renovate's own manager update: the guest-image vendorHash triggers on
	// go.mod/go.sum (gomod-manager-written), covered by assertion (top-level carries
	// the refresh). A future rule-level gomod task would evict that slot unflagged.

	// fileFilters entries are GLOB patterns, so coupling must be decided by glob
	// match, not string equality — otherwise a site naming a trigger under a
	// non-literal spelling is invisible here. Every entry is a literal path today,
	// for which glob and equality coincide.
	const covers = (filters: string[], path: string): boolean =>
		filters.some((filter) => new Bun.Glob(filter).match(path));

	// Every declared task site, enumerated as allDeclaredCommands does: the
	// top-level task plus every packageRule-level one. Labelled so a failure names
	// the offending site.
	const taskSites: { label: string; task: PostUpgradeTasks }[] = [];
	if (cfg.postUpgradeTasks) {
		taskSites.push({ label: "top-level", task: cfg.postUpgradeTasks });
	}
	cfg.packageRules.forEach((rule, i) => {
		const task = rule.postUpgradeTasks;
		if (!task) return;
		const group = rule.groupName;
		taskSites.push({
			label:
				group != null ? `packageRules[${i}] (${group})` : `packageRules[${i}]`,
			task,
		});
	});

	// The (site, entry) pairs the coupling actually binds: a site whose declared
	// write-set names at least one of that entry's triggers.
	const coupled = taskSites.flatMap(({ label, task }) => {
		const filters = task.fileFilters ?? [];
		return FOD_ENTRIES.filter((entry) =>
			entry.triggers.some((trigger) => covers(filters, trigger)),
		).map((entry) => ({ label, task, entry }));
	});

	test("the coupled (site, entry) set has its expected shape (guard is not vacuous)", () => {
		// The guard iterates coupled, so a thinned set would pass checking nothing.
		// entrypoint.nix is carried by TWO entries sharing one trigger list, so
		// renaming one trigger dissolves many pairings while the count stays positive.
		// Pinning the exact count catches that; a new pairing updates this number.

		// 12 = six sites naming a trigger of the two entrypoint.nix entries (2 × 6),
		// plus the guestd vendorHash entry's zero pairs — its triggers go.mod/go.sum
		// are written by the gomod MANAGER and named by no fileFilters.
		expect(coupled.length).toBe(12);
		expect(taskSites.length).toBeGreaterThan(0);
	});

	// Violation messages, built outside the scan loop so the loop below stays the
	// three predicates it checks.
	const missingRefresh = (context: string): string =>
		`${context}, but does NOT run '${FOD_COMMAND}'. A task that commits ` +
		`a trigger change without recomputing the pin ships a lockfile ` +
		`change beside a hash addressing the OLD closure — the image ` +
		`build then fails 'hash mismatch in fixed-output derivation'. ` +
		`Append the refresh AFTER the command that writes the trigger ` +
		`(the pin must be realised against the written file, not the ` +
		`still-at-base one), or drop the trigger from fileFilters.`;

	const refreshNotLast = (
		context: string,
		position: number,
		total: number,
	): string =>
		`${context}, but runs '${FOD_COMMAND}' at position ${position} ` +
		`of ${total} instead of LAST. The refresh realises the ` +
		`pin against the trigger files as they stand on disk, so it must ` +
		`run after EVERY command that writes a trigger. A refresh sitting ` +
		`before a relock either realises against the still-at-base file ` +
		`and then has that file rewritten underneath it, or — when no ` +
		`earlier command has moved a trigger yet — self-gates clean and ` +
		`no-ops entirely without realising anything. Either way the bump ` +
		`ships the stale pin. Move the refresh to the end of commands.`;

	const pinDropped = (context: string, required: string): string =>
		`${context}, but its fileFilters omit '${required}'. ` +
		`fileFilters is an INCLUDE allowlist, so the refreshed pin is ` +
		`recomputed and then silently DROPPED, and the bump lands with ` +
		`the stale pin exactly as if no refresh ran.`;

	// A command that refreshes the FOD pin IN-PROCESS rather than by shelling
	// FOD_COMMAND. A script importing refreshFodEntries satisfies the guard; adding
	// the command beside it would pay a SECOND realise. Each entry lists the call site
	// that makes it true, so a script added without it fails the assertion.
	const IN_PROCESS_REFRESHERS: Record<string, string> = {
		// refresh-agent-image-nixpkgs.ts: relocks the agent-image channel, then
		// awaits refreshFodEntries(agentImageFodEntries()) — the authoritative write
		// and its verify sibling, in the order refreshFodEntries enforces.
		"bun tools/renovate/refresh-agent-image-nixpkgs.ts": "refreshFodEntries",
	};

	test("every declared in-process refresher really drives the FOD table", async () => {
		// Guards the exemption itself: the claim above is only sound while each
		// listed script genuinely calls the refresher, so read the source and check.
		// Without this, a later edit could strip the call and the site would keep
		// its pass through this table alone.
		for (const [command, symbol] of Object.entries(IN_PROCESS_REFRESHERS)) {
			const script = command.replace(/^bun /, "");
			const source = await Bun.file(join(repoRoot, script)).text();
			const calls = new RegExp(`\\b(await\\s+)?${symbol}\\s*\\(`).test(source);
			expect(calls, `${script} must call ${symbol}()`).toBe(true);
		}
	});

	// Every way a single (site, entry) pairing can fail the coupling: the refresh
	// missing, the refresh not last, or a file the pin needs missing from the
	// include allowlist.
	const violationsFor = ({
		label,
		task,
		entry,
	}: (typeof coupled)[number]): string[] => {
		const filters = task.fileFilters ?? [];
		const commands = task.commands ?? [];
		const named = entry.triggers.filter((t) => covers(filters, t));
		const context =
			`${label} declares FOD trigger(s) ${named.join(", ")} for the pin in ` +
			`${entry.file}`;
		const found: string[] = [];
		// A site refreshes the pin either by shelling FOD_COMMAND — which must run
		// LAST, after whatever wrote the trigger — or by running a script that
		// drives the table in-process, which orders the two itself.
		const inProcess = commands.filter((c) => c in IN_PROCESS_REFRESHERS);
		const fodIndex = commands.indexOf(FOD_COMMAND);
		if (fodIndex !== -1) {
			if (fodIndex !== commands.length - 1) {
				found.push(refreshNotLast(context, fodIndex + 1, commands.length));
			}
		} else if (inProcess.length === 0) {
			found.push(missingRefresh(context));
		}
		for (const required of [entry.file, ...(entry.mirrorFiles ?? [])]) {
			if (!covers(filters, required)) {
				found.push(pinDropped(context, required));
			}
		}
		return found;
	};

	test("every task site naming a FOD trigger runs the refresh LAST and commits the pin", () => {
		expect(coupled.flatMap(violationsFor)).toEqual([]);
	});
});
