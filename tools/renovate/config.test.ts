import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import botConfig from "./bot-config.json5";
import config from "./config.json5";
import {
	CLOSURE_TARGETS,
	REVIEWABLE_CUT_VERIFIED_AT,
} from "./nix-package-diff.core.ts";
// Import the declared FOD entries so the coverage guard derives requirements
// from the shipped table instead of duplicating it.
import { FOD_ENTRIES } from "./refresh-fod-hashes.ts";

// Guard Renovate configuration and supported ecosystems using Bun's JSON5 loader.

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
	ignoreUnstable?: boolean;
	respectLatest?: boolean;
	groupName?: string | null;
	schedule?: string[];
	minimumReleaseAge?: string | null;
	dependencyDashboardApproval?: boolean;
	enabled?: boolean;
	force?: { enabled?: boolean };
	postUpgradeTasks?: PostUpgradeTasks;
	commitMessageTopic?: string;
	group?: { commitMessageTopic?: string };
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
	commitMessageTopic?: string;
};
type RenovateConfig = {
	extends: string[];
	timezone?: string;
	schedule?: string[];
	lockFileMaintenance?: { enabled?: boolean; schedule?: string[] };
	rebaseWhen?: string;
	prHourlyLimit?: number;
	minimumReleaseAge?: string;
	packageRules: PackageRule[];
	osvVulnerabilityAlerts?: boolean;
	vulnerabilityAlerts?: { enabled?: boolean };
	enabledManagers?: string[];
	customManagers?: CustomManager[];
	postUpgradeTasks?: PostUpgradeTasks;
	postUpdateOptions?: string[];
};

const cfg = config as RenovateConfig;
const bot = botConfig as {
	allowedCommands?: string[];
	customEnvVariables?: Record<string, string>;
	executionTimeout?: number;
};
// tools/renovate/ → repo root is two levels up.
const repoRoot = join(import.meta.dir, "..", "..");

describe("tools/renovate nix lock commit message topics", () => {
	const matchingFile = (manager: CustomManager, file: string): boolean =>
		manager.managerFilePatterns?.some((pattern) => {
			const match = /^\/(.*)\/$/.exec(pattern);
			return match !== null && new RegExp(match[1] ?? "").test(file);
		}) ?? false;

	const ruleForManager = (manager: CustomManager): PackageRule | undefined =>
		cfg.packageRules.find((candidate) =>
			candidate.matchDepNames?.includes(manager.depNameTemplate ?? ""),
		);

	const lockDepsForRule = (rule: PackageRule): string[] =>
		(rule.matchDepNames ?? []).filter((name) =>
			(cfg.customManagers ?? []).some(
				(candidate) =>
					candidate.depNameTemplate === name &&
					CLOSURE_TARGETS.some((target) =>
						matchingFile(candidate, target.lock),
					),
			),
		);

	test("every lock manager has a target-labeled topic, including multi-dep groups", () => {
		const managers = (cfg.customManagers ?? []).filter(
			(manager) => manager.depNameTemplate,
		);
		for (const target of CLOSURE_TARGETS) {
			const matchingManagers = managers.filter((manager) =>
				matchingFile(manager, target.lock),
			);
			for (const manager of matchingManagers) {
				const rule = ruleForManager(manager);
				expect(
					rule,
					`${manager.depNameTemplate} needs a packageRule`,
				).toBeDefined();
				if (!rule) continue;
				expect(rule.commitMessageTopic).toContain(target.label);
				if (lockDepsForRule(rule).length > 1) {
					expect(rule.group?.commitMessageTopic).toContain(target.label);
				}
			}
		}
	});
});
// The FOD-hash refresh command, declared once. It rides SIX task sites in
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

// An absent match* list passes every dep; a present one needs the dep's value in it.
const listAdmits = (list: string[] | undefined, value: string | undefined) =>
	!list || (!!value && list.includes(value));

// Renovate applies packageRules top-to-bottom, last-match-wins. Both replays
// below fold over this one gate list so a new match* key cannot reach only one.
const ruleMatches = (
	rule: (typeof cfg.packageRules)[number],
	dep: SyntheticDep,
): boolean =>
	listAdmits(rule.matchManagers, dep.manager) &&
	listAdmits(rule.matchUpdateTypes, dep.updateType) &&
	listAdmits(rule.matchDepTypes, dep.depType) &&
	listAdmits(rule.matchDepNames, dep.depName) &&
	listAdmits(rule.matchPackageNames, dep.packageName) &&
	(!rule.matchFileNames ||
		(!!dep.fileName &&
			rule.matchFileNames.some((g) =>
				globToRegExp(g).test(dep.fileName as string),
			))) &&
	!(dep.depName && rule.excludeDepNames?.includes(dep.depName));

