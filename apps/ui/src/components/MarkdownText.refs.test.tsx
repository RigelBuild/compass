import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import type { Account } from "../comms-stub";
import { RefLinkRulesContext } from "../context";
import { createFakeCompass } from "../live/compass-fake";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { MarkdownText } from "./MarkdownText";

const byHandle = new Map<string, Account>();

async function mount(text: string): Promise<HTMLElement> {
	const compass = createFakeCompass();
	const { container } = render(() => {
		const store = createAppStore({
			queryClient: testQueryClient(),
			compass: compass.client,
		});
		return (
			<RefLinkRulesContext value={store.refLinkRules}>
				<MarkdownText text={text} byHandle={byHandle} />
			</RefLinkRulesContext>
		);
	});
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		flush();
	}
	return container;
}

describe("Markdown reference links", () => {
	test("links prose refs but not inline code, code blocks, or existing links", async () => {
		const container = await mount(
			"RIG-1 and `RIG-2` and [RIG-3](https://example.com)\n\n    RIG-4",
		);
		const links = [...container.querySelectorAll("a")];
		expect(
			links.map((link) => [link.textContent, link.getAttribute("href")]),
		).toEqual([
			["RIG-1", "https://linear.app/rigel/issue/RIG-1"],
			["RIG-3", "https://example.com"],
		]);
		expect(container.querySelector("code")?.textContent).toBe("RIG-2");
		expect(container.textContent).toContain("RIG-4");
		expect(container.querySelector("pre a")).toBeNull();
	});

	test("leaves an unmatched Linear team key as plain text", async () => {
		const container = await mount("OPS-7 stays plain");
		expect(container.querySelector("a")).toBeNull();
		expect(container.textContent).toBe("OPS-7 stays plain");
	});
});
