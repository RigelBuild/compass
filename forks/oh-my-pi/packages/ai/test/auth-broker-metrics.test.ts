import { describe, expect, test } from "bun:test";
import {
	accountLabelOf,
	emailLabelOf,
	renderUsageMetrics,
	UNIDENTIFIED_ACCOUNT,
} from "@oh-my-pi/pi-ai/auth-broker/prometheus-metrics";
import type { UsageReport } from "@oh-my-pi/pi-ai/usage";

// A Claude report on the profile path: accountId in metadata, two shared
// windows (5h + 7d) plus a model-scoped weekly row. resetsAt in ms.
function claudeReport(): UsageReport {
	return {
		provider: "anthropic",
		fetchedAt: 1_700_000_000_000,
		metadata: { endpoint: "https://api.anthropic.com", accountId: "acct-claude-1", email: "a@example.com" },
		limits: [
			{
				id: "anthropic:5h",
				label: "Claude 5 Hour",
				scope: { provider: "anthropic", windowId: "5h", shared: true },
				window: { id: "5h", label: "5 Hour", durationMs: 18_000_000, resetsAt: 1_700_000_900_000 },
				amount: {
					used: 42,
					limit: 100,
					remaining: 58,
					usedFraction: 0.42,
					remainingFraction: 0.58,
					unit: "percent",
				},
				status: "ok",
			},
			{
				id: "anthropic:7d",
				label: "Claude 7 Day",
				scope: { provider: "anthropic", windowId: "7d", shared: true },
				window: { id: "7d", label: "7 Day", durationMs: 604_800_000, resetsAt: 1_700_500_000_000 },
				amount: {
					used: 95,
					limit: 100,
					remaining: 5,
					usedFraction: 0.95,
					remainingFraction: 0.05,
					unit: "percent",
				},
				status: "warning",
			},
		],
	};
}

// A Codex report: accountId in metadata, resetCredits present, one limit with
// no status (must map to -1) and no window (window label "").
function codexReport(): UsageReport {
	return {
		provider: "openai-codex",
		fetchedAt: 1_700_000_060_000,
		metadata: { planType: "pro", accountId: "acct-codex-9", email: "c@example.com" },
		resetCredits: { availableCount: 3 },
		limits: [
			{
				id: "openai-codex:primary",
				label: "5 Hour",
				scope: { provider: "openai-codex", windowId: "5h", shared: true },
				window: { id: "5h", label: "5 Hour", resetsAt: 1_700_000_900_000 },
				amount: { used: 10, limit: 100, remaining: 90, usedFraction: 0.1, unit: "percent" },
				status: "exhausted",
			},
			{
				id: "openai-codex:extra",
				label: "Extra",
				scope: { provider: "openai-codex" },
				amount: { usedFraction: 0.5, unit: "percent" },
				// no status -> -1
			},
		],
	};
}

