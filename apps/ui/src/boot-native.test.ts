/// <reference types="bun" />
import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { bootNativeClient, type NativeBootDeps } from "./boot-native";
import type { ConnectResult } from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";

// The client-mode boot gate (boot-native.ts) drives the shell `Connect` probe
// and, on failure, the connect screen. The seam it consumes — `shellConnect`
// (the probe) and `nativeConnectionProvider` (the resolved connection) — is a
// `NativeBootDeps` injected as bootNativeClient's second argument, so no
// @wailsio/runtime, no IPC, and no network is needed. Injection over a
// `mock.module("./daemon-transport", …)` is deliberate: Bun's mock.module is
// process-global and its restore does not reliably rebind a sibling suite's
// named imports, so a whole-module mock of `./daemon-transport` leaked into
// daemon-transport.wails.test.ts and made both suites' outcomes depend on file
// order. The DI seam is self-contained to this suite.
// What is defended:
//   - the in-flight `connecting` state renders before the probe settles;
//   - each failure kind renders its distinct heading/copy;
//   - an ok probe resolves the native provider with the injected baseUrl and
//     token === undefined (DL-109);
//   - submit is disabled on empty input, so the empty-token sentinel is never a
//     user action;
//   - after a connect-button submit the token input is cleared and no binding
//     retains it (the stub records every token it was handed).

// The token(s) the stub was handed, in call order — the spy the retention
// assertion reads. A local capture array, never the production module's state.
let connectTokens: string[];
// A programmable queue of resolvers, one per shellConnect call; a test settles a
// probe by resolving the matching entry (holding it lets a probe stay in flight).
let pending: Array<(result: ConnectResult) => void>;
// The stub transport injected into bootNativeClient, rebuilt fresh per test.
let deps: NativeBootDeps;

function connectResult(over: Partial<ConnectResult>): ConnectResult {
	return {
		ok: false,
		kind: "other",
		message: "",
		accountId: "",
		serverVersion: "",
		apiVersion: "",
		...over,
	};
}

const NATIVE_CONNECTION: ResolvedConnection = {
	baseUrl: "https://compass.example:8443",
	token: undefined,
	fetchImpl: undefined,
};

beforeEach(() => {
	connectTokens = [];
	pending = [];
	deps = {
		shellConnect: (token: string): Promise<ConnectResult> => {
			connectTokens.push(token);
			const { promise, resolve } = Promise.withResolvers<ConnectResult>();
			pending.push(resolve);
			return promise;
		},
		nativeConnectionProvider: (baseUrl: string): ConnectionProvider => ({
			async resolve(): Promise<ResolvedConnection> {
				return { ...NATIVE_CONNECTION, baseUrl };
			},
		}),
	};
	window.__COMPASS_SERVER_URL__ = "https://compass.example:8443";
});

afterEach(() => {
	window.__COMPASS_SERVER_URL__ = undefined;
});

/** Settle the Nth (0-based) outstanding shellConnect call. */
function settle(index: number, result: ConnectResult): void {
	const resolve = pending[index];
	if (!resolve)
		throw new Error(`no pending shellConnect call at index ${index}`);
	resolve(result);
}

/** Drain the microtask queue so the gate's resolved-promise `.then` callbacks
 *  (and the render they perform) run — deterministic, no wall-clock timer. A few
 *  ticks cover the short then-chain (settle → resolve provider → paint). */
async function flush(): Promise<void> {
	for (let i = 0; i < 5; i++) {
		await Promise.resolve();
	}
}

