import { describe, expect, it } from "bun:test";
import { BUILTIN_DEFAULTS_PROVIDER_ID, type Rule } from "@oh-my-pi/pi-coding-agent/capability/rule";
import { bucketRules } from "@oh-my-pi/pi-coding-agent/capability/rule-buckets";
import { TtsrManager } from "@oh-my-pi/pi-coding-agent/export/ttsr";

function source(provider: string): Rule["_source"] {
	return { provider, providerName: provider, path: "/tmp/rule.md", level: "user" };
}

function makeRule(partial: Partial<Rule>): Rule {
	return {
		name: partial.name ?? "rule",
		path: partial.path ?? "/tmp/rule.md",
		content: partial.content ?? "body",
		globs: partial.globs,
		alwaysApply: partial.alwaysApply,
		description: partial.description,
		condition: partial.condition,
		astCondition: partial.astCondition,
		scope: partial.scope,
		interruptMode: partial.interruptMode,
		_source: partial._source ?? source("native"),
	};
}

// A TtsrManager with TTSR disabled — every addRule is rejected. Full TtsrSettings
// so it type-checks; mirrors the inline object the disabled-manager test uses.
function disabledManager(): TtsrManager {
	return new TtsrManager({
		enabled: false,
		contextMode: "discard",
		interruptMode: "always",
		repeatMode: "once",
		repeatGap: 10,
	});
}

