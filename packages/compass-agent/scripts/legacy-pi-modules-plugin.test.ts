import { describe, expect, test } from "bun:test";
import {
	collectLegacyPiModuleEntries,
	renderLegacyPiVirtualModule,
} from "./legacy-pi-modules-plugin";

describe("collectLegacyPiModuleEntries", () => {
	test("includes each installed package, typebox, and nested wildcard exports", async () => {
		const entries = await collectLegacyPiModuleEntries();
		const keys = entries.map((entry) => entry.key);
		expect(keys).toEqual(
			expect.arrayContaining([
				"@oh-my-pi/pi-agent-core",
				"@oh-my-pi/pi-ai",
				"@oh-my-pi/pi-coding-agent",
				"@oh-my-pi/pi-natives",
				"@oh-my-pi/pi-tui",
				"@oh-my-pi/pi-utils",
				"typebox",
				"@oh-my-pi/pi-coding-agent/web/search/providers/anthropic",
			]),
		);

		const rendered = renderLegacyPiVirtualModule(entries);
		for (const entry of entries) {
			expect(rendered).toContain(
				`const ${entry.binding} = () => import(${JSON.stringify(entry.importSpecifier)});`,
			);
		}
	});
});
