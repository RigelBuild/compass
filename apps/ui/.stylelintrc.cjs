// Stylelint blocks retired tokens and raw motion values outside tokens.css.
// Only the design-system tokens are valid in component and app styles.
module.exports = {
	rules: {
		// Reject raw colors and primitive tokens outside tokens.css.
		"color-no-hex": true,

		"declaration-property-value-disallowed-list": {
			// Check every declaration value.
			"/.*/": [
				// Catch embedded hex values missed by color-no-hex.
				"/#[0-9a-fA-F]{3,8}\\b/",
				// Components use semantic tokens, not primitive references.
				"/var\\(\\s*--rigel-/",
				// Match retired variable names at a boundary; leave design tokens alone.
				"/var\\(\\s*--(bg|text|st|accent|purple|pink|radius|font-mono|add|del|warn|border|danger|surface|fg|bg-inset|text-muted)[-),\\s]/",
			],
			// Motion uses tokens; app.css retains existing exceptions.
			"/^(transition|animation)/": [
				"/(?:^|[\\s,])\\d*\\.?\\d+m?s(?:$|[\\s,])/", // raw Nms/Ns durations
				"/cubic-bezier\\(/", // raw cubic-bezier easing
				"/(?:^|[\\s,])(ease|ease-in|ease-out|ease-in-out|linear)(?:$|[\\s,])/", // raw easing keywords
			],
		},
		// The brand uses square corners; boot-styles.ts is not covered.
		"property-disallowed-list": ["/radius/"],
		// Restrict weights to faces shipped by Space Mono.
		"declaration-property-value-allowed-list": {
			"font-weight": ["400", "normal", "inherit"],
			// The shorthand can carry a weight too, so it may only inherit.
			font: ["inherit"],
		},
	},
	overrides: [
		{
			// Raw colors, primitives, and motion values are valid only here.
			files: ["**/design/tokens.css"],
			rules: {
				"color-no-hex": null,
				"declaration-property-value-disallowed-list": null,
				// @font-face descriptors declare the 700 face.
				"declaration-property-value-allowed-list": null,
			},
		},
	],
};