describe("bootNativeClient — the boot gate", () => {
	test("renders the connecting state before the probe settles", async () => {
		const root = document.createElement("div");

		void bootNativeClient(root, deps);
		await flush();

		// One in-flight probe with the empty-token sentinel, and the screen shows
		// the connecting state — not yet the connect form.
		expect(connectTokens).toEqual([""]);
		expect(root.textContent).toContain("Connecting");
		expect(root.querySelector("input")).toBeNull();
	});

	test("an ok probe resolves the native provider (baseUrl injected, token undefined — DL-109)", async () => {
		const root = document.createElement("div");

		const booted = bootNativeClient(root, deps);
		await flush();
		settle(0, connectResult({ ok: true, kind: "" }));

		const connection = await booted;
		expect(connection).toEqual({
			baseUrl: "https://compass.example:8443",
			token: undefined,
			fetchImpl: undefined,
		});
	});

	const FAILURES: Array<{
		kind: ConnectResult["kind"];
		over: Partial<ConnectResult>;
		expect: string;
	}> = [
		{ kind: "bad-url", over: {}, expect: "Can't reach the host" },
		{
			kind: "bad-cert",
			over: {},
			expect: "Can't verify the server's certificate",
		},
		{ kind: "bad-token", over: {}, expect: "The server rejected this token" },
		{
			kind: "version-mismatch",
			over: { apiVersion: "compass.v2" },
			expect: "App speaks compass.v1; server speaks compass.v2",
		},
		{
			kind: "other",
			over: { message: "the door was bolted" },
			expect: "the door was bolted",
		},
	];

	for (const f of FAILURES) {
		test(`renders the distinct ${f.kind} state on probe failure`, async () => {
			const root = document.createElement("div");

			void bootNativeClient(root, deps);
			await flush();
			settle(0, connectResult({ ok: false, kind: f.kind, ...f.over }));
			await flush();

			expect(root.textContent).toContain(f.expect);
			// The connect form is up: a read-only URL, one input, one button.
			expect(root.textContent).toContain("https://compass.example:8443");
			expect(root.querySelectorAll("input").length).toBe(1);
			expect(root.querySelectorAll("button").length).toBe(1);
		});
	}

	test("submit is disabled on empty input and never fires the empty-token call by user action", async () => {
		const root = document.createElement("div");

		void bootNativeClient(root, deps);
		await flush();
		settle(0, connectResult({ kind: "bad-token" }));
		await flush();

		const button = root.querySelector("button") as HTMLButtonElement;
		expect(button.disabled).toBe(true);

		// A click on the disabled/empty form must not fire a second shellConnect —
		// only the boot-internal probe (index 0) has run.
		button.click();
		await flush();
		expect(connectTokens).toEqual([""]);
	});

	test("a connect-button submit clears the input and retains no token (DL-109)", async () => {
		const root = document.createElement("div");

		void bootNativeClient(root, deps);
		await flush();
		settle(0, connectResult({ kind: "bad-token" }));
		await flush();

		const input = root.querySelector("input") as HTMLInputElement;
		const button = root.querySelector("button") as HTMLButtonElement;

		input.value = "secret-token";
		input.dispatchEvent(new Event("input"));
		expect(button.disabled).toBe(false);

		button.click();
		await flush();

		// The token reached the shell exactly once, and the input is cleared —
		// nothing UI-side retains it.
		expect(connectTokens).toEqual(["", "secret-token"]);
		expect(input.value).toBe("");
	});

	test("a failed retry keeps the screen up and re-renders the new failure kind", async () => {
		const root = document.createElement("div");

		void bootNativeClient(root, deps);
		await flush();
		settle(0, connectResult({ kind: "bad-token" }));
		await flush();

		const input = root.querySelector("input") as HTMLInputElement;
		const button = root.querySelector("button") as HTMLButtonElement;
		input.value = "nope";
		input.dispatchEvent(new Event("input"));
		button.click();
		await flush();

		settle(1, connectResult({ kind: "bad-url" }));
		await flush();

		// Still on the gate, now showing the bad-url copy.
		expect(root.textContent).toContain("Can't reach the host");
		expect(root.querySelectorAll("input").length).toBe(1);
	});

	test("a successful retry resolves the native connection", async () => {
		const root = document.createElement("div");

		const booted = bootNativeClient(root, deps);
		await flush();
		settle(0, connectResult({ kind: "bad-token" }));
		await flush();

		const input = root.querySelector("input") as HTMLInputElement;
		const button = root.querySelector("button") as HTMLButtonElement;
		input.value = "good-token";
		input.dispatchEvent(new Event("input"));
		button.click();
		await flush();

		settle(1, connectResult({ ok: true, kind: "" }));
		const connection = await booted;
		expect(connection?.baseUrl).toBe("https://compass.example:8443");
		expect(connection?.token).toBeUndefined();
	});
});