// Renovate applies packageRules top-to-bottom, last-match-wins. Replaying one
// arbitrary key lets guards cover both grouping and rule-level cooldown overrides.
const resolveRuleValue = <K extends keyof PackageRule>(
	dep: SyntheticDep,
	key: K,
): PackageRule[K] | undefined => {
	let value: PackageRule[K] | undefined;
	for (const rule of cfg.packageRules) {
		if (!ruleMatches(rule, dep)) continue;
		if (key in rule) value = rule[key];
	}
	return value;
};

const resolveGroupName = (dep: SyntheticDep) =>
	resolveRuleValue(dep, "groupName");

describe("tools/renovate postUpgradeTasks ↔ allowedCommands (RIG-2432)", () => {
	// Allowlist patterns must be anchored because Renovate tests them against commands.
	const commands = allDeclaredCommands();
	const distinctCommands = [...new Set(commands)];
	const allowed = bot.allowedCommands ?? [];

	test("declares ten DISTINCT postUpgrade commands and ten allowlist entries", () => {
		expect(distinctCommands).toHaveLength(10);
		expect(allowed).toHaveLength(10);
	});
	test("the fod-hash refresh is declared at all six task sites", () => {
		// Every task slot that changes an FOD trigger must refresh and commit its pin.
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

	test("permits exactly the ten declared commands", () => {
		expect(distinctCommands.sort()).toEqual(
			[
				"bun install --lockfile-only",
				"bun tools/guest-image/pin-agent-image.ts --relock",
				"bun tools/renovate/refresh-agent-image-nixpkgs.ts",
				"bun tools/renovate/refresh-devenv-lock.ts",
				"bun tools/renovate/refresh-devenv-nixpkgs.ts",
				"bun tools/renovate/refresh-fod-hashes.ts",
				"bun tools/renovate/refresh-go-overlay.ts",
				"bun tools/renovate/refresh-toolchain-hashes.ts",
				"bun tools/renovate/refresh-biome-catalog.ts",
				"devenv update meissa",
			].sort(),
		);
	});
});

describe("tools/renovate FOD-hash refresh wiring (PR #579)", () => {
	// Renovate commits only fileFilters, so include every recomputed FOD pin.
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
		// Commit Go's vendorHash mirror and the Bun FOD, plus any UI pin a bun bump moves.
		expect(topLevel?.fileFilters).toContain("apps/ui/dist.nix");
		expect(topLevel?.fileFilters).toContain("guest-image/default.nix");
		expect(topLevel?.fileFilters).toContain("flake.nix");
		expect(topLevel?.fileFilters).toContain("agent-image/entrypoint.nix");
	});

	test("the top-level task commits every Go vendorHash mirror the refresher writes", () => {
		// gomod writes go.mod itself, so the derived guard below never checks this site.
		const go = FOD_ENTRIES.find((e) => e.file === "guest-image/default.nix");
		expect(go?.mirrorFiles?.length).toBeGreaterThan(0);
		for (const mirror of go?.mirrorFiles ?? []) {
			expect(topLevel?.fileFilters).toContain(mirror);
		}
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

	// Renovate's forced alerts can reopen disabled bumps; leave enabled unset.
	test("does NOT set vulnerabilityAlerts.enabled (would re-open disabled bumps)", () => {
		expect(cfg.vulnerabilityAlerts?.enabled).toBeUndefined();
		expect(cfg.vulnerabilityAlerts?.enabled).not.toBe(true);
	});
});

describe("tools/renovate gomod bumps pass the go.sum tidy gate", () => {
	test("gomodTidy runs after patch and minor gomod updates", () => {
		expect(cfg.enabledManagers).toContain("gomod");
		expect(cfg.postUpdateOptions).toContain("gomodTidy");
	});
});

describe("tools/renovate extends", () => {
	// SHA-pin maintenance: helpers:pinGitHubActionDigests pins every workflow
	// `uses:` to a commit SHA on sight (RIG-2432), going beyond Dependabot.
	test("extends includes helpers:pinGitHubActionDigests", () => {
		expect(cfg.extends).toContain("helpers:pinGitHubActionDigests");
	});

	// A Renovate schedule window opens zero PRs once GitHub starts the cron hours
	// late, so the workflow's daily cron alone sets the cadence.
	test("sets no Renovate schedule window, globally or per rule", () => {
		expect(
			cfg.extends.filter(
				(p) => p.startsWith("schedule:") || p.startsWith(":timezone("),
			),
		).toEqual([]);
		expect(cfg.timezone).toBeUndefined();
		expect(cfg.schedule).toBeUndefined();
		expect(cfg.lockFileMaintenance).toBeUndefined();
		expect(cfg.packageRules.filter((r) => "schedule" in r)).toEqual([]);
	});

	// The run is daily, so any nonzero hourly cap starves every update past it.
	test("sets no hourly PR cap", () => {
		expect(cfg.prHourlyLimit).toBe(0);
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
	// Find the channel manager by depName; fork managers also match these locks.
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
	// devenv.lock, and it must be root's OUTER devenv-nixpkgs channel rev
	// (root.inputs.nixpkgs), not its inner src rev, and not Meissa's channel.
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
		const { nodes } = JSON.parse(lockText);
		const channel = nodes[nodes.root.inputs.nixpkgs];
		expect(rev).toBe(channel.locked.rev);
		expect(rev).not.toBe(nodes[channel.inputs["nixpkgs-src"]].locked.rev);
		// The anchor names the channel node, so it must not bind Meissa's own
		// devenv-nixpkgs node even if both share a rev on some day.
		const meissaChannelKey = nodes[nodes.root.inputs.meissa].inputs.nixpkgs;
		expect(meissaChannelKey).not.toBe(nodes.root.inputs.nixpkgs);
		expect(matches[0]?.[0]).toContain(`"${nodes.root.inputs.nixpkgs}": {`);
	});

	// Solo-branched: its own unique groupName so the branch-mode lockstep task owns
	// the single per-branch task slot; cooldown-nulled (a git-refs digest carries
	// no release age, so a strict cooldown would defer it forever).
	test("the devenv rule is solo-grouped and cooldown-exempt", () => {
		expect(devenvRule).toBeDefined();
		expect(devenvRule?.matchDepNames).toContain("cachix/devenv-nixpkgs");
		expect(devenvRule?.groupName).toBe("devenv nixpkgs channel");
		const sharing = cfg.packageRules.filter(
			(r) => r.groupName === "devenv nixpkgs channel",
		);
		expect(sharing).toHaveLength(1);
		expect(devenvRule?.minimumReleaseAge).toBeNull();
	});

	// Branch-mode lockstep task over the files the tasks write: devenv.lock (step
	// 2), flake.nix + flake.lock (step 3), package.json + bun.lock + biome.json
	// files + reformatted sources (the biome catalog writer), and the FOD pins.

	// The FOD refresh is required: devenv.lock is a declared FOD trigger and,
	// when the biome pin moves, the writer re-resolves the bun.lock closure —
	// either can move the outputHash (PR #580 failed on this). refresh-fod-hashes.ts
	// runs LAST and gates on bun.lock OR devenv.lock.

	// Order is load-bearing and silent when wrong: the devenv.lock trigger makes the
	// FOD gate in either order, so a reversed order realises the FOD against the
	// still-at-base bun.lock, then the writer rewrites bun.lock underneath — committing a
	// pin over the OLD closure. The pinned toEqual below turns that reversal red.
	test("the lockstep postUpgradeTask is branch-mode, runs relock-then-FOD, and commits every written file", () => {
		const task = devenvRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		// `biome format --write .` can touch any source file, so the filter is the
		// catch-all — the same as the Meissa rule, which runs the same writer.
		expect(task?.fileFilters).toEqual(["**/*"]);
		// Silent-drop guard: fileFilters is an INCLUDE allowlist — Renovate commits
		// ONLY matching files. Every file a task here writes must match, or a
		// channel bump ships with that write dropped (flake skew → flake-parity
		// red; stale pin → hash mismatch) while the scripts' own tests stay green.
		const filters = task?.fileFilters ?? [];
		for (const path of [
			"devenv.lock",
			"flake.nix",
			"flake.lock",
			"package.json",
			"bun.lock",
			"biome.json",
			"tools/ci-matrix/biome.json",
			"apps/ui/src/main.ts",
			"agent-image/entrypoint.nix",
			"apps/ui/dist.nix",
		]) {
			expect(filters.some((f) => new Bun.Glob(f).match(path))).toBe(true);
		}
		expect(task?.commands).toEqual([
			"bun tools/renovate/refresh-devenv-nixpkgs.ts",
			"bun tools/renovate/refresh-biome-catalog.ts",
			"bun tools/renovate/refresh-fod-hashes.ts",
		]);
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

describe("tools/renovate Meissa lint toolchain lockstep", () => {
	// Meissa supplies the dev shell's biome + rumdl. Its manager surfaces the
	// devenv.lock meissa rev; its solo rule relocks it and moves the biome
	// catalog pin to Meissa's biome in the same PR.
	const MEISSA = "RigelBuild/meissa";
	const RELOCK = "devenv update meissa";
	const WRITER = "bun tools/renovate/refresh-biome-catalog.ts";
	const manager = cfg.customManagers?.find((m) => m.depNameTemplate === MEISSA);
	const rule = cfg.packageRules.find((r) => r.matchDepNames?.includes(MEISSA));
	const channelRule = cfg.packageRules.find(
		(r) => r.groupName === "devenv nixpkgs channel",
	);
	const lockText = readFileSync(join(repoRoot, "devenv.lock"), "utf8");

	test("declares a git-refs regex manager scoped to the root devenv.lock", () => {
		expect(manager).toMatchObject({
			customType: "regex",
			datasourceTemplate: "git-refs",
			packageNameTemplate: "https://github.com/RigelBuild/meissa",
			currentValueTemplate: "main",
		});
		const delimited = /^\/(.*)\/$/.exec(
			manager?.managerFilePatterns?.[0] ?? "",
		);
		const re = new RegExp(delimited?.[1] as string);
		expect(re.test("devenv.lock")).toBe(true);
		expect(re.test("agent-image/devenv.lock")).toBe(false);
	});

	// Exactly one match, and it is the meissa node's locked rev — not a nixpkgs
	// node Meissa drags in, and not the `original` block.
	test("matchString extracts the meissa rev, uniquely, from the real lock", () => {
		const matches = [
			...lockText.matchAll(
				new RegExp(manager?.matchStrings?.[0] as string, "g"),
			),
		];
		expect(matches).toHaveLength(1);
		const { nodes } = JSON.parse(lockText);
		expect(matches[0]?.groups?.currentDigest).toBe(
			nodes[nodes.root.inputs.meissa].locked.rev,
		);
	});

	// A `follows` would put Meissa's linters on compass's nixpkgs, so the shell
	// would run a different biome/rumdl than every other Meissa consumer.
	test("the meissa lock input has no follows (Meissa's own nixpkgs)", () => {
		const { nodes } = JSON.parse(lockText);
		const meissa = nodes[nodes.root.inputs.meissa];
		for (const target of Object.values(meissa.inputs ?? {})) {
			expect(typeof target).toBe("string"); // an array is a `follows` path
		}
		expect(meissa.inputs.nixpkgs).not.toBe(nodes.root.inputs.nixpkgs);
	});

	test("the rule is solo-grouped, daily, and cooldown-exempt", () => {
		expect(rule).toBeDefined();
		expect(rule?.matchManagers).toEqual(["custom.regex"]);
		expect(rule?.groupName).toBe("meissa lint toolchain");
		expect(
			cfg.packageRules.filter((r) => r.groupName === rule?.groupName),
		).toHaveLength(1);
		expect(rule?.schedule).toEqual(channelRule?.schedule);
		expect(rule?.minimumReleaseAge).toBeNull();
		expect(
			resolveGroupName({
				manager: "custom.regex",
				fileName: "devenv.lock",
				depName: MEISSA,
				updateType: "digest",
			}),
		).toBe("meissa lint toolchain");
	});

	// Relock before the writer reads devenv.lock, and the FOD refresh last so it
	// realises against both writes. The filter must cover every file biome
	// format can rewrite, so it is the catch-all.
	test("the task relocks, then writes the catalog, then refreshes FODs", () => {
		const task = rule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.commands).toEqual([RELOCK, WRITER, FOD_COMMAND]);
		expect(task?.fileFilters).toEqual(["**/*"]);
		for (const path of [
			"devenv.lock",
			"package.json",
			"bun.lock",
			"biome.json",
			"tools/ci-matrix/biome.json",
			"apps/ui/src/main.ts",
			".github/workflows/ci.yml",
		]) {
			expect(new Bun.Glob("**/*").match(path)).toBe(true);
		}
	});

	// The channel rule keeps its own relock, then re-checks the pin with the same
	// writer, so a channel bump can never strand a drifted pin.
	test("the devenv-nixpkgs rule runs the writer after its refresh", () => {
		const commands = channelRule?.postUpgradeTasks?.commands ?? [];
		expect(commands.indexOf(WRITER)).toBe(
			commands.indexOf("bun tools/renovate/refresh-devenv-nixpkgs.ts") + 1,
		);
	});

	test("both commands have exactly one anchored allowlist entry", () => {
		for (const [command, entry] of [
			[RELOCK, "^devenv update meissa$"],
			[WRITER, "^bun tools/renovate/refresh-biome-catalog\\.ts$"],
		] as const) {
			expect(
				bot.allowedCommands?.filter((a) => new RegExp(a).test(command)),
			).toEqual([entry]);
			expect(
				bot.allowedCommands?.some((a) => new RegExp(a).test(`${command}; id`)),
			).toBe(false);
		}
	});
});

describe("tools/renovate devenv fork currency (RIG-2815, RIG-2546 T7)", () => {
	// Separate managers track both fork locks; one rule relocks each changed scope.
	const RELOCK = "bun tools/renovate/refresh-devenv-lock.ts";
	const GROUP = "devenv fork";
	const forkRule = cfg.packageRules.find((r) => r.groupName === GROUP);
	const forkScopes: {
		label: string;
		depName: string;
		lock: string;
		patternLiteral: string;
	}[] = [
		{
			label: "root",
			depName: "RigelBuild/devenv",
			lock: "devenv.lock",
			patternLiteral: "/^devenv\\.lock$/",
		},
		{
			label: "agent-image",
			depName: "RigelBuild/devenv-agent-image",
			lock: "agent-image/devenv.lock",
			patternLiteral: "/^agent-image\\/devenv\\.lock$/",
		},
	];
	const managerFor = (depName: string) =>
		cfg.customManagers?.find((m) => m.depNameTemplate === depName);

	test.each(forkScopes)(
		"declares a git-refs regex manager for the $label lock's fork rev",
		({ depName, lock, patternLiteral }) => {
			const manager = managerFor(depName);
			expect(manager).toBeDefined();
			expect(manager?.customType).toBe("regex");
			expect(manager?.datasourceTemplate).toBe("git-refs");
			// Both managers share the packageName cache key, so Renovate resolves
			// the moving fork HEAD once and gives both upgrades the same newDigest.
			expect(manager?.packageNameTemplate).toBe(
				"https://github.com/RigelBuild/devenv",
			);
			// Both locks name rigel-release, the tracked branch for this fork.
			expect(manager?.currentValueTemplate).toBe("rigel-release");

			// Pin the literal before re-parsing it to catch delimiter changes.
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

	// Require one match for the fork revision, not another node's revision.
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
			// Not the devenv-nixpkgs channel rev — a "repo": "devenv" prefix match
			// against "devenv-nixpkgs" is the exact mis-bind the trailing quote in
			// the anchor prevents.
			for (const node of Object.values<{
				locked?: { repo?: string; rev?: string };
			}>(parsed.nodes)) {
				if (node.locked?.repo === "devenv-nixpkgs") {
					expect(rev).not.toBe(node.locked.rev);
				}
			}
		},
	);

	// One fork rule matches both depNames, owns the branch-mode task slot, and
	// disables the inapplicable release-age cooldown for the moving git ref.
	test("one fork rule matches both depNames, solo-grouped and cooldown-exempt", () => {
		expect(forkRule?.matchManagers).toContain("custom.regex");
		expect(forkRule?.matchDepNames).toEqual(
			forkScopes.map((scope) => scope.depName),
		);
		expect(
			cfg.packageRules.filter((rule) => rule.groupName === GROUP),
		).toHaveLength(1);
		expect(
			cfg.packageRules.filter((rule) =>
				rule.matchDepNames?.some((depName) =>
					forkScopes.some((scope) => scope.depName === depName),
				),
			),
		).toHaveLength(1);
		expect(forkRule?.minimumReleaseAge).toBeNull();
	});

	// Branch-mode task relocks both changed locks before refreshing the FOD pin.
	// fileFilters is an INCLUDE allowlist; all three files written by these steps
	// must be named so Renovate commits both relocks and the refreshed pin.
	test("the fork task relocks, then refreshes the FOD pin, branch-mode, over the three files it writes", () => {
		const task = forkRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.commands).toEqual([RELOCK, FOD_COMMAND]);
		expect(task?.fileFilters).toEqual([
			"devenv.lock",
			"agent-image/devenv.lock",
			"agent-image/entrypoint.nix",
		]);
	});

	// One rule declares this command, so the single anchored allowlist entry
	// authorizes it.
	test("one rule declares the relock command (one allowlist entry)", () => {
		const declaring = cfg.packageRules.filter((r) =>
			r.postUpgradeTasks?.commands?.includes(RELOCK),
		);
		expect(declaring).toHaveLength(1);
		expect(
			bot.allowedCommands?.filter((a) => new RegExp(a).test(RELOCK)),
		).toEqual(["^bun tools/renovate/refresh-devenv-lock\\.ts$"]);
	});

	// Replay last-match-wins packageRule semantics for each digest: both scopes
	// resolve to the one fork group, not the TypeScript rollup.
	test.each(forkScopes)(
		"the $label fork digest resolves to the one fork group, not the TS rollup",
		({ depName, lock }) => {
			const group = resolveGroupName({
				manager: "custom.regex",
				fileName: lock,
				depName,
				updateType: "digest",
			});
			expect(group).toBe(GROUP);
			expect(group).not.toBe("TypeScript dependencies");
		},
	);

	// Pin a longer timeout so a cold fork build cannot commit before relocking.
	test("bot-config sets an executionTimeout covering a cold fork build", () => {
		expect(typeof bot.executionTimeout).toBe("number");
		// Comfortably above the 15-min default; a cold from-source fork build can
		// exceed 15 min on a hosted runner.
		expect(bot.executionTimeout).toBeGreaterThanOrEqual(30);
	});

	// Nix ignores the fork's nixConfig here; require the workflow's cache settings.
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

	// Both fork managers retain the SAME matchStrings literal. A one-sided edit
	// would drift silently, so assert they remain equal.
	test("both fork managers declare the IDENTICAL matchString literal", () => {
		const literals = forkScopes.map(
			(s) => managerFor(s.depName)?.matchStrings?.[0],
		);
		expect(literals.every((l) => typeof l === "string")).toBe(true);
		expect(new Set(literals).size).toBe(1);
	});
});

describe("tools/renovate devenv nixpkgs channel: agent base image", () => {
	// Find the agent-image channel manager by depName; the fork manager shares its lock.
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
		// differs, so channel rules remain independently governed.
		expect(manager?.packageNameTemplate).toBe(
			"https://github.com/cachix/devenv-nixpkgs",
		);
		expect(manager?.currentValueTemplate).toBe("rolling");

		// Pin the LITERAL first (a Renovate delimiter-semantics drift then fails
		// as a changed literal rather than silently changing what the re-parse
		// below tests), then re-parse it behaviourally. The escaped interior
		// slash matches the fork manager's literal.
		expect(manager?.managerFilePatterns).toEqual([
			"/^agent-image\\/devenv\\.lock$/",
		]);
		const delimited = /^\/(.*)\/$/.exec(
			manager?.managerFilePatterns?.[0] as string,
		);
		expect(delimited).not.toBeNull();
		const re = new RegExp(delimited?.[1] as string);
		expect(re.test(LOCK)).toBe(true);
		// NEVER the root lock; the agent-image channel manager remains file-scoped.
		expect(re.test("devenv.lock")).toBe(false);
		expect(re.test(`a/${LOCK}`)).toBe(false); // anchored, no arbitrary prefix
	});

	// Extract the outer channel revision, not the nested nixpkgs or fork revisions.
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

	// The root and agent-image channel managers keep tracking their own locks.
	test("the two channel scopes retain their own revs", () => {
		const agent = JSON.parse(readFileSync(join(repoRoot, LOCK), "utf8"));
		const root = JSON.parse(
			readFileSync(join(repoRoot, "devenv.lock"), "utf8"),
		);
		// Both are real 40-hex channel revs…
		expect(agent.nodes.nixpkgs.locked.rev).toMatch(/^[a-f0-9]{40}$/);
		expect(root.nodes[root.nodes.root.inputs.nixpkgs].locked.rev).toMatch(
			/^[a-f0-9]{40}$/,
		);
		// The config does not reconcile these independent channel pins, whether or
		// not they happen to differ today.
		const channelManagers = (cfg.customManagers ?? []).filter((m) =>
			m.matchStrings?.some((s) => s.includes("devenv-nixpkgs")),
		);
		expect(channelManagers).toHaveLength(2);
		expect(
			new Set(channelManagers.map((m) => m.managerFilePatterns?.[0])).size,
		).toBe(2);
	});

	// Solo branch and cooldown-exempt, separate from both channel and fork groups.
	test("the rule is solo-grouped and cooldown-exempt", () => {
		expect(rule).toBeDefined();
		expect(rule?.matchManagers).toContain("custom.regex");
		expect(rule?.groupName).toBe(GROUP);
		expect(cfg.packageRules.filter((r) => r.groupName === GROUP)).toHaveLength(
			1,
		);
		// A git-refs digest on a moving branch HEAD carries no release age, so the
		// repo-wide strict cooldown would peg it permanently `pending` and cut
		// zero PRs (the RIG-1220 silent-no-updates shape).
		expect(rule?.minimumReleaseAge).toBeNull();
	});

	// Include both the relocked file and the FOD hash in Renovate's allowlist.
	test("the postUpgradeTask is branch-mode over the lock AND the FOD file", () => {
		const task = rule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.commands).toEqual([REFRESH]);
		expect(task?.fileFilters).toEqual([LOCK, "agent-image/entrypoint.nix"]);
	});

	// Use this scope's refresher; the root script would relock unrelated files.
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

	// Keep the digest in a solo branch so its relock task has the branch slot.
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

	// Its group differs from both channel groups and the shared fork group.
	test("its group differs from both channel groups and the devenv fork group", () => {
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

	// Relock before refreshing the FOD pin; only commit the lock and updated pin.
	// Keep this task separate from the channel and fork groups.
	test("its group differs from the root channel and devenv fork groups", () => {
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

	// Keep Go on a solo branch so it cannot compete for the top-level task slot.
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
	// Keep wails/v3 below 3.1 until GTK4 is proven; v3.1 removes the GTK3 tag.
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
		// Admit the live pin and v3.0.x while rejecting v3.1 prereleases and v4+.
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
	// Track the postgres image digest while keeping the allowed major at 18.
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

	// Ensure the unscoped CI postgres disable does not suppress this dependency.
	const resolveEnabled = (dep: SyntheticDep): boolean => {
		let enabled = true;
		for (const rule of cfg.packageRules) {
			if (!ruleMatches(rule, dep)) continue;
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
	// The catalog soak-exempt rule must name exactly the bunfig excludes that are
	// catalog deps; Solid prerelease pins use a separate apps/ui-scoped rule.
	const firstPartyToolNames = [
		"@rigelbuild/ref-gate",
		"@rigelbuild/biome-config",
		"@rigelbuild/tsconfig",
		"@rigelbuild/comment-scan",
		"@rigelbuild/length-gate",
	];
	// Runs with the Renovate App token in renovate.yml, so it must stay soaked.
	const tokenBearingPackage = "@rigelbuild/renovate-preflight";
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
		return bunfigExcludes().filter((name) => catalog.has(name));
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
		expect(expected.slice().sort()).toEqual(
			["@types/bun", "bun-types", ...firstPartyToolNames].sort(),
		);
		expect(soakRule?.matchPackageNames?.slice().sort()).toEqual(
			expected.slice().sort(),
		);
	});
	test("first-party package pins match Bun excludes and skip Renovate soak", () => {
		const expected = firstPartyToolNames.slice().sort();
		expect(
			bunfigExcludes()
				.filter((name) => expected.includes(name))
				.sort(),
		).toEqual(expected);
		expect(
			soakRule?.matchPackageNames
				?.filter((name) => firstPartyToolNames.includes(name))
				.sort(),
		).toEqual(expected);
	});
	test("the token-bearing preflight package keeps the release soak", () => {
		expect(bunfigExcludes()).not.toContain(tokenBearingPackage);
		// Glob-aware on both axes so a wildcard rule such as "@rigelbuild/*" also trips this.
		const glob = (value: string) => (pattern: string) =>
			new Bun.Glob(pattern).match(value);
		for (const rule of cfg.packageRules) {
			if (rule.minimumReleaseAge !== null) continue;
			if (
				rule.matchFileNames &&
				!rule.matchFileNames.some(glob("package.json"))
			)
				continue;
			const matches = glob(tokenBearingPackage);
			expect((rule.matchPackageNames ?? []).filter(matches)).toEqual([]);
		}
	});
	test("RigelBuild git-ref packages bypass the release-age cooldown", () => {
		const managers =
			cfg.customManagers?.filter(
				(manager) =>
					manager.datasourceTemplate === "git-refs" &&
					manager.depNameTemplate?.startsWith("RigelBuild/") === true,
			) ?? [];
		expect(managers.length).toBeGreaterThan(0);
		for (const manager of managers) {
			const rule = cfg.packageRules.find((candidate) =>
				candidate.matchDepNames?.includes(manager.depNameTemplate ?? ""),
			);
			expect(rule?.minimumReleaseAge).toBeNull();
		}
	});

	test("Solid fork pins skip cooldown only in the UI manifest", () => {
		const rule = cfg.packageRules.find(
			(candidate) =>
				candidate.matchPackageNames?.includes("@rigelbuild/solid-*") === true,
		);
		expect(rule?.minimumReleaseAge).toBeNull();
		expect(rule?.matchFileNames).toEqual(["apps/ui/package.json"]);
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
	test("the Reviewable body-cut pin matches the core's verified Renovate version", () => {
		const match = /bunx renovate@(?<version>\S+)/.exec(runLines);
		expect(match?.groups?.version).toBe(REVIEWABLE_CUT_VERIFIED_AT);
	});

	// The preflight uses GH_TOKEN to probe the repo and RENOVATE_TOKEN for its
	// token-presence guard. Both env vars must be set or the workflow fails before
	// Renovate starts. Guard the env so the probe cannot silently regress.
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

	test("the preflight runs the installed package after a frozen install and before Renovate", () => {
		const install = workflow.indexOf("bun install --frozen-lockfile");
		const preflight = workflow.indexOf(
			"bun node_modules/@rigelbuild/renovate-preflight/index.ts",
		);
		const renovate = workflow.search(/^\s*-\s+name:\s*Run Renovate/m);
		expect(install).toBeGreaterThanOrEqual(0);
		expect(preflight).toBeGreaterThan(install);
		expect(renovate).toBeGreaterThan(preflight);
	});
});

describe("tools/renovate FOD trigger coverage (every task site, derived from FOD_ENTRIES)", () => {
	// Trigger-writing tasks must refresh and commit the pin; derive coverage from
	// FOD_ENTRIES. Manager-written triggers need a separate top-level guard.

	// Match Renovate fileFilters as globs so equivalent patterns count.
	const covers = (filters: string[], path: string): boolean =>
		filters.some((filter) => new Bun.Glob(filter).match(path));

	// Every declared task site, enumerated exactly as allDeclaredCommands does:
	// the top-level task plus every packageRule-level one. Labelled so a failure
	// names the offending site rather than an index alone.
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
		// Pin the coupled set so a renamed trigger or filter cannot empty the guard.
		expect(coupled.length).toBe(20);
		expect(taskSites.length).toBeGreaterThan(0);
	});

	// Violation messages, built outside the scan loop: each states the failure
	// mechanism a maintainer needs, and keeping them here leaves the loop below
	// as the three predicates it checks.
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

	// In-process refreshers need no duplicate command; list only actual callers.
	const IN_PROCESS_REFRESHERS: Record<string, string> = {
		// refresh-agent-image-nixpkgs.ts: relocks the agent-image channel, then
		// awaits refreshFodEntries(agentImageFodEntries()).
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

describe("tools/renovate guest-rootfs agent-image pin lockstep", () => {
	// Renovate cannot derive layer digests, so image digest bumps need a relock.
	const DEP = "compass-agent-guest";
	const RELOCK = "bun tools/guest-image/pin-agent-image.ts --relock";
	const LOCK = "guest-image/agent-oci.lock";

	const manager = cfg.customManagers?.find((m) => m.depNameTemplate === DEP);
	const rule = cfg.packageRules.find((r) => r.matchDepNames?.includes(DEP));

	test("a custom.regex manager surfaces the pin from the lock file", () => {
		expect(manager).toBeDefined();
		expect(manager?.customType).toBe("regex");
		expect(manager?.packageNameTemplate).toBe(
			"ghcr.io/rigelbuild/compass-agent",
		);
		expect(
			manager?.managerFilePatterns?.some((p) => p.includes("agent-oci")),
		).toBe(true);
	});

	test("detection rides the moving tag, because the pinned tag cannot be ordered", () => {
		// The lock pins an immutable per-commit `git-<sha12>`; no datasource can
		// order it, so the dep's currentValue is the moving `latest` and only the
		// digest is matched.
		expect(manager?.currentValueTemplate).toBe("latest");
		expect(manager?.matchStrings?.join("")).toContain("currentDigest");
	});

	test("versioning is explicit docker, not the regex manager's semver default", () => {
		// A custom.regex manager defaults to `semver-coerced` regardless of
		// datasource, which mishandles a digest reference.
		expect(manager?.datasourceTemplate).toBe("docker");
		expect(manager?.versioningTemplate).toBe("docker");
	});

	test("the pin's rule runs the relock as a branch-mode task", () => {
		expect(rule).toBeDefined();
		expect(rule?.postUpgradeTasks?.commands).toContain(RELOCK);
		// Branch mode is what gives the task the branch's single task slot.
		expect(rule?.postUpgradeTasks?.executionMode).toBe("branch");
	});

	test("the rule is solo-grouped, so it owns that single task slot", () => {
		// Sharing a branch with the rollup would contend for the one branch-mode
		// slot and silently drop the relock.
		expect(rule?.groupName).toBeDefined();
		const sharing = cfg.packageRules.filter(
			(r) => r.groupName === rule?.groupName,
		);
		expect(sharing).toHaveLength(1);
	});

	test("fileFilters permits the lock, so the relock's write is committed", () => {
		// fileFilters is an INCLUDE allowlist: omit the lock and Renovate computes
		// the relock and then drops it.
		expect(rule?.postUpgradeTasks?.fileFilters).toContain(LOCK);
	});

	test("the relock command is permitted by an anchored allowlist entry", () => {
		const allowed = bot.allowedCommands ?? [];
		expect(allowed.some((a) => new RegExp(a).test(RELOCK))).toBe(true);
		// …and the anchoring refuses an appended-metacharacter variant.
		expect(allowed.some((a) => new RegExp(a).test(`${RELOCK}; id`))).toBe(
			false,
		);
	});

	test("the cooldown is nulled, so an unknown-age digest is not held forever", () => {
		// Reproducible zero timestamps make strict minimumReleaseAge hold this digest forever.
		expect(rule?.minimumReleaseAge).toBeNull();
	});

	test("the pin's dep resolves ENABLED, not swallowed by an unscoped rule", () => {
		// Fails closed if any future rule disables this depName the way the
		// `postgres` CI-service fence disables its namesake.
		expect(
			resolveGroupName({
				manager: "custom.regex",
				depName: "compass-agent-guest",
				packageName: "ghcr.io/rigelbuild/compass-agent",
				fileName: LOCK,
				updateType: "digest",
			}),
		).toBe("compass-agent image (guest rootfs)");
	});
});

describe("tools/renovate guest-agent index digest lockstep", () => {
	test("the committed guest pin regex matches exactly its index digest", () => {
		const lock = JSON.parse(
			readFileSync(join(repoRoot, "guest-image/agent-oci.lock"), "utf8"),
		) as { digest: string };
		const manager = cfg.customManagers?.find(
			(entry) => entry.depNameTemplate === "compass-agent-guest",
		);
		const expression = manager?.matchStrings?.join("");
		expect(expression).toBeDefined();
		const matches = [
			...readFileSync(
				join(repoRoot, "guest-image/agent-oci.lock"),
				"utf8",
			).matchAll(new RegExp(expression ?? "", "g")),
		];
		expect(matches).toHaveLength(1);
		expect(matches[0]?.[1]).toBe(lock.digest);
	});
});

describe("tools/renovate fork prerelease rules", () => {
	const dep = (name: string): SyntheticDep => ({
		manager: "bun",
		depName: name,
		packageName: name,
		fileName: "apps/ui/package.json",
		depType: "dependencies",
		updateType: "patch",
	});

	test("solid-markdown follows its -rc.N prereleases", () => {
		const md = dep("@rigelbuild/solid-markdown");
		expect(resolveRuleValue(md, "ignoreUnstable")).toBe(false);
		expect(resolveRuleValue(md, "respectLatest")).toBeUndefined();
	});

	test("solid-virtual follows -rigel.N builds past the latest dist-tag", () => {
		const virt = dep("@rigelbuild/solid-virtual");
		expect(resolveRuleValue(virt, "ignoreUnstable")).toBe(false);
		expect(resolveRuleValue(virt, "respectLatest")).toBe(false);
	});

	test("other npm deps keep the stable-only default", () => {
		const other = dep("solid-js");
		expect(resolveRuleValue(other, "ignoreUnstable")).toBeUndefined();
		expect(resolveRuleValue(other, "respectLatest")).toBeUndefined();
	});
});
