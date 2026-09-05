import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import botConfig from "./bot-config.json5";
import config from "./config.json5";

// Guard suite for compass's self-hosted Renovate config (RIG-2432). Ported from
// the internal monorepo's ci/renovate config.test.ts and adapted to compass's config +
// ecosystems (bun catalog, devenv-nixpkgs channel, toolchain pins, gomod, GitHub
// Actions; NO rust/cargo, pulumi, woodpecker, nix-manager, or markdownlint
// catalog — dropped, compass has none). The .json5 configs load via Bun's native
// JSON5 import loader, exactly as the internal monorepo's suite loads them.

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
	// entry's `^…$` IS the security property. Compass declares six DISTINCT
	// commands across the task sites (the FOD-hash refresh rides three sites — the
	// top-level branch-mode task, the catalog rule's update-mode task, and the
	// devenv-nixpkgs branch-mode lockstep task — so it appears three times in the
	// declared list but needs only one allowlist entry; the devenv-fork relock
	// likewise rides BOTH devenv-fork rules under one command string, since the
	// script self-gates on which lock changed); every
	// distinct command must be permitted, every entry must be used, and no entry may
	// be an unanchored substring rule. RIG-3100 added the fifth: the go↔go-overlay
	// lockstep on the go pin's solo branch. RIG-2815 added the sixth: the
	// devenv-fork relock on each devenv lock's solo branch.
	const commands = allDeclaredCommands();
	const distinctCommands = [...new Set(commands)];
	const allowed = bot.allowedCommands ?? [];

	test("declares six DISTINCT postUpgrade commands and six allowlist entries", () => {
		expect(distinctCommands).toHaveLength(6);
		expect(allowed).toHaveLength(6);
	});

	test("the fod-hash refresh is declared at all three task sites", () => {
		// The command must ride every task shape that can own a dependency bump:
		// the top-level branch-mode slot, the catalog rule's update-mode pass, and
		// the devenv-nixpkgs branch-mode lockstep task. Catalog-first branches evict
		// the top-level slot, while channel branches use the devenv-nixpkgs slot; the
		// raw command list therefore carries the FOD refresh once at each site.
		const fod = "bun tools/renovate/refresh-fod-hashes.ts";
		expect(commands.filter((c) => c === fod)).toHaveLength(3);
		const topLevel = cfg.postUpgradeTasks?.commands ?? [];
		expect(topLevel).toContain(fod);
		const catalogRule = cfg.packageRules.find(
			(r) =>
				r.matchDepTypes?.includes("workspaces.catalog") && r.postUpgradeTasks,
		);
		expect(catalogRule?.postUpgradeTasks?.commands).toContain(fod);
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

	test("permits exactly the six RIG-2432/RIG-3100/RIG-2815 commands", () => {
		expect(distinctCommands.sort()).toEqual(
			[
				"bun install --lockfile-only",
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
	// A dep bump moves a pinned Nix fixed-output-derivation hash; left stale the
	// image build fails `hash mismatch in fixed-output derivation`. refresh-fod-
	// hashes.ts recomputes it, but Renovate only COMMITS files a task's fileFilters
	// name — so a task that rewrites a FOD file without listing it silently drops
	// the fix and the bump PR still goes red. Guard all three sites' fileFilters.
	const FOD = "bun tools/renovate/refresh-fod-hashes.ts";
	const topLevel = cfg.postUpgradeTasks;
	const catalogRule = cfg.packageRules.find(
		(r) =>
			r.matchDepTypes?.includes("workspaces.catalog") && r.postUpgradeTasks,
	);

	test("the top-level branch-mode task runs the fod refresh and is branch mode", () => {
		expect(topLevel?.commands).toContain(FOD);
		expect(topLevel?.executionMode).toBe("branch");
	});

	test("the top-level task commits ALL THREE Go/bun FOD files (fileFilters cover them)", () => {
		// gomod branches + bun/npm-first branches inherit this slot; it must be able
		// to commit the Go vendorHash file, its flake.nix mirror (identical hash,
		// same buildGoModule proxyVendor set over go/ — refresh-fod-hashes.ts mirrors
		// the recomputed value into it), and the bun outputHash file. Renovate only
		// commits files a task's fileFilters names, so a missing flake.nix here would
		// silently drop the mirror edit → a gomod bump lands with flake.nix's
		// vendorHash stale and `nix flake check` red (RIG-2852 Gap 1).
		expect(topLevel?.fileFilters).toContain("guest-image/default.nix");
		expect(topLevel?.fileFilters).toContain("flake.nix");
		expect(topLevel?.fileFilters).toContain("agent-image/entrypoint.nix");
	});

	test("the catalog rule runs the fod refresh in UPDATE mode (eviction-proof)", () => {
		// A catalog-first rollup branch evicts the top-level branch task, so the
		// refresh must also ride the catalog rule's per-upgrade update pass.
		expect(catalogRule?.postUpgradeTasks?.commands).toContain(FOD);
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
	// find it by the dep it stamps, not index — and NOT by file pattern alone:
	// the RIG-2815 devenv-FORK managers also pattern-match a devenv lock, so a
	// `includes("devenv")` finder would be ambiguous (order-dependent) between
	// three managers over the same two files.
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

	// Branch-mode lockstep task over the files the script writes:
	// devenv.lock + package.json (biome catalog pin) + bun.lock (steps 2/4/5),
	// flake.nix + flake.lock (step 6's flake-parity lockstep), and
	// agent-image/entrypoint.nix (the FOD outputHash). compass has NO committed
	// inner-rev guard file, unlike the internal monorepo's guard entry.
	//
	// The FOD refresh is required here: a channel bump moves pkgs.bun, the
	// nixpkgs-versioned builder the FOD realises, and — when the biome catalog pin
	// also moves — re-resolves the compass-agent bun.lock closure (opentelemetry
	// transitives). Either can move the outputHash; PR #580 empirically failed
	// with a compass-agent-node-modules hash mismatch. refresh-fod-hashes.ts runs
	// AFTER the relock (it reads the relock's bun.lock write from the working
	// tree) and gates on bun.lock OR devenv.lock so it fires on every channel bump
	// regardless of whether the relock ran; fileFilters must include its output or
	// Renovate silently drops the edit. The order is load-bearing and silent when
	// wrong: reversed, the FOD refresh runs before the relock writes bun.lock, the
	// gate reads clean, and it no-ops — the pinned toEqual below turns that into a
	// red test.
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
		// writes flake.nix + flake.lock, and fileFilters is an INCLUDE allowlist —
		// Renovate commits ONLY listed files. Drop either from the filter and a
		// channel bump ships with the flake skewed from devenv.lock → flake-parity
		// reds on every bump while the script's own tests stay green. These two
		// asserts turn that silent drop into a red test.
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
	// Both compass devenv scopes resolve github:RigelBuild/devenv by DEFAULT
	// BRANCH, so the concrete rev lives only in devenv.lock and nothing moved it
	// until T7. Two customManagers surface the two locks' fork revs as git-refs
	// digests, each paired with its own solo-branched packageRule carrying the
	// relock postUpgradeTask. RD-1 keeps the two locks on INDEPENDENT cadences
	// (unify the source, do NOT reconcile the locks), which is why this is two
	// managers + two rules + two groupNames and not one widened pair.
	//
	// Found by the dep each stamps, never by index or a bare "devenv" file-pattern
	// substring (the devenv-nixpkgs channel manager pattern-matches the same root
	// lock).
	const RELOCK = "bun tools/renovate/refresh-devenv-lock.ts";
	const forkScopes: {
		label: string;
		depName: string;
		lock: string;
		groupName: string;
		patternLiteral: string;
	}[] = [
		{
			label: "root",
			depName: "RigelBuild/devenv",
			lock: "devenv.lock",
			groupName: "devenv fork (root)",
			patternLiteral: "/^devenv\\.lock$/",
		},
		{
			label: "agent-image",
			depName: "RigelBuild/devenv-agent-image",
			lock: "agent-image/devenv.lock",
			groupName: "devenv fork (agent-image)",
			patternLiteral: "/^agent-image\\/devenv\\.lock$/",
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

			// The file pattern must be ANCHORED to exactly this lock: a loose
			// pattern would make the root manager extract from the agent-image lock
			// too (or vice versa), collapsing the two independent scopes into one
			// dep with two files and two conflicting digests.
			//
			// Pin the LITERAL first, then re-parse it behaviourally below. The
			// literal assertion is what makes a Renovate delimiter-semantics drift
			// (e.g. how an unescaped interior `/` is read) fail as a changed literal
			// rather than silently changing what the re-parse below is testing.
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

	// The matchString must recover EXACTLY ONE 40-hex rev from the REAL lock, and
	// it must be the fork's own `nodes.devenv.locked.rev`. The anchor's whole
	// safety argument is that `"repo": "devenv",` is followed by `"rev"` ONLY in
	// the `locked` block — the `original` block repeats the repo but is followed
	// by `"type"` (the input names no ref). Assert the uniqueness against ground
	// truth so a devenv lock-format change fails HERE rather than silently
	// binding to the wrong node (or to the `devenv-nixpkgs` node the channel
	// manager owns).
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

	// Branch-mode relock over exactly the ONE lock the rule governs. fileFilters
	// is an INCLUDE allowlist — Renovate commits ONLY listed files — so naming
	// the sibling lock would be dead surface and naming LESS would silent-drop
	// the relock, shipping a rev bump whose narHash/lastModified never moved (the
	// same silent-drop mode the FOD guard above documents).
	test.each(forkScopes)(
		"the $label relock postUpgradeTask is branch-mode over its lock alone",
		({ depName, lock }) => {
			const task = ruleFor(depName)?.postUpgradeTasks;
			expect(task?.executionMode).toBe("branch");
			expect(task?.fileFilters).toEqual([lock]);
			expect(task?.commands).toEqual([RELOCK]);
		},
	);

	// ONE command string serves both rules — the script self-gates on WHICH lock
	// changed — so a single anchored allowlist entry covers both. This is the
	// coupling the allowedCommands describe above counts; assert the two rules
	// really do share the string rather than drifting into two near-identical
	// scripts (which would silently need a second allowlist entry).
	test("both fork rules declare the SAME relock command (one allowlist entry)", () => {
		const declaring = cfg.packageRules.filter((r) =>
			r.postUpgradeTasks?.commands?.includes(RELOCK),
		);
		expect(declaring).toHaveLength(2);
		expect(
			bot.allowedCommands?.filter((a) => new RegExp(a).test(RELOCK)),
		).toEqual(["^bun tools/renovate/refresh-devenv-lock\\.ts$"]);
	});

	// The two locks must never land in ONE branch: two branch-mode relock tasks
	// on a shared branch means Renovate builds only one and the other lock ships
	// rev-bumped-but-unrelocked. Replay the real last-match-wins packageRule
	// semantics for each scope's digest and assert each resolves to its own
	// group, never the TypeScript rollup (which also matches custom.regex).
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

	// M1 (RIG-2815 review): the relock `nix run`s the fork flakeref, and the fork
	// publishes no binary cache, so every fork rev is a from-source devenv build
	// before the relock runs — measured minutes, plausibly past Renovate's 15-min
	// default executionTimeout on a cold 2-vCPU runner. A timeout kills the child,
	// the relock never runs, and Renovate still commits the regex bump (the exact
	// half-relock this task exists to prevent). Pin the raised ceiling so a
	// default change or accidental removal fails HERE, not as a nightly relock
	// silently timing out. globalOnly, so it lives in bot-config.
	test("bot-config sets an executionTimeout covering a cold fork build", () => {
		expect(typeof bot.executionTimeout).toBe("number");
		// Comfortably above the 15-min default; a cold from-source fork build can
		// exceed 15 min on a hosted runner.
		expect(bot.executionTimeout).toBeGreaterThanOrEqual(30);
	});

	// M2 (RIG-2815 review): the relock's `nix run` depends on the runner's
	// nix.conf naming the devenv + cachix substituters and their trusted keys —
	// the fork's `#devenv` closure is not on cache.nixos.org and nix ignores the
	// fork flake's own nixConfig non-interactively, so without these caches the
	// realise cold-compiles the Nix fork from source and can exhaust the runner.
	// Nothing in refresh-devenv-lock.ts provides them; renovate.yml's
	// extra_nix_config does. Assert that block still names both caches + keys so
	// trimming them there (e.g. once the PATH devenv-cli step is retired) fails a
	// test rather than wedging the nightly relock. Same fail-closed posture as the
	// self-pin workflow guard below (which already reads renovate.yml).
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
	// literal (RD-1 forbids reconciling the rules, and JSON5 has no anchor, so the
	// duplication is deliberate). Nothing else pins that they stay in sync — each
	// per-manager matchString test reads its own manager — so a one-sided edit
	// would drift silently. Assert the two share one literal, mirroring the
	// "same relock command" guard above.
	test("both fork managers declare the IDENTICAL matchString literal", () => {
		const literals = forkScopes.map(
			(s) => managerFor(s.depName)?.matchStrings?.[0],
		);
		expect(literals.every((l) => typeof l === "string")).toBe(true);
		expect(new Set(literals).size).toBe(1);
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

	// Branch-mode task over exactly devenv.lock — the sole file the refresh
	// writes (the `devenv update go-overlay` re-lock). It must NOT rewrite go.nix
	// (the go manager already did) nor any hash pin, so listing anything else
	// would be dead filter surface; listing LESS would silent-drop the re-lock,
	// shipping a go bump the overlay can't resolve → the exact CI red this task
	// exists to prevent. fileFilters is an INCLUDE allowlist, so this pins it.
	test("the lockstep postUpgradeTask is branch-mode over devenv.lock alone", () => {
		const task = goOverlayRule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.fileFilters).toEqual(["devenv.lock"]);
		expect(task?.commands).toEqual([
			"bun tools/renovate/refresh-go-overlay.ts",
		]);
	});

	// Solo-branch safety: the branch-mode task slot is winner-take-all per
	// branch, so this rule is safe ONLY because the go pin never shares a branch.
	// The versions/*.nix un-group rule nulls its groupName, so a go bump resolves
	// to its own solo branch, never the TypeScript rollup — where it would
	// collide with the top-level branch-mode task. Holds for BOTH minor and major
	// bumps: the un-group rule has no matchUpdateTypes (fires for every type),
	// and the go-overlay refresh rule likewise has none, so a major go bump also
	// solo-branches and gets the overlay refresh on its own single task slot.
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

describe("tools/renovate wails/v3 floor cap (RIG-2852, GTK4 migration)", () => {
	// The GTK4 migration record freezes a "Never v3.1" floor: wails v3.1 removes
	// the legacy GTK3 build tag, so an auto-opened v3.1 bump before the GTK4 flip
	// (RIG-2819) is proven would strand the app with no native shell. A gomod
	// packageRule caps github.com/wailsapp/wails/v3 below v3.1 via a REGEX
	// allowedVersions (not a semver range — gomod's node-semver ranges exclude a
	// prerelease at a different major.minor.patch, so `< 3.1.0-0` would wrongly
	// reject the current v3.0.0 prerelease pin). Find it by behavior, not index.
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
		// Compile the shipped regex from its /.../ delimiters and replay it. The
		// load-bearing assertion reads the ACTUAL wails require line from go/go.mod
		// and asserts the cap admits whatever is pinned — so a future pin/regex
		// pairing that would open zero PRs (the cap silently rejecting the real pin,
		// the RIG-1220 freeze shape) fails HERE, tied to ground truth rather than a
		// hard-coded literal. The boundary cases below then pin the reject edge so a
		// fat-fingered cap (e.g. `^v?3\.`) that leaked v3.1 also fails.
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
	// DefaultPostgresImage (go/internal/stack/postgres_image.go) is a standalone Go
	// const the native managers can't see; a custom.regex manager surfaces it as a
	// docker dep so upstream postgres:18 rebuilds (same major, new digest) flow
	// through a reviewable PR. DL-260 freezes the major at 18, so the paired
	// packageRule pins allowedVersions to /^18$/ — the digest moves, an 18->19
	// major never auto-opens. Find both by behavior, not index.
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

	// Load-bearing behavioral guard: the CI-service disable fence
	// (matchDepNames ["postgres"], enabled false) is unscoped by manager/file, so a
	// `postgres` depName here would inherit the disable and open ZERO PRs. Replay
	// Renovate's last-match-wins packageRule semantics (mirroring resolveGroupName's
	// gates) for a synthetic postgres-stack docker dep and confirm it resolves
	// ENABLED — this fails closed if the fence (or any future unscoped rule) ever
	// swallows postgres-stack, silently defeating the automation.
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
	// The catalog-scoped soak-exemption packageRule governs ONLY catalog deps
	// (matchManagers custom.regex + matchDepTypes workspaces.catalog), so its
	// matchPackageNames must equal exactly the bunfig `minimumReleaseAgeExcludes`
	// entries that ARE catalog deps — i.e. the bun-types pair (@types/bun is a
	// catalog pin; bun-types is its transitive lockstep). Every other bunfig
	// exclude is a literal npm pin or an `overrides` pin (@tanstack/virtual-core,
	// the Solid v2 / @tanstack query RC track, the @rigelbuild forks) — all
	// outside the catalog manager's reach, so a catalog-scoped rule cannot and
	// must not list them: a future auto-bump of those still soaks the 5 days.
	// Deriving the catalog set from the real manifest (not a hard-coded list)
	// keeps this guard current as the migration track lands and later retires.
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
	// RENOVATE_TOKEN (tools/renovate-preflight/index.ts) — so the step MUST set
	// RENOVATE_TOKEN, or index.ts short-circuits to reason="no-token" and exits 1
	// on every run, failing the job before Renovate starts. Guard the env so that
	// drop can't silently regress (it shipped green once because nothing covered
	// the preflight step's env).
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
