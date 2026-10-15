import { describe, expect, spyOn, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { type Accessor, flush } from "solid-js";
import { RefLinkRulesContext } from "../context";
import { createFakeCompass } from "../live/compass-fake";
import type { RefLinkRule } from "../refs/grammar";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { RefText } from "./RefText";

const LINEAR_RIG: Accessor<readonly RefLinkRule[]> = () => [
	{
		kind: "linear",
		prefix: "RIG",
		urlTemplate: "https://linear.app/rigel/issue/{key}-{number}",
	},
];

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

describe("RefText inside a button", () => {
	test("renders no anchor and opens the ref without activating the button", () => {
		const opener = spyOn(window, "open").mockImplementation(() => null);
		let clicks = 0;
		let dblclicks = 0;
		try {
			const { container } = render(() => (
				<RefLinkRulesContext value={LINEAR_RIG}>
					<button
						type="button"
						onClick={() => clicks++}
						onDblClick={() => dblclicks++}
					>
						<RefText text="Fix RIG-12" inControl />
					</button>
				</RefLinkRulesContext>
			));
			expect(container.querySelector("button a")).toBeNull();
			const ref = container.querySelector("button [role=link]");
			if (!ref) throw new Error("reference did not render");
			ref.dispatchEvent(
				new MouseEvent("click", { bubbles: true, cancelable: true }),
			);
			ref.dispatchEvent(
				new MouseEvent("dblclick", { bubbles: true, cancelable: true }),
			);
			expect(opener).toHaveBeenCalledWith(
				"https://linear.app/rigel/issue/RIG-12",
				"_blank",
				"noreferrer,noopener",
			);
			expect([clicks, dblclicks]).toEqual([0, 0]);
		} finally {
			opener.mockRestore();
		}
	});

	for (const key of ["Enter", " "]) {
		test(`${JSON.stringify(key)} on the ref opens it without activating the button`, () => {
			const opener = spyOn(window, "open").mockImplementation(() => null);
			let clicks = 0;
			try {
				const { container } = render(() => (
					<RefLinkRulesContext value={LINEAR_RIG}>
						<button type="button" onClick={() => clicks++}>
							<RefText text="Fix RIG-12" inControl />
						</button>
					</RefLinkRulesContext>
				));
				const ref = container.querySelector<HTMLElement>("button [role=link]");
				if (!ref) throw new Error("reference did not render");
				expect(ref.tabIndex).toBe(0);
				const press = new KeyboardEvent("keydown", {
					key,
					bubbles: true,
					cancelable: true,
				});
				ref.dispatchEvent(press);
				expect(press.defaultPrevented).toBe(true);
				expect(opener).toHaveBeenCalledTimes(1);
				expect(clicks).toBe(0);
			} finally {
				opener.mockRestore();
			}
		});
	}

	test("leaves the tab order inside the board roving group", () => {
		const { container } = render(() => (
			<RefLinkRulesContext value={LINEAR_RIG}>
				<button type="button">
					<RefText text="Fix RIG-12" inControl inRovingGroup />
				</button>
			</RefLinkRulesContext>
		));
		const ref = container.querySelector<HTMLElement>("button [role=link]");
		expect(ref?.tabIndex).toBe(-1);
	});
});
