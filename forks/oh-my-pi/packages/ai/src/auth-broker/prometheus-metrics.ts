/**
 * Prometheus text-exposition renderer for the auth-broker `/metrics` endpoint
 * (SEA-1242). Pure `UsageReport[] -> string`: no I/O and no metrics library, so
 * it stays dependency-free on the broker and unit-testable in isolation.
 *
 * Emits the `llm_usage_*` gauge family the sealed LLM-usage dashboard binds to.
 * Those names are the cross-repo contract — the dashboard exprs use them
 * verbatim, so the renderer must not rename a family. The label set is bounded:
 * {provider, account, email, limit_id, window} (+ unit on the raw-amount
 * families). `email` is exported to Grafana Cloud by design (Matt's call) so the
 * six subscription accounts are human-readable on the dashboard. Windows are
 * rows, never hardcoded tiers, so a new limit window appears as a new series
 * with zero renderer change.
 */
import type { UsageLimit, UsageReport, UsageStatus } from "../usage";
import { resolveUsedFraction } from "../usage";

/** Content-type for a Prometheus v0.0.4 text exposition response. */
export const PROMETHEUS_CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8";

/** Sentinel account label when a report carries no stable account id. */
export const UNIDENTIFIED_ACCOUNT = "unidentified";

/**
 * Stable, opaque account label derived from report data alone (SEA-1242 OQ4).
 *
 * The renderer sees only the {@link UsageReport}, never the credential, so the
 * label must come from the report. `accountId` placement is inconsistent across
 * providers: Codex sets `metadata.accountId` (and `scope.accountId` on its
 * additional limits); Claude's profile path sets `metadata.accountId`; Claude's
 * ratelimit-header path carries none. So check `metadata.accountId`, then fall
 * back to any `limit.scope.accountId`, else the `unidentified` sentinel. Stays
 * the opaque stable id because it is the series/join key the dashboard joins on
 * — the human-readable address rides as its own `email` label. Never a report
 * ordinal (unstable under the null-filtered report fan-out — a dropped
 * credential would remap every later account).
 */
export function accountLabelOf(report: UsageReport): string {
	const metaId = report.metadata?.accountId;
	if (typeof metaId === "string" && metaId.length > 0) return metaId;
	for (const limit of report.limits) {
		const scopeId = limit.scope.accountId;
		if (typeof scopeId === "string" && scopeId.length > 0) return scopeId;
	}
	return UNIDENTIFIED_ACCOUNT;
}

/**
 * Human-readable account email label, read from `report.metadata?.email`.
 *
 * `metadata` is untyped (`Record<string, unknown>`), so type-guard it; a missing
 * or non-string value emits `email=""` rather than dropping the label, since an
 * inconsistent label set across samples of one family fails the scrape at parse.
 * Canonicalized here (trim + lowercase) because the providers disagree: the
 * Codex path normalizes through `normalizeEmail` (trim + lowercase), while the
 * Claude payload path only trims and never case-folds. `email` is part of every
 * `llm_usage_*` series identity, so a case or whitespace divergence would split
 * one account into two timeseries. Exported to Grafana Cloud by design (Matt's
 * call).
 */
export function emailLabelOf(report: UsageReport): string {
	const email = report.metadata?.email;
	if (typeof email === "string") return email.trim().toLowerCase();
	return "";
}

/** Numeric gauge value per usage status; absent AND `unknown` both map to -1. */
const STATUS_VALUE: Record<UsageStatus, number> = {
	ok: 0,
	warning: 1,
	exhausted: 2,
	unknown: -1,
};

/** Format a numeric sample value; Go-parseable floats incl. the Inf/NaN forms. */
function formatValue(value: number): string {
	if (Number.isNaN(value)) return "NaN";
	if (value === Number.POSITIVE_INFINITY) return "+Inf";
	if (value === Number.NEGATIVE_INFINITY) return "-Inf";
	return String(value);
}

type Label = readonly [string, string];

/** Escape a label value for text exposition: backslash, quote, then newline. */
function escapeLabelValue(value: string): string {
	return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n");
}

function renderLabels(labels: readonly Label[]): string {
	if (labels.length === 0) return "";
	const inner = labels.map(([k, v]) => `${k}="${escapeLabelValue(v)}"`).join(",");
	return `{${inner}}`;
}

interface Sample {
	readonly labels: readonly Label[];
	readonly value: number;
}

interface MetricFamily {
	readonly name: string;
	readonly help: string;
	readonly samples: Sample[];
}

/**
 * Render usage reports as Prometheus text. `opts.accountLabel` and
 * `opts.emailLabel` are injectable for tests; they default to
 * {@link accountLabelOf} and {@link emailLabelOf}. Returns an empty string when
 * there are no samples (the endpoint still answers 200 — an absent series set is
 * the signal the dashboard's expected-accounts panel reads).
 */
