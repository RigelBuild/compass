import { expect, test } from "bun:test";
import { createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import { mountShell } from "./mount";
import { applyReduceMotion, REDUCE_MOTION_KEY } from "./preferences";
import { createAppStore } from "./store";
import { testQueryClient } from "./test-support";

test("mount applies the stored motion preference to the document root", () => {
	localStorage.setItem(REDUCE_MOTION_KEY, "on");
	let disposeStore = (): void => {};
	const store = createRoot((dispose) => {
		disposeStore = dispose;
		return createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
	});
	const unmount = mountShell(
		document.createElement("div"),
		store,
		testQueryClient(),
	);
	try {
		expect(document.documentElement.getAttribute("data-reduce")).toBe("on");
	} finally {
		unmount();
		disposeStore();
		applyReduceMotion(document.documentElement, "system");
		localStorage.removeItem(REDUCE_MOTION_KEY);
	}
});
