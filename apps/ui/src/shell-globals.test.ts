/// <reference types="bun" />
import { afterEach, describe, expect, test } from "bun:test";
import { setShellServerUrl, shellMode, shellServerUrl } from "./shell-globals";

// The two shell-injected startup globals are the synchronous, no-IPC source of
// truth for launch mode + server URL (OQ-8). The readers must return whatever
// the shell set, and tolerate their absence (a browser dev build sets neither)
// without throwing. `window` exists here under happy-dom, so absence is the
// undefined value, not a missing global object.

const w = window;

afterEach(() => {
	w.__COMPASS_MODE__ = undefined;
	w.__COMPASS_SERVER_URL__ = undefined;
});

describe("shellMode / shellServerUrl", () => {
	test("read the injected globals when the shell set them", () => {
		w.__COMPASS_MODE__ = "client";
		w.__COMPASS_SERVER_URL__ = "https://compass.example:8443";

		expect(shellMode()).toBe("client");
		expect(shellServerUrl()).toBe("https://compass.example:8443");
	});

	test("report undefined when absent (browser dev build)", () => {
		expect(shellMode()).toBeUndefined();
		expect(shellServerUrl()).toBeUndefined();
	});

	test("distinguish embedded from client mode", () => {
		w.__COMPASS_MODE__ = "embedded";
		expect(shellMode()).toBe("embedded");
	});
	test("recognizes setup and reopen shell modes", () => {
		w.__COMPASS_MODE__ = "setup";
		expect(shellMode()).toBe("setup");
		w.__COMPASS_MODE__ = "reopen";
		expect(shellMode()).toBe("reopen");
	});

	test("updates the live server URL for a configured handoff", () => {
		setShellServerUrl("https://sibling.example");
		expect(shellServerUrl()).toBe("https://sibling.example");
	});
});