export function renderUsageMetrics(
	reports: readonly UsageReport[],
	opts: { accountLabel?: (report: UsageReport) => string; emailLabel?: (report: UsageReport) => string } = {},
): string {
	const accountLabel = opts.accountLabel ?? accountLabelOf;
	const emailLabel = opts.emailLabel ?? emailLabelOf;

	// Families in canonical emission order. `_used`/`_max`/`_remaining` carry an
	// extra `unit` label; the others key on {provider, account, email, limit_id,
	// window} (or {provider, account, email} for the per-report families).
	const families: MetricFamily[] = [
		{
			name: "llm_usage_limit_used_fraction",
			help: "Fraction (0..1) of a usage limit consumed; >1 means overage.",
			samples: [],
		},
		{ name: "llm_usage_limit_used", help: "Amount used for a usage limit, in the series unit label.", samples: [] },
		{ name: "llm_usage_limit_max", help: "Maximum for a usage limit, in the series unit label.", samples: [] },
		{
			name: "llm_usage_limit_remaining",
			help: "Remaining amount for a usage limit, in the series unit label.",
			samples: [],
		},
		{
			name: "llm_usage_limit_resets_at_seconds",
			help: "Unix time (seconds) at which a usage-limit window resets.",
			samples: [],
		},
		{
			name: "llm_usage_limit_status",
			help: "Usage-limit status: 0 ok, 1 warning, 2 exhausted, -1 unknown.",
			samples: [],
		},
		{
			name: "llm_usage_reset_credits_available",
			help: "Saved rate-limit resets an account can redeem right now.",
			samples: [],
		},
		{
			name: "llm_usage_report_fetched_at_seconds",
			help: "Unix time (seconds) the usage report for an account was last fetched.",
			samples: [],
		},
	];
	const byName = new Map(families.map(f => [f.name, f]));
	// Per-family seen-key set: a duplicate {name, labels} fails the WHOLE scrape
	// at parse, so drop-and-note the collision rather than emit it or suffix it.
	const seen = new Map<string, Set<string>>(families.map(f => [f.name, new Set<string>()]));
	const notes: string[] = [];

	const add = (name: string, labels: readonly Label[], value: number | undefined): void => {
		if (value === undefined) return;
		const family = byName.get(name);
		const seenSet = seen.get(name);
		if (!family || !seenSet) return;
		const key = [...labels]
			.sort(([a], [b]) => a.localeCompare(b))
			.map(([k, v]) => `${k}=${v}`)
			.join(",");
		if (seenSet.has(key)) {
			// Identify the collided family and its `limit_id` only: a note is a
			// comment line, so any raw label value here would both escape the
			// exposition escaping and leak the address into a non-sample line.
			const limitId = labels.find(([k]) => k === "limit_id")?.[1];
			notes.push(
				limitId === undefined
					? `duplicate series dropped: ${name}`
					: `duplicate series dropped: ${name}{limit_id="${escapeLabelValue(limitId)}"}`,
			);
			return;
		}
		seenSet.add(key);
		family.samples.push({ labels, value });
	};

	for (const report of reports) {
		const provider = report.provider;
		const account = accountLabel(report);
		const email = emailLabel(report);
		const perAccount: readonly Label[] = [
			["provider", provider],
			["account", account],
			["email", email],
		];

		add("llm_usage_report_fetched_at_seconds", perAccount, report.fetchedAt / 1000);
		if (report.resetCredits) {
			add("llm_usage_reset_credits_available", perAccount, report.resetCredits.availableCount);
		}

		for (const limit of report.limits) {
			const base: readonly Label[] = [
				["provider", provider],
				["account", account],
				["email", email],
				["limit_id", limit.id],
				["window", limit.window?.id ?? ""],
			];
			addLimit(add, base, limit);
		}
	}

	const lines: string[] = [];
	for (const family of families) {
		if (family.samples.length === 0) continue;
		lines.push(`# HELP ${family.name} ${family.help}`);
		lines.push(`# TYPE ${family.name} gauge`);
		for (const sample of family.samples) {
			lines.push(`${family.name}${renderLabels(sample.labels)} ${formatValue(sample.value)}`);
		}
	}
	for (const note of notes) lines.push(`# note ${note}`);
	return lines.length === 0 ? "" : `${lines.join("\n")}\n`;
}

/** Emit the per-limit families for one {@link UsageLimit} under `base` labels. */
function addLimit(
	add: (name: string, labels: readonly Label[], value: number | undefined) => void,
	base: readonly Label[],
	limit: UsageLimit,
): void {
	add("llm_usage_limit_used_fraction", base, resolveUsedFraction(limit));

	const withUnit: readonly Label[] = [...base, ["unit", limit.amount.unit]];
	add("llm_usage_limit_used", withUnit, limit.amount.used);
	add("llm_usage_limit_max", withUnit, limit.amount.limit);
	add("llm_usage_limit_remaining", withUnit, limit.amount.remaining);

	if (limit.window?.resetsAt !== undefined) {
		add("llm_usage_limit_resets_at_seconds", base, limit.window.resetsAt / 1000);
	}
	add("llm_usage_limit_status", base, STATUS_VALUE[limit.status ?? "unknown"]);
}
