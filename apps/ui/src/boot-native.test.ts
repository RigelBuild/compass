/// <reference types="bun" />
import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { bootNativeClient, type NativeBootDeps } from "./boot-native";
import type { ConnectResult, PickedCA, ServerChoice } from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";

// The native client boot gate consumes injected shell transport dependencies.
// Tests drive calls directly without Wails, IPC, or a live network.
//
// The transport records server choices and captures each pending shell call.

// The transport records the optional first-run server choice per call.
let connectTokens: string[];
let serverChoices: Array<ServerChoice | undefined>;
let pending: Array<(result: ConnectResult) => void>;
let pickedCA: PickedCA;
let deps: NativeBootDeps;

function connectResult(over: Partial<ConnectResult>): ConnectResult {
	return {
		ok: false,
		kind: "other",
		message: "",
		accountId: "",
		serverVersion: "",
		apiVersion: "",
		serverUrl: "https://compass.example:8443",
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
	serverChoices = [];
	pending = [];
	pickedCA = { ref: "", name: "" };
	deps = {
		shellConnect: (
			token: string,
			server?: ServerChoice,
		): Promise<ConnectResult> => {
			connectTokens.push(token);
			serverChoices.push(server);
			const { promise, resolve } = Promise.withResolvers<ConnectResult>();
			pending.push(resolve);
			return promise;
		},
		pickCACert: async (): Promise<PickedCA> => pickedCA,
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
test("setup entry shows an editable URL without probing and sends an empty CA ref", async () => {
	const root = document.createElement("div");
	const booted = bootNativeClient(root, deps, "setup");
	await flush();

	expect(connectTokens).toEqual([]);
	expect(root.textContent).toContain("Connect to a server");
	const fields = root.querySelectorAll("input");
	expect(fields.length).toBe(2);
	const url = fields.item(0);
	const token = fields.item(1);
	if (
		!(url instanceof HTMLInputElement) ||
		!(token instanceof HTMLInputElement)
	) {
		throw new Error("setup form inputs are missing");
	}
	url.value = "https://new.example:9443";
	url.dispatchEvent(new Event("input"));
	token.value = "first-token";
	token.dispatchEvent(new Event("input"));
	const button = [...root.querySelectorAll("button")].find(
		(candidate) => candidate.textContent === "Connect",
	);
	if (!(button instanceof HTMLButtonElement))
		throw new Error("connect button is missing");
	button.click();
	await flush();

	expect(connectTokens).toEqual(["first-token"]);
	expect(serverChoices).toEqual([
		{ url: "https://new.example:9443", caRef: "" },
	]);
	settle(
		0,
		connectResult({
			ok: true,
			kind: "",
			serverUrl: "https://new.example:9443",
		}),
	);
	expect((await booted)?.baseUrl).toBe("https://new.example:9443");
});

test("a picked CA name is shown and Use system trust clears its ref", async () => {
	pickedCA = { ref: "opaque-ca-ref", name: "private-root.pem" };
	const root = document.createElement("div");
	void bootNativeClient(root, deps, "setup");
	await flush();
	const buttons = root.querySelectorAll("button");
	const choose = buttons.item(0);
	if (!(choose instanceof HTMLButtonElement))
		throw new Error("CA button is missing");
	choose.click();
	await flush();
	expect(root.textContent).toContain("private-root.pem");

	const trustButton = [...root.querySelectorAll("button")].find(
		(button) => button.textContent === "Use system trust",
	);
	if (!(trustButton instanceof HTMLButtonElement))
		throw new Error("trust button is missing");
	const fields = root.querySelectorAll("input");
	const url = fields.item(0);
	const token = fields.item(1);
	if (
		!(url instanceof HTMLInputElement) ||
		!(token instanceof HTMLInputElement)
	)
		throw new Error("setup form inputs are missing");
	url.value = "https://new.example";
	token.value = "token";
	token.dispatchEvent(new Event("input"));
	const connect = [...root.querySelectorAll("button")].find(
		(button) => button.textContent === "Connect",
	);
	if (!(connect instanceof HTMLButtonElement))
		throw new Error("connect button is missing");
	connect.click();
	await flush();
	expect(serverChoices).toEqual([
		{ url: "https://new.example", caRef: "opaque-ca-ref" },
	]);
	settle(0, connectResult({ kind: "bad-token" }));
	await flush();
	trustButton.click();
	expect(root.textContent).toContain("System trust");
	url.value = "https://new.example";
	url.dispatchEvent(new Event("input"));
	token.value = "token-again";
	token.dispatchEvent(new Event("input"));
	connect.click();
	await flush();
	expect(serverChoices).toEqual([
		{ url: "https://new.example", caRef: "opaque-ca-ref" },
		{ url: "https://new.example", caRef: "" },
	]);
	settle(1, connectResult({ kind: "bad-token" }));
	await flush();
});
test("invalid URL messages are rendered literally and a success uses serverUrl", async () => {
	const root = document.createElement("div");
	const booted = bootNativeClient(root, deps, "setup");
	await flush();
	const fields = root.querySelectorAll("input");
	const url = fields.item(0);
	const token = fields.item(1);
	if (
		!(url instanceof HTMLInputElement) ||
		!(token instanceof HTMLInputElement)
	) {
		throw new Error("setup form inputs are missing");
	}
	url.value = "not-validated-in-ui";
	token.value = "token";
	token.dispatchEvent(new Event("input"));
	const connect = [...root.querySelectorAll("button")].find(
		(button) => button.textContent === "Connect",
	);
	if (!(connect instanceof HTMLButtonElement))
		throw new Error("connect button is missing");
	connect.click();
	await flush();
	settle(0, connectResult({ kind: "invalid-url", message: "<b>bad URL</b>" }));
	await flush();
	expect(root.querySelector("b")).toBeNull();
	expect(root.textContent).toContain("<b>bad URL</b>");

	token.value = "good-token";
	token.dispatchEvent(new Event("input"));
	connect.click();
	await flush();
	settle(
		1,
		connectResult({
			ok: true,
			kind: "",
			serverUrl: "https://server-returned.example",
		}),
	);
	expect((await booted)?.baseUrl).toBe("https://server-returned.example");
});

test("an idle setup form resolves undefined and clears when its signal aborts", async () => {
	const root = document.createElement("div");
	const controller = new AbortController();
	const booted = bootNativeClient(root, deps, "setup", controller.signal);
	await flush();
	expect(root.textContent).toContain("Connect to a server");
	controller.abort();
	expect(await booted).toBeUndefined();
	expect(root.childElementCount).toBe(0);
});
