/// <reference types="bun" />
// Contracts defended here (the Wails binding of the shell IPC seam,
// daemon-transport.ts):
// - rpc subscribes before calling the shell, preserves frame order, and removes
//   its listener after a terminal frame.
// - Invalid runtime frames reach the transport as an error before unsubscription.
// - Shell calls keep the established binding names and request arguments.
// - Client provider connections never carry a bearer token (DL-109).
// - shellConnect maps current Go results, including those without serverUrl.
//
// The Wails runtime is a hand-installed fake via mock.module. Events.On records
// subscriptions and returns an unsubscribe function for each event.
// Call.ByName records invocations that each test settles.
import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import * as realRuntime from "@wailsio/runtime";
import {
	chooseEmbedded,
	createDaemonFetch,
	nativeConnectionProvider,
	onSetupDecided,
	pickCACert,
	quitApp,
	shellConnect,
	shellState,
	wailsShellIpc,
} from "./daemon-transport";

/** One captured `Events.On(name, cb)` subscription. `off` records whether the
 *  binding has torn it down. */
type Subscription = {
	name: string;
	cb: (event: { name: string; data: unknown }) => void;
	off: boolean;
};

/** One captured `Call.ByName(method, ...args)` invocation, with the resolvers
 *  the test drives to settle the returned promise. */
type Invocation = {
	method: string;
	args: unknown[];
	resolve: (value: unknown) => void;
	reject: (err: unknown) => void;
};

let subscriptions: Subscription[];
let calls: Invocation[];
let quitCalls: number;

/** Install a fresh fake `@wailsio/runtime` for a test. Bun's `mock.module`
 *  retroactively updates the live ESM binding, so the statically-imported
 *  binding calls the fake `Call.ByName`/`Events.On` from its next invocation.
 *  The test reads the module-level `subscriptions`/`calls`. */
function installFakeRuntime(): void {
	subscriptions = [];
	calls = [];
	quitCalls = 0;
	mock.module("@wailsio/runtime", () => ({
		Application: {
			Quit() {
				quitCalls++;
				return Promise.resolve();
			},
		},
		Events: {
			On(name: string, cb: (event: { name: string; data: unknown }) => void) {
				const sub: Subscription = { name, cb, off: false };
				subscriptions.push(sub);
				return () => {
					sub.off = true;
				};
			},
		},
		Call: {
			ByName(method: string, ...args: unknown[]) {
				let resolve!: (value: unknown) => void;
				let reject!: (err: unknown) => void;
				const promise = new Promise<unknown>((res, rej) => {
					resolve = res;
					reject = rej;
				});
				calls.push({ method, args, resolve, reject });
				return promise;
			},
		},
	}));
}

beforeEach(() => {
	installFakeRuntime();
});

// mock.module leaks across FILES (bun runs one process), so the fake runtime
// must be torn down or a sibling suite importing @wailsio/runtime inherits it.
afterEach(() => {
	mock.module("@wailsio/runtime", () => realRuntime);
});

/** Emit a runtime event for the given name to every LIVE subscription (as the
 *  Go shell's per-frame Emit would), carrying the ResponseFrame as `data`. A
 *  subscription the binding has unsubscribed (`off`) no longer receives events —
 *  faithful to the real runtime, and what makes the unsubscribe-on-terminal
 *  tests non-vacuous: a binding that failed to tear down would still be `off:
 *  false` here and receive the stray frame. */
function emit(name: string, data: unknown): void {
	for (const sub of subscriptions) {
		if (sub.name === name && !sub.off) sub.cb({ name, data });
	}
}

