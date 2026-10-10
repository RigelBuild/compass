import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush, mountApp } from "../../test-router";

afterEach(cleanup);

describe("AppearanceSection", () => {
	test("Always and Follow system apply and save immediately", async () => {
		localStorage.removeItem("compass.settings.reduceMotion");
		const { store, container } = mountApp("/settings/appearance");
		await flush();
		const group = container.querySelector(
			'[role="group"][aria-label="Reduce motion"]',
		);
		const buttons = [
			...container.querySelectorAll<HTMLButtonElement>("button"),
		];
		const always = buttons.find(
			(button) => button.textContent?.trim() === "Always",
		);
		const followSystem = buttons.find(
			(button) => button.textContent?.trim() === "Follow system",
		);
		if (!group || !always || !followSystem) {
			throw new Error("missing Reduce motion controls");
		}
		expect(group.getAttribute("aria-label")).toBe("Reduce motion");
		expect(followSystem.getAttribute("aria-pressed")).toBe("true");
		expect(followSystem.dataset.selected).toBe("");

		fireEvent.click(always);
		await flush();
		expect(always.getAttribute("aria-pressed")).toBe("true");
		expect(followSystem.getAttribute("aria-pressed")).toBe("false");
		expect(store.reduceMotion()).toBe("on");
		expect(always.dataset.selected).toBe("");
		expect(document.documentElement.getAttribute("data-reduce")).toBe("on");
		expect(localStorage.getItem("compass.settings.reduceMotion")).toBe("on");

		fireEvent.click(followSystem);
		await flush();
		expect(followSystem.getAttribute("aria-pressed")).toBe("true");
		expect(store.reduceMotion()).toBe("system");
		expect(followSystem.dataset.selected).toBe("");
		expect(always.getAttribute("aria-pressed")).toBe("false");
		expect(always.hasAttribute("data-selected")).toBe(false);
		expect(document.documentElement.hasAttribute("data-reduce")).toBe(false);
		expect(localStorage.getItem("compass.settings.reduceMotion")).toBe(
			"system",
		);
	});
});
