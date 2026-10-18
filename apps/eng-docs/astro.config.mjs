// @ts-check

import starlight from "@astrojs/starlight";
import { defineConfig } from "astro/config";
import { sidebar } from "./src/sidebar.generated.ts";

// Public engineering docs gather canonical Markdown into a Starlight site.
// The generated sidebar uses the same gather classification.
export default defineConfig({
	// Placeholder until the domain lands (eng.compass.rigel.build). Q4 ruling.
	site: "https://compass-eng-docs.pages.dev",
	trailingSlash: "ignore",
	integrations: [
		starlight({
			title: "Compass Engineering Docs",
			description: "Compass — public engineering docs.",
			customCss: ["./src/styles/fonts.css", "./src/styles/custom.css"],
			// One dark Shiki theme: night-owl, so code token colors are drawn
			// from the same Night Owl palette the rest of the surface uses
			// (set in custom.css).
			expressiveCode: {
				themes: ["night-owl"],
				useStarlightDarkModeSwitch: false,
				themeCssSelector: false,
			},
			tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 4 },
			pagefind: true,
			// Generated nav: one group per populated section. gather.ts classifies
			// every source into a section, mirrors it under src/content/docs/<section>/,
			// and emits the matching autogenerate groups here.
			sidebar,
			// Per-page source URLs are required; the generated mirror has no canonical GitHub path.
			editLink: {},
			lastUpdated: true,
		}),
	],
});
