// D7 stylelint guard (RIG-2034 T6) — the DS-token-cutover ratchet.
//
// Bans, at ERROR severity (stylelint's default; a violation exits non-zero and
// fails `moon run compass-ui:stylelint` -> CI), the vocabulary the cutover
// retired, so a revived legacy token or an off-DS literal reds CI instead of
// silently resolving to `inherit`. `design/tokens.css` is the sole file where
// the primitive tier (raw hex, `--rigel-*`, literal durations/easings) is
// legal — it IS the definition file — and is exempted via the `overrides` block
// below. Component + app CSS may name ONLY `--cx-*` (+ the three layout knobs).
//
// Active bans at error: raw hex (1), `--rigel-*` refs (2), literal durations/
// easings (3), legacy-var vocabulary by name (4). Ban 3 catches a bare `0.12s`/
// `ease`/`cubic-bezier()` on a transition/animation — motion must flow from
// `--cx-motion-*`/`--cx-ease-*`. The 8 pre-existing raw-motion literals in
// app.css (this color-cutover lane never scoped the motion axis, D9/foundation-
// T8) are grandfathered with `stylelint-disable-next-line` markers the motion
// lane removes on migration (Matt ruling 2026-08-15) — the ratchet stays whole,
// so any NEW raw motion reds CI.
//
// Refs RIG-2034; design: docs/designs/ui/compass-ds-token-cutover/design.md (e)
module.exports = {
	rules: {
		// D7 ban 1a — raw hex color literals. Catches hex in properties stylelint
		// knows are colors (`color`, `background`, `border-color`, …). Off-DS color
		// must flow from a `--cx-*` token; the primitives live in tokens.css only.
		"color-no-hex": true,

		"declaration-property-value-disallowed-list": {
			// Applies to EVERY declaration (`/.*/`):
			"/.*/": [
				// D7 ban 1b — raw hex ANYWHERE in a value, including inside
				// color-mix()/gradients/shadows where `color-no-hex` does not reach.
				"/#[0-9a-fA-F]{3,8}\\b/",
				// D7 ban 2 — `--rigel-*` primitive references. Components consume the
				// semantic `--cx-*` tier only; the primitive tier is tokens.css-only.
				"/var\\(\\s*--rigel-/",
				// D7 ban 4 — the LEGACY VAR VOCABULARY BY NAME. A revived legacy var
				// (`var(--bg)`, `var(--st-working)`, `var(--purple)`, …) reds CI here
				// instead of resolving to `inherit`. Anchored on `var(--<name>` with a
				// name-boundary char `[-),\s]`, so `var(--cx-bg…)` / `var(--cx-st-*)`
				// (the DS tier) and the layout knobs `--topbar-h`/`--usage-h`/`--right-w`
				// are NOT matched — only the exact retired names and their `-<suffix>`
				// families (`--bg-raised`, `--text-dim`, `--border-strong`, …).
				"/var\\(\\s*--(bg|text|st|accent|purple|pink|radius|font-mono|add|del|warn|border|danger|surface|fg|bg-inset|text-muted)[-),\\s]/",
			],
			// D7 ban 3 — literal durations/easings on motion properties (ACTIVE, Matt
			// ruling 2026-08-15). Motion must flow from `--cx-motion-*`/`--cx-ease-*`
			// (backed by `--rigel-*`), never a bare `0.12s`/`ease`/`cubic-bezier(...)`.
			// Scoped to the transition/animation shorthands so a bare number+`s`/`ms`
			// or an easing keyword elsewhere is not mistaken for motion. (steps() is a
			// discrete timing function the DS uses inline with a tokenized duration and
			// is NOT a duration/cubic-bezier/ease literal, so it is intentionally not
			// banned.) The 8 pre-existing raw-motion literals in app.css this color
			// lane never scoped (the motion axis is D9/foundation-T8) are grandfathered
			// with `stylelint-disable-next-line` markers the motion lane removes on
			// migration — so the ratchet catches every NEW raw motion.
			"/^(transition|animation)/": [
				"/(?:^|[\\s,])\\d*\\.?\\d+m?s(?:$|[\\s,])/", // raw Nms/Ns durations
				"/cubic-bezier\\(/", // raw cubic-bezier easing
				"/(?:^|[\\s,])(ease|ease-in|ease-out|ease-in-out|linear)(?:$|[\\s,])/", // raw easing keywords
			],
		},
	},
	overrides: [
		{
			// The primitive+semantic definition file: raw hex, `--rigel-*`, and
			// literal durations/easings are legal here and ONLY here.
			files: ["**/design/tokens.css"],
			rules: {
				"color-no-hex": null,
				"declaration-property-value-disallowed-list": null,
			},
		},
	],
};
