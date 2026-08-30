import { type Component, For } from "solid-js";
import { STUB_USAGE } from "../stub-data";

const fmtTokens = (n: number): string =>
	n >= 1_000_000
		? `${(n / 1_000_000).toFixed(1)}M`
		: `${(n / 1000).toFixed(0)}k`;

/** Percent of an account's token quota consumed, for the meter fill + label.
 *  A limit of 0 means unlimited / not-yet-loaded — no bounded ratio to show, so
 *  read 0% rather than Infinity. Clamped to [0,100] so a real over-limit account
 *  can't overflow the meter past full. */
export const usagePct = (used: number, limit: number): number =>
	limit > 0 ? Math.min(100, Math.max(0, Math.round((used / limit) * 100))) : 0;

/** The bottom usage bar: per-account token usage, rate-limit reset, and cost —
 *  mirrors Orca's account/usage strip. */
export const UsageBar: Component = () => (
	<footer class="usage">
		<For each={STUB_USAGE}>
			{(acct) => {
				const pct = () => usagePct(acct.tokensUsed, acct.tokensLimit);
				return (
					<div class="usage-item">
						<span class="u-provider">{acct.provider}</span>
						<span class="u-plan">{acct.plan}</span>
						<span class="usage-meter">
							<span
								class={["fill", { hot: pct() >= 80 }]}
								style={{ width: `${pct()}%` }}
							/>
						</span>
						<span class="u-pct">{pct()}%</span>
						<span class="u-reset">
							{fmtTokens(acct.tokensUsed)}/{fmtTokens(acct.tokensLimit)} ·
							resets {acct.resetIn}
						</span>
					</div>
				);
			}}
		</For>
		<span class="usage-spacer" />
		<span class="usage-git">
			<span aria-hidden="true">⎇</span> compass · main
		</span>
	</footer>
);
