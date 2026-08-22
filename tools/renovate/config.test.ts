import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import botConfig from "./bot-config.json5";
import config from "./config.json5";

// Guard suite for compass's self-hosted Renovate config (RIG-2432). Ported from
// orion's ci/renovate/config.test.ts and adapted to compass's config +
// ecosystems (bun catalog, devenv-nixpkgs channel, toolchain pins, gomod, GitHub
// Actions; NO rust/cargo, pulumi, woodpecker, nix-manager, or markdownlint
// catalog — dropped, compass has none). The .json5 configs load via Bun's native
// JSON5 import loader, exactly as orion's suite loads them.

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
};
// tools/renovate/ → repo root is two levels up.
const repoRoot = join(import.meta.dir, "..", "..");

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
	// postUpgradeTasks.commands are gated by the BOT config's global
	// `allowedCommands` allowlist (a repo config cannot self-authorize a command),
	// which Renovate matches UNANCHORED via regEx(pattern).test(cmd). So each
	// entry's `^…$` IS the security property. Compass has exactly 3 commands and 3
	// allowlist entries; every command must be permitted, every entry must be used,
	// and no entry may be an unanchored substring rule.
	const commands = allDeclaredCommands();
	const allowed = bot.allowedCommands ?? [];

	test("declares exactly three postUpgrade commands and three allowlist entries", () => {
		expect(commands).toHaveLength(3);
		expect(allowed).toHaveLength(3);
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
	});

	test("permits exactly the three RIG-2432 commands", () => {
		expect(commands.sort()).toEqual(
			[
				"bun install --lockfile-only",
				"bun tools/renovate/refresh-devenv-nixpkgs.ts",
				"bun tools/renovate/refresh-toolchain-hashes.ts",
			].sort(),
		);
	});
});

describe("tools/renovate OSV vuln source honors the fork fence", () => {
	// Renovate owns security remediation. OSV is the config-driven vuln source
	// that respects the forks/*/** disable packageRule, unlike a repo-wide toggle.
	test("osvVulnerabilityAlerts is enabled", () => {
		expect(cfg.osvVulnerabilityAlerts).toBe(true);
	});

	// INVARIANT: a vuln fix is injected as a packageRule carrying
	// `force: { ...vulnerabilityAlerts }`, and applyPackageRules clears a prior
	// skipReason when force.enabled is truthy — which would CANCEL the forks/*/**
	// disable and re-open fork bumps. The default vulnerabilityAlerts object has no
	// `enabled` key, so the fence holds; assert it is absent (never true).
	test("does NOT set vulnerabilityAlerts.enabled (would re-open fork bumps)", () => {
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
	// find it by file pattern, not index.
	const devenvManager = cfg.customManagers?.find((m) =>
		m.managerFilePatterns?.some((p) => p.includes("devenv")),
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

	// Branch-mode lockstep task over exactly the three files the script writes
	// (compass has NO committed inner-rev guard file, unlike orion's fourth entry).
	test("the lockstep postUpgradeTask is branch-mode over the written files", () => {
		const task = devenvRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.fileFilters).toEqual([
			"devenv.lock",
			"package.json",
			"bun.lock",
		]);
		expect(task?.commands?.length).toBeGreaterThan(0);
		expect(
			task?.commands?.every((c) =>
				/^bun tools\/renovate\/refresh-devenv-nixpkgs\.ts$/.test(c),
			),
		).toBe(true);
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

describe("tools/renovate fork fence (last packageRule, last-match-wins)", () => {
	// Vendored fork subtrees (forks/<name>/) are upstream code; Renovate must open
	// no bump PRs there. A disable packageRule scoped to forks/*/**.
	const rules = cfg.packageRules;
	const forkRule = rules.find((r) =>
		r.matchFileNames?.some((f) => f.startsWith("forks/")),
	);

	test("a fork-scoped packageRule exists and disables Renovate (enabled: false)", () => {
		expect(forkRule).toBeDefined();
		expect(forkRule?.enabled).toBe(false);
	});

	test("scopes to the subtree glob forks/*/**, not the whole root", () => {
		expect(forkRule?.matchFileNames).toEqual(["forks/*/**"]);
	});

	// Renovate is last-match-wins: the fence is only authoritative if it is the
	// LAST packageRule. A later re-enable (enabled:true or force.enabled:true)
	// would cancel it — being last makes that structurally impossible.
	test("the fork fence is the LAST packageRule", () => {
		expect(rules[rules.length - 1]).toBe(forkRule);
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

describe("tools/renovate bun-types soak exemption ↔ bunfig excludes", () => {
	// The soak-exemption packageRule for the bun types packages must equal
	// bunfig.toml's minimumReleaseAgeExcludes MINUS @tanstack/virtual-core (which
	// is an overrides pin, outside the catalog manager's reach) — i.e. exactly
	// ["@types/bun","bun-types"]. Read the real bunfig, parse its exclude list.
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

	test("the soak-exemption rule exists (custom.regex catalog, minimumReleaseAge null)", () => {
		expect(soakRule).toBeDefined();
		expect(soakRule?.minimumReleaseAge).toBeNull();
		expect(soakRule?.matchManagers).toEqual(["custom.regex"]);
	});

	test("its package names EQUAL bunfig excludes minus @tanstack/virtual-core", () => {
		const excludes = bunfigExcludes();
		expect(excludes).toContain("@tanstack/virtual-core");
		const expected = excludes.filter((e) => e !== "@tanstack/virtual-core");
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
});