async function flushMicrotasks(): Promise<void> {
	for (let i = 0; i < 8; i++) await Promise.resolve();
}
describe("wailsShellIpc", () => {
	const rpcArgs = {
		requestId: "req-1",
		path: "/compass.v1.CompassService/SubscribeEvents",
		headers: [{ name: "content-type", value: "application/grpc-web+proto" }],
		body: [1, 2, 3],
	};

	test("subscribes to compass_rpc:<id> before invoking CompassRPC, then delivers frames in order", async () => {
		const ipc = wailsShellIpc();
		const seen: string[] = [];
		void ipc.rpc(rpcArgs, (frame) => seen.push(frame.kind));

		// Subscription is installed under the per-request event name, and the bound
		// method was invoked BY NAME with the exact args.
		expect(subscriptions.map((s) => s.name)).toEqual(["compass_rpc:req-1"]);
		expect(calls).toHaveLength(1);
		expect(calls[0].method).toBe("main.bridgeService.CompassRPC");
		expect(calls[0].args).toEqual([rpcArgs]);

		// Frames emitted on the event arrive at onFrame in emission order.
		emit("compass_rpc:req-1", { kind: "head", status: 200, headers: [] });
		emit("compass_rpc:req-1", { kind: "body", chunk: "AAEC" });
		emit("compass_rpc:req-1", { kind: "end" });
		expect(seen).toEqual(["head", "body", "end"]);
	});
	test("accepts a head frame with omitted headers", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		emit(sub.name, { kind: "end" });

		expect((await fetched).status).toBe(200);
		expect(sub.off).toBe(true);
	});
	test("accepts a body frame with omitted chunk as empty bytes", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "body" });
		emit(sub.name, { kind: "end" });

		expect((await response.arrayBuffer()).byteLength).toBe(0);
		expect(sub.off).toBe(true);
	});

	test("invalid head frames reject fetch and unsubscribe", async () => {
		for (const status of [700, 200.5]) {
			installFakeRuntime();
			const fetched = createDaemonFetch(wailsShellIpc())(
				"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
			);
			await flushMicrotasks();
			const sub = subscriptions[0];
			if (!sub) throw new Error("response subscription is missing");
			emit(sub.name, { kind: "head", status, headers: [] });

			await expect(fetched).rejects.toThrow(
				"Invalid response frame from shell",
			);
			expect(sub.off).toBe(true);
		}
	});

	test("a malformed head rejects fetch and unsubscribes", async () => {
		const ipc = wailsShellIpc();
		const fetched = createDaemonFetch(ipc)(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head" });

		await expect(fetched).rejects.toThrow("Invalid response frame from shell");
		expect(sub.off).toBe(true);
	});
	test("a body frame with a non-string chunk rejects the active response", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "body", chunk: 42 });

		await expect(response.arrayBuffer()).rejects.toThrow(
			"Invalid response frame from shell",
		);
		expect(sub.off).toBe(true);
	});
	test("a malformed terminal frame rejects the active response and unsubscribes", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "terminal" });

		await expect(response.arrayBuffer()).rejects.toThrow(
			"Invalid response frame from shell",
		);
		expect(sub.off).toBe(true);
	});
	test("an error frame with omitted message uses the default failure text", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "error" });

		await expect(response.arrayBuffer()).rejects.toThrow("Shell RPC failed");
		expect(sub.off).toBe(true);
	});
	test("an error frame with a non-string message is rejected", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "error", message: 42 });

		await expect(response.arrayBuffer()).rejects.toThrow(
			"Invalid response frame from shell",
		);
		expect(sub.off).toBe(true);
	});
	test("error frames with explicit messages fail the active response", async () => {
		const fetched = createDaemonFetch(wailsShellIpc())(
			"https://daemon.invalid/compass.v1.CompassService/GetDaemonInfo",
		);
		await flushMicrotasks();
		const sub = subscriptions[0];
		if (!sub) throw new Error("response subscription is missing");
		emit(sub.name, { kind: "head", status: 200 });
		const response = await fetched;
		emit(sub.name, { kind: "error", message: "daemon error" });

		await expect(response.arrayBuffer()).rejects.toThrow("daemon error");
		expect(sub.off).toBe(true);
	});

	test("unsubscribes on the terminal end frame — a later frame never reaches onFrame", async () => {
		const ipc = wailsShellIpc();
		const seen: string[] = [];
		void ipc.rpc(rpcArgs, (frame) => seen.push(frame.kind));

		emit("compass_rpc:req-1", { kind: "head", status: 200, headers: [] });
		emit("compass_rpc:req-1", { kind: "end" });
		expect(subscriptions[0].off).toBe(true);

		// A stray frame after the terminal one is dropped (the listener is gone).
		emit("compass_rpc:req-1", { kind: "body", chunk: "AAEC" });
		expect(seen).toEqual(["head", "end"]);
	});

	test("unsubscribes on the terminal error frame", async () => {
		const ipc = wailsShellIpc();
		const seen: string[] = [];
		void ipc.rpc(rpcArgs, (frame) => seen.push(frame.kind));

		emit("compass_rpc:req-1", { kind: "error", message: "boom" });
		expect(subscriptions[0].off).toBe(true);
		emit("compass_rpc:req-1", { kind: "body", chunk: "AAEC" });
		expect(seen).toEqual(["error"]);
	});

	test("cancel invokes CompassRPCCancel by name with the requestId", async () => {
		const ipc = wailsShellIpc();
		ipc.cancel("req-42");
		expect(calls).toHaveLength(1);
		expect(calls[0].method).toBe("main.bridgeService.CompassRPCCancel");
		expect(calls[0].args).toEqual([{ requestId: "req-42" }]);
	});

	test("a CompassRPC invoke rejection tears down the subscription and rejects rpc", async () => {
		const ipc = wailsShellIpc();
		const promise = ipc.rpc(rpcArgs, () => {});
		calls[0].reject(new Error("invoke failed"));
		await expect(promise).rejects.toThrow("invoke failed");
		expect(subscriptions[0].off).toBe(true);
	});
});

