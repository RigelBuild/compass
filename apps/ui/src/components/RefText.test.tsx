import { describe, expect, spyOn, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { RefLinkRulesContext } from "../context";
import { createFakeCompass } from "../live/compass-fake";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { RefText } from "./RefText";

async function mount(text: string): Promise<HTMLElement> {
	const compass = createFakeCompass();
	const { container } = render(() => {
		const store = createAppStore({
			queryClient: testQueryClient(),
			compass: compass.client,
		});
		return (
			<RefLinkRulesContext value={store.refLinkRules}>
				<RefText text={text} />
			</RefLinkRulesContext>
		);
	});
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		flush();
	}
	return container;
}

describe("RefText", () => {
	test("links configured refs and leaves unmatched refs and bare numbers plain", async () => {
		const container = await mount("RIG-12 owner/repo#3 #17 OPS-4");
		const links = [...container.querySelectorAll("a")];
		expect(
			links.map((link) => [link.textContent, link.getAttribute("href")]),
		).toEqual([
			["RIG-12", "https://linear.app/rigel/issue/RIG-12"],
			["owner/repo#3", "https://github.com/owner/repo/issues/3"],
		]);
		expect(container.textContent).toBe("RIG-12 owner/repo#3 #17 OPS-4");
	});

	test("activates a linked ref through the external opener", async () => {
		const opener = spyOn(window, "open").mockImplementation(() => null);
		try {
			const container = await mount("See RIG-12");
			const link = container.querySelector("a");
			if (!link) throw new Error("reference link did not render");
			const click = new MouseEvent("click", {
				bubbles: true,
				cancelable: true,
			});
			link.dispatchEvent(click);
			expect(click.defaultPrevented).toBe(true);
			expect(opener).toHaveBeenCalledWith(
				"https://linear.app/rigel/issue/RIG-12",
				"_blank",
				"noreferrer,noopener",
			);
			expect(link.getAttribute("target")).toBe("_blank");
			expect(link.getAttribute("rel")).toBe("noreferrer noopener");
		} finally {
			opener.mockRestore();
		}
	});
});