describe("bucketRules", () => {
	it("registers a condition rule as TTSR and excludes it from rulebook/always buckets", () => {
		const mgr = new TtsrManager();
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"], description: "blocks foo" });

		const { rulebookRules, alwaysApplyRules } = bucketRules([ttsr], mgr);

		expect(rulebookRules).toHaveLength(0);
		expect(alwaysApplyRules).toHaveLength(0);
		expect(mgr.checkDelta("contains FORBIDDEN token", { source: "text" }).map(r => r.name)).toEqual(["no-foo"]);
	});

	it("registers an ast-only rule as TTSR and excludes it from rulebook/always buckets", () => {
		const mgr = new TtsrManager();
		const ttsr = makeRule({ name: "no-console", astCondition: ["console.log($A)"], description: "blocks console" });

		const { rulebookRules, alwaysApplyRules } = bucketRules([ttsr], mgr);

		expect(rulebookRules).toHaveLength(0);
		expect(alwaysApplyRules).toHaveLength(0);
		expect(mgr.hasRules()).toBe(true);
		expect(mgr.hasAstRules()).toBe(true);
	});

	it("splits non-TTSR rules into always-apply and rulebook by metadata", () => {
		const mgr = new TtsrManager();
		const sticky = makeRule({ name: "sticky", alwaysApply: true, description: "sticky desc" });
		const book = makeRule({ name: "book", description: "rulebook desc" });
		const orphan = makeRule({ name: "orphan" });

		const { rulebookRules, alwaysApplyRules } = bucketRules([sticky, book, orphan], mgr);

		expect(alwaysApplyRules.map(r => r.name)).toEqual(["sticky"]);
		expect(rulebookRules.map(r => r.name)).toEqual(["book"]);
		expect(mgr.hasRules()).toBe(false);
	});

	it("disabledRules drops a rule from every bucket and from TTSR registration", () => {
		const mgr = new TtsrManager();
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"], description: "blocks foo" });
		const book = makeRule({ name: "book", description: "rulebook desc" });

		const { rulebookRules } = bucketRules([ttsr, book], mgr, { disabledRules: ["no-foo", "book"] });

		expect(rulebookRules).toHaveLength(0);
		expect(mgr.hasRules()).toBe(false);
		expect(mgr.checkDelta("contains FORBIDDEN token", { source: "text" })).toHaveLength(0);
	});

	it("disabledRules trims entries and ignores blanks", () => {
		const mgr = new TtsrManager();
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"] });

		bucketRules([ttsr], mgr, { disabledRules: ["  no-foo  ", "", "   "] });

		expect(mgr.hasRules()).toBe(false);
	});

	it("builtinRules:false drops builtin-defaults rules but keeps the rest", () => {
		const mgr = new TtsrManager();
		const builtin = makeRule({
			name: "builtin-foo",
			condition: ["FORBIDDEN"],
			_source: source(BUILTIN_DEFAULTS_PROVIDER_ID),
		});
		const userRule = makeRule({ name: "user-foo", condition: ["BANNED"], _source: source("native") });

		bucketRules([builtin, userRule], mgr, { builtinRules: false });

		expect(mgr.checkDelta("contains FORBIDDEN token", { source: "text" })).toHaveLength(0);
		mgr.resetBuffer();
		expect(mgr.checkDelta("contains BANNED token", { source: "text" }).map(r => r.name)).toEqual(["user-foo"]);
	});

	it("includes builtin-defaults rules when builtinRules is unset (default on)", () => {
		const mgr = new TtsrManager();
		const builtin = makeRule({
			name: "builtin-foo",
			condition: ["FORBIDDEN"],
			_source: source(BUILTIN_DEFAULTS_PROVIDER_ID),
		});

		bucketRules([builtin], mgr);

		expect(mgr.checkDelta("contains FORBIDDEN token", { source: "text" }).map(r => r.name)).toEqual(["builtin-foo"]);
	});

	it("falls condition rules through to the rulebook when ttsr is disabled on the manager", () => {
		const mgr = new TtsrManager({
			enabled: false,
			contextMode: "discard",
			interruptMode: "always",
			repeatMode: "once",
			repeatGap: 10,
		});
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"], description: "blocks foo" });

		const { rulebookRules, alwaysApplyRules } = bucketRules([ttsr], mgr);

		// Manager refused to register; condition rule degrades to its rulebook shape.
		expect(mgr.hasRules()).toBe(false);
		expect(mgr.checkDelta("contains FORBIDDEN token", { source: "text" })).toEqual([]);
		expect(alwaysApplyRules.map(r => r.name)).toEqual([]);
		expect(rulebookRules.map(r => r.name)).toEqual(["no-foo"]);
	});

	// In-session refresh re-runs bucketRules against the SAME live manager. A
	// TTSR-conditioned rule the manager already holds must stay consumed on the
	// re-bucket — pre-fix it gated on addRule()'s name-idempotent return (false
	// the second time), so the already-registered rule fell through into the
	// rulebook and the advertised roster grew on every refresh.
	it("keeps a TTSR-conditioned rule consumed across a re-bucket with the same live manager", () => {
		const mgr = new TtsrManager();
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"], description: "blocks foo" });

		const first = bucketRules([ttsr], mgr);
		expect(first.rulebookRules).toHaveLength(0);
		expect(first.alwaysApplyRules).toHaveLength(0);

		// Same manager, second pass — the rule is already registered, so addRule
		// returns false; membership (hasRule) is what must keep it consumed.
		const second = bucketRules([ttsr], mgr);
		expect(second.rulebookRules).toHaveLength(0);
		expect(second.alwaysApplyRules).toHaveLength(0);
		expect(mgr.getRules().map(r => r.name)).toEqual(["no-foo"]);
	});

	// The fix must not OVER-consume: a rule with a TTSR condition the manager
	// REJECTS (here TTSR disabled) is not held, so it must still fall through to
	// the rulebook on both a first and a repeat bucketing — matching init.
	it("falls a manager-rejected TTSR rule through to the rulebook on every bucketing", () => {
		const mgr = disabledManager();
		const ttsr = makeRule({ name: "no-foo", condition: ["FORBIDDEN"], description: "blocks foo" });

		const first = bucketRules([ttsr], mgr);
		expect(mgr.hasRule("no-foo")).toBe(false);
		expect(first.rulebookRules.map(r => r.name)).toEqual(["no-foo"]);

		const second = bucketRules([ttsr], mgr);
		expect(mgr.hasRule("no-foo")).toBe(false);
		expect(second.rulebookRules.map(r => r.name)).toEqual(["no-foo"]);
	});

	// Observable roster stability: the rendered bucket count (rulebook + always)
	// must be identical across two identical re-buckets. Pre-fix it grew as
	// already-registered TTSR rules leaked into the rulebook.
	it("keeps the rendered bucket count stable across two identical re-buckets", () => {
		const mgr = new TtsrManager();
		const rules = [
			makeRule({ name: "ttsr-a", condition: ["ALPHA"], description: "a" }),
			makeRule({ name: "ttsr-b", astCondition: ["console.log($A)"], description: "b" }),
			makeRule({ name: "book-a", description: "rulebook a" }),
			makeRule({ name: "sticky-a", alwaysApply: true, description: "always a" }),
		];

		const first = bucketRules(rules, mgr);
		const second = bucketRules(rules, mgr);

		const count = (b: { rulebookRules: unknown[]; alwaysApplyRules: unknown[] }) =>
			b.rulebookRules.length + b.alwaysApplyRules.length;
		expect(count(first)).toBe(2);
		expect(count(second)).toBe(count(first));
	});

	// TtsrManager.hasRule is the membership predicate bucketRules now gates on.
	// It must be true for a registered rule (whether or not this call registered
	// it), false for an unknown name, and false after a rejected addRule.
	it("TtsrManager.hasRule reports membership independent of addRule's return", () => {
		const mgr = new TtsrManager();
		const rule = makeRule({ name: "no-foo", condition: ["FORBIDDEN"] });

		expect(mgr.hasRule("no-foo")).toBe(false);
		expect(mgr.addRule(rule)).toBe(true);
		expect(mgr.hasRule("no-foo")).toBe(true);
		// Idempotent: second add returns false, but membership persists.
		expect(mgr.addRule(rule)).toBe(false);
		expect(mgr.hasRule("no-foo")).toBe(true);
		expect(mgr.hasRule("never-added")).toBe(false);
	});

	it("TtsrManager.hasRule stays false after a rejected addRule", () => {
		const disabled = disabledManager();
		expect(disabled.addRule(makeRule({ name: "no-foo", condition: ["FORBIDDEN"] }))).toBe(false);
		expect(disabled.hasRule("no-foo")).toBe(false);

		// Empty condition set is also rejected even when TTSR is enabled.
		const enabled = new TtsrManager();
		expect(enabled.addRule(makeRule({ name: "empty", description: "no conditions" }))).toBe(false);
		expect(enabled.hasRule("empty")).toBe(false);
	});
});