describe("nativeConnectionProvider", () => {
	test("resolve() yields token === undefined (DL-109) and a defined fetchImpl", async () => {
		const resolved = await nativeConnectionProvider(
			"https://compass.example:8443",
		).resolve();
		expect(resolved.baseUrl).toBe("https://compass.example:8443");
		// DL-109: the UI-side Connection NEVER carries a bearer in client mode.
		expect(resolved.token).toBeUndefined();
		expect(typeof resolved.fetchImpl).toBe("function");
	});
});

describe("shellConnect", () => {
	test("invokes Connect by name with the token and maps an ok result through", async () => {
		const promise = shellConnect("tok-abc");
		expect(calls).toHaveLength(1);
		expect(calls[0].method).toBe("main.bridgeService.Connect");
		expect(calls[0].args).toEqual([{ token: "tok-abc" }]);

		calls[0].resolve({
			ok: true,
			kind: "",
			message: "",
			accountId: "acc-1",
			serverVersion: "1.2.3",
			apiVersion: "compass.v1",
			serverUrl: "https://compass.example",
		});
		const result = await promise;
		expect(result.ok).toBe(true);
		expect(result.kind).toBe("");
		expect(result.accountId).toBe("acc-1");
		expect(result.serverVersion).toBe("1.2.3");
		expect(result.apiVersion).toBe("compass.v1");
	});

	test("accepts the current Go ConnectResult without serverUrl", async () => {
		const promise = shellConnect("");
		calls[0]?.resolve({
			ok: true,
			kind: "",
			message: "",
			accountId: "acc-1",
			serverVersion: "1.2.3",
			apiVersion: "compass.v1",
		});
		const result = await promise;
		expect(result.ok).toBe(true);
		expect(result.serverUrl).toBeUndefined();
	});

	test("maps a failure-kind result through faithfully", async () => {
		const promise = shellConnect("bad");
		calls[0].resolve({
			ok: false,
			kind: "bad-token",
			message: "the token was rejected",
			accountId: "",
			serverVersion: "",
			apiVersion: "",
			serverUrl: "",
		});
		const result = await promise;
		expect(result.ok).toBe(false);
		expect(result.kind).toBe("bad-token");
		expect(result.message).toBe("the token was rejected");
	});
});
describe("setup bindings", () => {
	test("send server choice and call the exact setup methods", async () => {
		const connect = shellConnect("first-token", {
			url: "https://host",
			caRef: "ca-ref",
		});
		expect(calls[0]?.method).toBe("main.bridgeService.Connect");
		expect(calls[0]?.args).toEqual([
			{
				token: "first-token",
				server: { url: "https://host", caRef: "ca-ref" },
			},
		]);
		calls[0]?.resolve({
			ok: true,
			kind: "",
			message: "",
			accountId: "",
			serverVersion: "",
			apiVersion: "",
			serverUrl: "https://host",
		});
		expect(await connect).toMatchObject({
			ok: true,
			serverUrl: "https://host",
		});

		const picked = pickCACert();
		expect(calls[1]?.method).toBe("main.dialogService.PickCACert");
		calls[1]?.resolve({ ref: "opaque-ref", name: "root.pem" });
		expect(await picked).toEqual({ ref: "opaque-ref", name: "root.pem" });

		const embedded = chooseEmbedded();
		expect(calls[2]?.method).toBe("main.setupService.ChooseEmbedded");
		calls[2]?.resolve({ ok: false, message: "not ready" });
		expect(await embedded).toEqual({ ok: false, message: "not ready" });

		const state = shellState();
		expect(calls[3]?.method).toBe("main.bridgeService.ShellState");
		calls[3]?.resolve({ mode: "setup", serverUrl: "" });
		expect(await state).toEqual({ mode: "setup", serverUrl: "" });

		const off = onSetupDecided(() => {});
		expect(subscriptions[0]?.name).toBe("setup:decided");
		off();
		expect(subscriptions[0]?.off).toBe(true);
		await quitApp();
		expect(quitCalls).toBe(1);
	});

	test("send only token for a configured connect", async () => {
		const connect = shellConnect("configured-token");
		expect(calls[0]?.args).toEqual([{ token: "configured-token" }]);
		calls[0]?.resolve({
			ok: false,
			kind: "invalid-ca",
			message: "bad CA",
			accountId: "",
			serverVersion: "",
			apiVersion: "",
			serverUrl: "",
		});
		expect((await connect).kind).toBe("invalid-ca");
	});
});
