import { describe, expect, test } from "bun:test";
import {
	applyReduceMotion,
	loadReduceMotion,
	REDUCE_MOTION_KEY,
	saveReduceMotion,
} from "./preferences";

const root = document.createElement("div");

describe("reduce motion preference", () => {
	test("defaults missing and invalid values to system", () => {
		localStorage.removeItem(REDUCE_MOTION_KEY);
		expect(loadReduceMotion(localStorage)).toBe("system");

		localStorage.setItem(REDUCE_MOTION_KEY, "invalid");
		expect(loadReduceMotion(localStorage)).toBe("system");
	});

	test("loads and saves Always", () => {
		localStorage.setItem(REDUCE_MOTION_KEY, "on");
		expect(loadReduceMotion(localStorage)).toBe("on");
		saveReduceMotion(localStorage, "system");
		expect(localStorage.getItem(REDUCE_MOTION_KEY)).toBe("system");
	});

	test("applies Always and removes the override for Follow system", () => {
		applyReduceMotion(root, "on");
		expect(root.getAttribute("data-reduce")).toBe("on");
		applyReduceMotion(root, "system");
		expect(root.hasAttribute("data-reduce")).toBe(false);
	});

	test("tolerates storage reads and writes that throw", () => {
		const throwingStorage = {
			length: 0,
			clear: () => {},
			getItem: () => {
				throw new Error("read blocked");
			},
			key: () => null,
			removeItem: () => {},
			setItem: () => {
				throw new Error("write blocked");
			},
		} satisfies Storage;

		expect(loadReduceMotion(throwingStorage)).toBe("system");
		expect(() => saveReduceMotion(throwingStorage, "on")).not.toThrow();
	});
});