describe("renderUsageMetrics", () => {
	test("emits every llm_usage_ family with bounded labels including account and email", () => {
		const out = renderUsageMetrics([claudeReport(), codexReport()]);

		// Headers present, TYPE gauge.
		expect(out).toContain("# HELP llm_usage_limit_used_fraction");
		expect(out).toContain("# TYPE llm_usage_limit_used_fraction gauge");

		// used_fraction keyed on {provider, account, email, limit_id, window}.
		expect(out).toContain(
			'llm_usage_limit_used_fraction{provider="anthropic",account="acct-claude-1",email="a@example.com",limit_id="anthropic:5h",window="5h"} 0.42',
		);
		// resets_at converted ms -> s.
		expect(out).toContain(
			'llm_usage_limit_resets_at_seconds{provider="anthropic",account="acct-claude-1",email="a@example.com",limit_id="anthropic:5h",window="5h"} 1700000900',
		);
		// status enum: ok=0, warning=1, exhausted=2.
		expect(out).toContain(
			'llm_usage_limit_status{provider="anthropic",account="acct-claude-1",email="a@example.com",limit_id="anthropic:5h",window="5h"} 0',
		);
		expect(out).toContain(
			'llm_usage_limit_status{provider="anthropic",account="acct-claude-1",email="a@example.com",limit_id="anthropic:7d",window="7d"} 1',
		);
		expect(out).toContain(
			'llm_usage_limit_status{provider="openai-codex",account="acct-codex-9",email="c@example.com",limit_id="openai-codex:primary",window="5h"} 2',
		);
		// raw amount families carry unit label.
		expect(out).toContain(
			'llm_usage_limit_used{provider="anthropic",account="acct-claude-1",email="a@example.com",limit_id="anthropic:5h",window="5h",unit="percent"} 42',
		);
		// reset credits keyed on {provider, account, email} only.
		expect(out).toContain(
			'llm_usage_reset_credits_available{provider="openai-codex",account="acct-codex-9",email="c@example.com"} 3',
		);
		// fetched_at per account, ms -> s.
		expect(out).toContain(
			'llm_usage_report_fetched_at_seconds{provider="anthropic",account="acct-claude-1",email="a@example.com"} 1700000000',
		);
		// Email is exported by design: the account UUID is opaque, so the email
		// label is what makes a subscription account legible on the dashboard.
		expect(out).toContain('email="a@example.com"');
		expect(out).toContain('email="c@example.com"');
	});

	test('absent status maps to -1, and a windowless limit emits window=""', () => {
		const out = renderUsageMetrics([codexReport()]);
		expect(out).toContain(
			'llm_usage_limit_status{provider="openai-codex",account="acct-codex-9",email="c@example.com",limit_id="openai-codex:extra",window=""} -1',
		);
		// A limit with no window/resetsAt emits no resets_at series for it.
		expect(out).not.toContain(
			'llm_usage_limit_resets_at_seconds{provider="openai-codex",account="acct-codex-9",email="c@example.com",limit_id="openai-codex:extra"',
		);
		// used_fraction still emitted for the windowless limit.
		expect(out).toContain(
			'llm_usage_limit_used_fraction{provider="openai-codex",account="acct-codex-9",email="c@example.com",limit_id="openai-codex:extra",window=""} 0.5',
		);
	});

	test("empty reports render an empty (but valid) exposition", () => {
		expect(renderUsageMetrics([])).toBe("");
	});

	test("a limit with no amount values emits only status", () => {
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-x" },
			limits: [
				{
					id: "anthropic:5h",
					label: "Claude 5 Hour",
					scope: { provider: "anthropic", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { unit: "unknown" },
					status: "unknown",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		// unknown status -> -1; no email in metadata -> email="".
		expect(out).toContain(
			'llm_usage_limit_status{provider="anthropic",account="acct-x",email="",limit_id="anthropic:5h",window="5h"} -1',
		);
		// no used_fraction, used, max, remaining, resets_at families for this limit
		expect(out).not.toContain("llm_usage_limit_used_fraction");
		expect(out).not.toContain("llm_usage_limit_used{");
		expect(out).not.toContain("llm_usage_limit_resets_at_seconds");
	});

	test("escapes label values", () => {
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: 'quote"and\\slash' },
			limits: [
				{
					id: "anthropic:5h",
					label: "x",
					scope: { provider: "anthropic" },
					amount: { usedFraction: 0.1, unit: "percent" },
					status: "ok",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		expect(out).toContain('account="quote\\"and\\\\slash"');
		// No email in metadata -> the label is still emitted, empty.
		expect(out).toContain('email=""');
	});

	test("escapes the email label value", () => {
		// A newline in the value is the dangerous one: unescaped it splits the
		// sample across two physical lines and fails the whole scrape at parse.
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-esc", email: 'quote"and\\slash\nnewline@example.com' },
			limits: [
				{
					id: "anthropic:5h",
					label: "x",
					scope: { provider: "anthropic" },
					amount: { usedFraction: 0.1, unit: "percent" },
					status: "ok",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		expect(out).toContain('email="quote\\"and\\\\slash\\nnewline@example.com"');
		// Every sample line is whole: the newline never breaks one in two.
		const sampleLines = out.split("\n").filter(line => line.startsWith("llm_usage_"));
		expect(sampleLines.length).toBeGreaterThan(0);
		for (const line of sampleLines) {
			expect(line).toContain('email="quote\\"and\\\\slash\\nnewline@example.com"');
		}
	});

	test("drops a duplicate {name,labels} series and notes it", () => {
		// Two limits that produce the identical {provider,account,email,limit_id,window}
		// key — a duplicate sample would fail the whole scrape at parse.
		const report: UsageReport = {
			provider: "openai-codex",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-dup" },
			limits: [
				{
					id: "openai-codex:dup",
					label: "A",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.1, unit: "percent" },
					status: "ok",
				},
				{
					id: "openai-codex:dup",
					label: "B",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.9, unit: "percent" },
					status: "warning",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		// First wins (0.1), duplicate dropped, note emitted.
		expect(out).toContain(
			'llm_usage_limit_used_fraction{provider="openai-codex",account="acct-dup",email="",limit_id="openai-codex:dup",window="5h"} 0.1',
		);
		expect(out).not.toContain("} 0.9");
		expect(out).toContain("# note duplicate series dropped: llm_usage_limit_used_fraction");
	});

	test("escapes the limit_id in the duplicate-series note", () => {
		// `limit_id` is provider data and reaches the `# note` line on a collision.
		// A raw newline in it would split the note into a second physical line that
		// is neither a comment nor a valid sample, failing the whole scrape at parse.
		const hostileId = 'dup"a\\b\nc';
		const report: UsageReport = {
			provider: "openai-codex",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-dup" },
			limits: [
				{
					id: hostileId,
					label: "A",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.1, unit: "percent" },
					status: "ok",
				},
				{
					id: hostileId,
					label: "B",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.9, unit: "percent" },
					status: "warning",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		expect(out).toContain(
			'# note duplicate series dropped: llm_usage_limit_used_fraction{limit_id="dup\\"a\\\\b\\nc"}',
		);
		// Every physical line is still a comment or a sample.
		for (const line of out.split("\n").filter(l => l.length > 0)) {
			expect(line.startsWith("#") || /^llm_usage_\w+\{/.test(line)).toBe(true);
		}
	});

	test("every emitted line is a comment or a sample, even when the email holds a newline", () => {
		// The note path once concatenated raw label values, so a newline in the
		// email emitted a second physical line that is neither a `#` comment nor
		// a valid sample — the whole scrape fails at parse, not just this series.
		const report: UsageReport = {
			provider: "openai-codex",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-dup", email: "leak\nlocal@example.com" },
			limits: [
				{
					id: "openai-codex:dup",
					label: "A",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.1, unit: "percent" },
					status: "ok",
				},
				{
					id: "openai-codex:dup",
					label: "B",
					scope: { provider: "openai-codex", windowId: "5h" },
					window: { id: "5h", label: "5 Hour" },
					amount: { usedFraction: 0.9, unit: "percent" },
					status: "warning",
				},
			],
		};
		const out = renderUsageMetrics([report]);
		// The collision fired, so the note path is exercised by this render.
		expect(out).toContain("duplicate series dropped: ");

		const lines = out.split("\n").filter(line => line.length > 0);
		for (const line of lines) {
			if (line.startsWith("#")) continue;
			expect(line).toMatch(/^llm_usage_\w+\{/);
		}

		// The comment stream is not a PII surface: the email is a label, never
		// a value echoed into a note.
		for (const line of lines.filter(l => l.startsWith("#"))) {
			expect(line).not.toContain("leak");
			expect(line).not.toContain("local");
			expect(line).not.toContain("example.com");
		}
	});

	test("canonicalizes the email label so case/whitespace variants are one series", () => {
		// Same account seen twice with a differently-cased, padded email. If the
		// label were emitted verbatim the two reports would produce two distinct
		// per-account series instead of one collision.
		const base = (email: string): UsageReport => ({
			provider: "anthropic",
			fetchedAt: 1_700_000_000_000,
			metadata: { accountId: "acct-canon", email },
			limits: [],
		});
		const out = renderUsageMetrics([base("  A.User@Example.COM  "), base("a.user@example.com")]);

		expect(out).toContain(
			'llm_usage_report_fetched_at_seconds{provider="anthropic",account="acct-canon",email="a.user@example.com"} 1700000000',
		);
		expect(out).not.toContain("A.User@Example.COM");
		// One series, not two: the second report collided and was dropped.
		expect(out).toContain("duplicate series dropped: ");
		const samples = out.split("\n").filter(line => line.startsWith("llm_usage_report_fetched_at_seconds{"));
		expect(samples.length).toBe(1);
	});

	test("emits the email label on every sample, including reports that carry none", () => {
		// A label present on some samples of a family and absent on others is an
		// inconsistent label set; the scrape fails at parse.
		const withEmail = claudeReport();
		const withoutEmail = codexReport();
		withoutEmail.metadata = { planType: "pro", accountId: "acct-codex-9" };

		const out = renderUsageMetrics([withEmail, withoutEmail]);
		const samples = out.split("\n").filter(line => line.length > 0 && !line.startsWith("#"));
		expect(samples.length).toBeGreaterThan(0);
		for (const line of samples) expect(line).toContain("email=");
		// The email-less report still emits the label, empty.
		expect(out).toContain('account="acct-codex-9",email=""');
	});
});

describe("accountLabelOf", () => {
	test("prefers metadata.accountId", () => {
		expect(accountLabelOf(claudeReport())).toBe("acct-claude-1");
		expect(accountLabelOf(codexReport())).toBe("acct-codex-9");
	});

	test("falls back to a limit scope.accountId when metadata lacks one", () => {
		const report: UsageReport = {
			provider: "openai-codex",
			fetchedAt: 1,
			metadata: { planType: "pro" },
			limits: [
				{
					id: "openai-codex:extra:primary",
					label: "Extra",
					scope: { provider: "openai-codex", accountId: "scope-acct" },
					amount: { usedFraction: 0.1, unit: "percent" },
				},
			],
		};
		expect(accountLabelOf(report)).toBe("scope-acct");
		// The report carries no email, so the exposition emits email="".
		const out = renderUsageMetrics([report]);
		expect(out).toContain(
			'llm_usage_limit_used_fraction{provider="openai-codex",account="scope-acct",email="",limit_id="openai-codex:extra:primary",window=""} 0.1',
		);
	});

	test("falls back to the unidentified sentinel (Claude ratelimit-header path)", () => {
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1,
			metadata: { source: "ratelimit-headers" },
			limits: [
				{
					id: "anthropic:5h",
					label: "Claude 5 Hour",
					scope: { provider: "anthropic", windowId: "5h" },
					amount: { usedFraction: 0.1, unit: "percent" },
				},
			],
		};
		expect(accountLabelOf(report)).toBe(UNIDENTIFIED_ACCOUNT);
		expect(accountLabelOf(report)).toBe("unidentified");
	});

	test("never uses an email as the *account* label value", () => {
		// Scoped to the account label only: an email is never a substitute for
		// an accountId here. Email IS exported, but as its own `email` label.
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1,
			metadata: { email: "secret@example.com" },
			limits: [],
		};
		expect(accountLabelOf(report)).toBe(UNIDENTIFIED_ACCOUNT);
	});
});

describe("emailLabelOf", () => {
	test("returns metadata.email when present", () => {
		expect(emailLabelOf(claudeReport())).toBe("a@example.com");
		expect(emailLabelOf(codexReport())).toBe("c@example.com");
	});

	test('returns "" when metadata carries no email', () => {
		const report: UsageReport = {
			provider: "openai-codex",
			fetchedAt: 1,
			metadata: { planType: "pro", accountId: "acct-codex-9" },
			limits: [],
		};
		expect(emailLabelOf(report)).toBe("");
	});

	test('returns "" when metadata is absent entirely', () => {
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1,
			limits: [],
		};
		expect(emailLabelOf(report)).toBe("");
	});

	test('returns "" for a non-string email value', () => {
		const report: UsageReport = {
			provider: "anthropic",
			fetchedAt: 1,
			metadata: { email: 42 },
			limits: [],
		};
		expect(emailLabelOf(report)).toBe("");
	});
});
