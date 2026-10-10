import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import botConfig from "./bot-config.json5";
import config from "./config.json5";
// The shipped FOD table. Imported (not restated) so the coverage guard at the
// end of this file re-derives its requirement from the declaration itself.
// Side-effect-safe: that module does no I/O at import (its main() is behind an
// `import.meta.main` guard); the only load-time work is a fragment-disjointness
// assertion over this same table.
import { FOD_ENTRIES } from "./refresh-fod-hashes.ts";

// Guard suite for compass's self-hosted Renovate config (RIG-2432). Covers compass's
// config + ecosystems (bun catalog, devenv-nixpkgs channel, toolchain pins, gomod,
// GitHub Actions; no rust/pulumi/woodpecker). The .json5 configs load via Bun's loader.

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
};

const cfg = config as RenovateConfig;
const bot = botConfig as {
	allowedCommands?: string[];
	customEnvVariables?: Record<string, string>;
	executionTimeout?: number;
};
// tools/renovate/ → repo root is two levels up.
const repoRoot = join(import.meta.dir, "..", "..");

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
	// postUpgradeTasks.commands are gated by the BOT config's global
	// `allowedCommands` allowlist (a repo config cannot self-authorize a command),
	// which Renovate matches UNANCHORED via regEx(pattern).test(cmd). So each
	// entry's `^…$` IS the security property. Compass declares eleven DISTINCT
	// commands across the task sites (the FOD-hash refresh rides SIX sites — see
	// the per-site enumeration on the count test below — so it appears six times
	// in the declared list but needs only one allowlist entry; the devenv-fork
	// relock rides the one devenv-fork rule, which relocks every changed lock); every
	// distinct command must be permitted, every entry must be used, and no entry may
	// be an unanchored substring rule. RIG-3100 added the fifth: the go↔go-overlay
	// lockstep on the go pin's solo branch. RIG-2815 added the sixth: the
	// devenv-fork relock on the one rule covering both locks. The seventh is the
	// agent-image devenv-nixpkgs CHANNEL relock — the fourth devenv pin, on its
	// own solo branch, with its own script because the root channel script's
	// biome/catalog/bun.lock/flake tail has no counterpart in that scope. The
	// eighth is the guest-rootfs agent-image relock, which rewrites the pinned
	// tag, digest, and per-layer fetch keys together. The ninth refreshes the
	// coupled source and vendor hashes for Go analysis pins. The tenth and
	// eleventh are the Meissa relock (`devenv update meissa`) and biome catalog writer.
	const commands = allDeclaredCommands();
	const distinctCommands = [...new Set(commands)];
	const allowed = bot.allowedCommands ?? [];

	test("declares eleven DISTINCT postUpgrade commands and eleven allowlist entries", () => {
		expect(distinctCommands).toHaveLength(11);
		expect(allowed).toHaveLength(11);
	});

	test("the fod-hash refresh is declared at all six task sites", () => {
		// The command must ride every task shape that can own a bump able to move a
		// pinned FOD, because a rule-level task REPLACES the top-level one on its
		// branch. The six sites, all carrying the refresh:
		//   1. top-level (branch mode)      — gomod + bun/TypeScript-first branches
		//   2. devenv-nixpkgs channel rule  — relocks devenv.lock, a declared trigger
		//   3. devenv fork rule            — relocks both locks, each a declared trigger
		//   4. go ↔ go-overlay lockstep     — relocks devenv.lock likewise
		//   5. workspaces.catalog rule     — update mode, eviction-proof
		//   6. Meissa lockstep             — relocks devenv.lock and may move the
		//                                    biome pin + bun.lock
		// Sites 3 and 4 carry it fail-safe: each relocks one non-nixpkgs input, so
		// neither moves the FOD's bun — but each writes a declared trigger of the
		// entrypoint.nix entry, so the coupling holds at file granularity and the
		// refresh's write is a no-op when nothing moved (the gate itself fires on
		// those branches — the relocked devenv.lock IS the trigger — so the price
		// is one extra realise, not nothing). The generalized guard at the end of
		// this file is what keeps that coverage property true for future sites: it
		// derives from FOD_ENTRIES that any site declaring a trigger must run the
		// refresh LAST and name the pin's file.
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

	test("permits exactly the eleven declared commands", () => {
		expect(distinctCommands.sort()).toEqual(
			[
				"bun install --lockfile-only",
				"bun tools/guest-image/pin-agent-image.ts --relock",
				"bun tools/renovate/refresh-agent-image-nixpkgs.ts",
				"bun tools/renovate/refresh-devenv-lock.ts",
				"bun tools/renovate/refresh-devenv-nixpkgs.ts",
				"bun tools/renovate/refresh-fod-hashes.ts",
				"bun tools/renovate/refresh-go-analysis-hashes.ts",
				"bun tools/renovate/refresh-go-overlay.ts",
				"bun tools/renovate/refresh-toolchain-hashes.ts",
				"bun tools/renovate/refresh-biome-catalog.ts",
				"devenv update meissa",
			].sort(),
		);
	});
});

describe("tools/renovate FOD-hash refresh wiring (PR #579)", () => {
	// A dep bump moves a pinned Nix fixed-output-derivation hash; left stale the
	// image build fails `hash mismatch in fixed-output derivation`. refresh-fod-
	// hashes.ts recomputes it, but Renovate only COMMITS files a task's fileFilters
	// name — so a task that rewrites a FOD file without listing it silently drops
	// the fix and the bump PR still goes red. This describe pins the two
	// eviction-critical sites named below site-by-site; the generalized guard at
	// the end of this file covers the property across EVERY task site, present and
	// future, deriving what each must declare from FOD_ENTRIES itself.
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
		// gomod branches + bun/npm-first branches inherit this slot; it must be able
		// to commit the Go vendorHash file, its flake.nix mirror (identical hash,
		// same buildGoModule proxyVendor set over go/ — refresh-fod-hashes.ts mirrors
		// the recomputed value into it), and the bun outputHash file. Renovate only
		// commits files a task's fileFilters names, so a missing flake.nix here would
		// silently drop the mirror edit → a gomod bump lands with flake.nix's
		// vendorHash stale and `nix flake check` red (RIG-2852 Gap 1).
		// A bun-first branch also moves the UI node_modules pin.
		expect(topLevel?.fileFilters).toContain("apps/ui/dist.nix");
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

	// INVARIANT: a vuln fix is injected as a packageRule carrying
	// `force: { ...vulnerabilityAlerts }`, and applyPackageRules clears a prior
	// skipReason when force.enabled is truthy — which would CANCEL every
	// `enabled: false` rule (the postgres CI-service pin, the gomod `go`
	// directive, biome) and re-open the bumps they exist to hold shut. The
	// default vulnerabilityAlerts object has no `enabled` key, so those disables
	// hold; assert it is absent (never true).
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
	// Both compass devenv scopes resolve github:RigelBuild/devenv by default
	// branch. Separate regex managers surface each lock's fork rev, while one
	// package rule groups both updates and relocks every changed scope.
	//
	// Found by the dep each stamps, never by index or a bare "devenv" file-pattern
	// substring (the devenv-nixpkgs channel manager pattern-matches the same root
	// lock).
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
	const ruleFor = (depName: string) =>
		cfg.packageRules.find((r) => r.matchDepNames?.includes(depName));

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
			// The locks name no ref, so the tracked value is the default branch.
			expect(manager?.currentValueTemplate).toBe("main");

			// The file pattern must be ANCHORED to exactly this lock: a loose
			// pattern would make the root manager extract from the agent-image lock
			// too (or vice versa), so each manager remains scoped to one lock.
			//
			// Pin the LITERAL first, then re-parse it behaviourally below. The
			// literal assertion is what makes a Renovate delimiter-semantics drift
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
	// The fourth devenv pin. The agent-image channel digest had no manager, so it
	// only advanced when someone relocked by hand. Its manager and rule now give
	// it a solo-branched, self-relocking shape independent of the fork update.
	//
	// Find managers by stamped depName: both the channel and fork managers match
	// agent-image/devenv.lock, so a file-pattern finder would be order-dependent.
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

	// The matchString must recover EXACTLY ONE 40-hex rev from the REAL
	// agent-image lock, and it must be the OUTER channel rev
	// (nodes.nixpkgs.locked.rev) — not the inner nixpkgs-src rev the relock
	// refreshes from it, not the devenv FORK rev the sibling manager owns, and
	// not the `original` block (which repeats `"repo": "devenv-nixpkgs"` but is
	// followed by `"ref"`, never `"rev"` — the whole anchor-uniqueness argument).
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

	// Branch-mode task over EXACTLY the two files the script writes. fileFilters
	// is an INCLUDE allowlist — Renovate commits ONLY listed files — so dropping
	// entrypoint.nix would run the FOD refresh and silently discard it, shipping
	// a channel bump whose outputHash never moved (`hash mismatch in
	// fixed-output derivation` on the image build), and dropping the lock would
	// discard the relock itself.
	test("the postUpgradeTask is branch-mode over the lock AND the FOD file", () => {
		const task = rule?.postUpgradeTasks;
		expect(task?.executionMode).toBe("branch");
		expect(task?.commands).toEqual([REFRESH]);
		expect(task?.fileFilters).toEqual([LOCK, "agent-image/entrypoint.nix"]);
	});

	// A SEPARATE script from the root channel lockstep, deliberately: the root
	// script's tail (biome eval, catalog pin, bun.lock, flake lockstep) has no
	// counterpart in this scope. Assert the two rules do NOT share a command, so
	// a future "simplification" that points this rule at the root script — which
	// would relock the ROOT lock on an agent-image branch — fails here.
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

	// The two locks must never land in ONE branch, and this digest must never
	// fold into the TypeScript rollup (which also matches custom.regex) — either
	// would put two branch-mode tasks on one branch, where Renovate builds only
	// one and the other pin ships bumped-but-unrelocked. Replay the real
	// last-match-wins packageRule semantics.
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

	// Branch-mode task over exactly the two files it writes: devenv.lock (the
	// `devenv update go-overlay` re-lock) and the bun outputHash pin that re-lock
	// invalidates by declaration — devenv.lock is a trigger of the entrypoint.nix
	// FOD entry, so the refresh rides here and its recomputed pin needs a filter
	// slot to be committed. It must NOT rewrite go.nix (the go manager already
	// did) nor any other hash pin, so listing anything else — guest-image/
	// default.nix or flake.nix, whose entry triggers on go/go.mod|go.sum — would be
	// dead filter surface; listing LESS would silent-drop the re-lock (shipping a
	// go bump the overlay can't resolve → the exact CI red this task exists to
	// prevent) or the refreshed pin. fileFilters is an INCLUDE allowlist, so this
	// pins it. The command order is load-bearing: relock FIRST, so the pin is
	// realised against the written lock. The failure mode under a REVERSED order
	// differs here from the devenv-nixpkgs and devenv-fork sites: the go manager's
	// file scope is /^tools/toolchain/versions/go\.nix$/ (config.json5:319),
	// which never touches devenv.lock. If refresh ran first, its self-gate would
	// no-op; only then would `devenv update go-overlay` rewrite the lock. The
	// bump would ship with a stale pin. Hence the literal command-order pin below.
	// …and it is a DIFFERENT branch from the root channel pin and the shared
	// devenv fork group.
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

describe("tools/renovate go-analysis pins (RIG-3306)", () => {
	const pinText = readFileSync(
		join(repoRoot, "tools/toolchain/versions/go-analysis.nix"),
		"utf8",
	);
	const golangci = cfg.customManagers?.find(
		(m) => m.depNameTemplate === "golangci/golangci-lint",
	);
	const nilaway = cfg.customManagers?.find(
		(m) => m.depNameTemplate === "uber-go/nilaway",
	);
	const goAnalysisFile = "tools/toolchain/versions/go-analysis.nix";

	test("both managers extract exactly their real pin", () => {
		expect(golangci).toBeDefined();
		expect(nilaway).toBeDefined();
		const golangciMatches = [
			...pinText.matchAll(new RegExp(golangci?.matchStrings?.[0] ?? "", "g")),
		];
		expect(golangciMatches).toHaveLength(1);
		expect(golangciMatches[0]?.groups?.currentValue).toBe("2.14.0");
		const nilawayMatches = [
			...pinText.matchAll(new RegExp(nilaway?.matchStrings?.[0] ?? "", "g")),
		];
		expect(nilawayMatches).toHaveLength(1);
		const nilawayBlock = pinText.match(
			/^ {2}nilaway = \{[\s\S]*?^ {2}\};/m,
		)?.[0];
		expect(nilawayBlock).toBeDefined();
		expect(nilawayMatches[0]?.groups?.currentDigest).toBe(
			nilawayBlock?.match(/^\s*rev = "([a-f0-9]{40})"/m)?.[1],
		);
	});

	test("manager metadata preserves the pin-specific update sources", () => {
		expect(golangci).toMatchObject({
			customType: "regex",
			managerFilePatterns: ["/^tools/toolchain/versions/go-analysis\\.nix$/"],
			depNameTemplate: "golangci/golangci-lint",
			datasourceTemplate: "github-releases",
			extractVersionTemplate: "^v(?<version>.+)$",
			depTypeTemplate: "go-analysis",
		});
		expect(golangci?.matchStrings?.[0]).toBe(
			'golangci-lint = \\{\\s*version = "(?<currentValue>[^"]+)"',
		);
		expect(nilaway).toMatchObject({
			customType: "regex",
			managerFilePatterns: ["/^tools/toolchain/versions/go-analysis\\.nix$/"],
			depNameTemplate: "uber-go/nilaway",
			packageNameTemplate: "https://github.com/uber-go/nilaway",
			currentValueTemplate: "main",
			datasourceTemplate: "git-refs",
			depTypeTemplate: "go-analysis",
		});
		expect(nilaway?.matchStrings?.[0]).toBe(
			'rev = "(?<currentDigest>[a-f0-9]{40})"',
		);
	});

	test("both pins resolve to solo branches outside the TypeScript rollup", () => {
		for (const dep of [
			{
				manager: "custom.regex",
				fileName: goAnalysisFile,
				depName: "golangci/golangci-lint",
				depType: "go-analysis",
				updateType: "minor",
			},
			{
				manager: "custom.regex",
				fileName: goAnalysisFile,
				depName: "uber-go/nilaway",
				depType: "go-analysis",
				updateType: "digest",
			},
		]) {
			expect(resolveGroupName(dep)).toBeNull();
			expect(resolveGroupName(dep)).not.toBe("TypeScript dependencies");
		}
	});

	test("cooldown resolves only for the git-refs digest exception", () => {
		const golangciDep = {
			manager: "custom.regex",
			fileName: goAnalysisFile,
			depName: "golangci/golangci-lint",
			depType: "go-analysis",
			updateType: "minor",
		};
		const nilawayDep = {
			manager: "custom.regex",
			fileName: goAnalysisFile,
			depName: "uber-go/nilaway",
			depType: "go-analysis",
			updateType: "digest",
		};
		expect(cfg.minimumReleaseAge).toBe("5 days");
		expect(resolveRuleValue(golangciDep, "minimumReleaseAge")).toBeUndefined();
		expect(resolveRuleValue(nilawayDep, "minimumReleaseAge")).toBeNull();
	});

	test("no matching rule-level task can evict the top-level refresher", () => {
		for (const [depName, updateTypes] of [
			["golangci/golangci-lint", ["patch", "minor", "major"]],
			["uber-go/nilaway", ["digest"]],
		] as const) {
			for (const updateType of updateTypes) {
				const dep = {
					manager: "custom.regex",
					fileName: goAnalysisFile,
					depName,
					depType: "go-analysis",
					updateType,
				};
				for (const rule of cfg.packageRules.filter((r) => r.postUpgradeTasks)) {
					expect(ruleMatches(rule, dep)).toBe(false);
				}
			}
		}
	});

	test("the top-level task commits the pin file and has the bot allowlist entry", () => {
		const command = "bun tools/renovate/refresh-go-analysis-hashes.ts";
		expect(cfg.postUpgradeTasks?.commands).toContain(command);
		expect(cfg.postUpgradeTasks?.fileFilters).toContain(goAnalysisFile);
		expect(
			bot.allowedCommands?.some((entry) => new RegExp(entry).test(command)),
		).toBe(true);
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
	// Renovate's last-match-wins semantics (shared ruleMatches gates) for a
	// synthetic postgres-stack docker dep and confirm it resolves
	// ENABLED — this fails closed if the fence (or any future unscoped rule) ever
	// swallows postgres-stack, silently defeating the automation.
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
	// The catalog-scoped soak-exemption packageRule governs ONLY catalog deps
	// (matchManagers custom.regex + matchDepTypes workspaces.catalog), so its
	// matchPackageNames must equal exactly the bunfig `minimumReleaseAgeExcludes`
	// entries that ARE catalog deps — i.e. the bun-types pair (@types/bun is a
	// catalog pin; bun-types is its transitive lockstep). Every other bunfig
	// exclude is a literal npm pin or an `overrides` pin (@tanstack/virtual-core,
	// the Solid v2 / @tanstack query RC track, the two @rigelbuild/solid-* pins)
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

describe("tools/renovate FOD trigger coverage (every task site, derived from FOD_ENTRIES)", () => {
	// THE CLASS THIS GUARDS. A pinned Nix fixed-output-derivation hash content-
	// addresses a fetched dependency set, so it is invalidated by a change to any
	// of the manifests refresh-fod-hashes.ts declares as that entry's `triggers`.
	// A postUpgradeTask that names such a trigger in its fileFilters is declaring
	// "this task may commit a change to that manifest" — and Renovate commits ONLY
	// files a task's fileFilters names, so such a task ships the lock change while
	// the pin beside it still addresses the OLD closure. The image build then fails
	// `hash mismatch in fixed-output derivation` and the bump PR goes red — the
	// failure that kept PR #580 red for weeks.
	//
	// So the requirement is structural, not per-site: any site that can write a
	// declared trigger must ALSO run the refresh (to recompute the pin) and name
	// that entry's FOD file plus every mirrorFile (or Renovate drops the recomputed
	// pin on the floor — the RIG-2852 Gap 1 silent-drop shape). The refresh
	// self-gates per entry on TRIGGER CHANGE, and devenv.lock is exactly what
	// these tasks rewrite — so on those branches the gate FIRES rather than
	// passing over: firing means writing a deliberately-fake SRI and running a
	// full `nix build` of the FOD's vehicle, a guaranteed fixed-output cache
	// miss that forces a networked `bun install`. What is a no-op is the
	// resulting WRITE (the same SRI), not the work. So covering a site whose
	// installed tree did not actually move costs one extra realise per
	// such branch (refresh-fod-hashes.ts:54: "costs at most one extra realise"),
	// and no site's coverage gap is worth that price.
	//
	// The trigger sets are READ from FOD_ENTRIES rather than restated here, so
	// adding a trigger or a new pinned FOD re-derives the requirement over every
	// site automatically instead of needing a matching test edit.
	//
	// DETECTION BOUNDARY. What this reads is a site's fileFilters — i.e. the files
	// the TASK declares it may commit. That misses the case where the trigger is
	// written by Renovate's own manager update rather than by the task, because a
	// manager's writes are never declared in fileFilters. The live instance is the
	// guest-image/default.nix vendorHash: its triggers are go/go.mod and go/go.sum,
	// which the gomod manager writes, so NO site names them and the pair below is
	// empty for that entry — yet the pin still moves and the top-level task still
	// has to refresh it. That leg is covered by assertion instead of derivation
	// (the top-level site is pinned to carry the refresh, and gomod branches fall
	// to the top-level slot because no rule-level task matches the gomod manager).
	// A future rule-level task that matched gomod deps would evict that slot and
	// this guard would NOT flag it; such a rule must carry the refresh, the FOD
	// file, and its mirrorFiles by hand.

	// fileFilters entries are GLOB patterns (Renovate matches them as globs, e.g.
	// "**/*.js"), so coupling must be decided by glob match, not string equality —
	// otherwise a site that names a trigger under any non-literal spelling is
	// invisible here and the exact failure class this guard exists to catch ships
	// silently. Every entry is a literal path today, for which a glob match and an
	// equality test coincide.
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
		// The guard below iterates `coupled`, so a set that emptied or thinned out
		// would leave it passing while checking nothing. A bare `> 0` cannot see
		// the thinning: renaming a single trigger dissolves several pairings while
		// leaving the count positive.
		// Pinning the exact count catches a partial trigger rename, or a
		// fileFilters edit, that dissolves any single pairing. It does NOT check
		// WHICH sites are coupled — the per-site describes above pin that — only
		// that the population has not shrunk or grown. A newly coupled site is a
		// deliberate edit: update this number in the same change.
		//
		// 13 = today's baseline 14, less the one site-entry pair the merged rule removes.
		expect(coupled.length).toBe(13);
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

	// A command that refreshes the FOD pin IN-PROCESS rather than by shelling
	// FOD_COMMAND. The guard's job is to ensure a site committing a trigger
	// recomputes the pin; a script that imports refreshFodEntries and drives the
	// same table satisfies that as completely as the standalone command, and
	// adding the command beside it would pay a SECOND faked-pin realise (a full
	// FOD cache miss plus a networked bun install) to rewrite a value already
	// correct. Each entry is listed with the call site that makes it true, so this
	// stays a per-script statement of fact and never a blanket exemption: a script
	// added here without that call fails the assertion below.
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
	// guest-image/agent-oci.lock pins the published compass-agent image the
	// microVM guest rootfs is built FROM. The lock carries the per-layer
	// descriptor digests the rootfs's fixed-output fetches key on, which Renovate
	// cannot compute — so the manager MUST be paired with a relock task, exactly
	// like the devenv-lock pair and unlike the digest-only postgres pin. These
	// assertions are what keep that pairing from being trimmed back to a bare
	// regex bump, which would redden every fetch on every Renovate PR.
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
		// nix2container zeroes timestamps for reproducibility, so this image
		// reports Created: 0001-01-01T00:00:00Z and the repo-wide
		// minimumReleaseAge + internalChecksFilter:"strict" would keep the digest
		// permanently `pending` — zero PRs, pin stale forever. Unlike the
		// postgres pin, which keeps the soak because its registry supplies a
		// real timestamp.
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
