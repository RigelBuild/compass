/// <reference types="bun" />
// Contracts defended here (the Wails binding of the shell IPC seam,
// daemon-transport.ts):
//  - wailsShellIpc().rpc subscribes to the per-request runtime event
//    "compass_rpc:"+requestId BEFORE invoking the bound CompassRPC method, and
//    delivers each ResponseFrame to onFrame in the order the runtime emits them.
//  - it unsubscribes on the terminal frame (end / error): a frame pushed after
//    the terminal one never reaches onFrame.
//  - rpc invokes CompassRPC by name with the exact {requestId,path,headers,body}
//    args; cancel invokes CompassRPCCancel by name with the requestId.
//  - nativeConnectionProvider().resolve() yields token === undefined (DL-109:
//    the UI-side Connection never carries a bearer in client mode) and a defined
//    fetchImpl.
//  - shellConnect(token) invokes the Connect method by name with the token and
//    maps the returned ConnectResult through faithfully (ok and failure kinds).
//
// The Wails runtime is a hand-installed fake via mock.module: Events.On records
// each subscription and hands back an unsubscribe that flips a flag, and
// Call.ByName records every (method, args) and is driven by the test — exactly
// the fake-the-seam style of FakeShellIpc (daemon-transport.test.ts). No live
// Wails app, no Go process, no webview.

import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import * as realRuntime from "@wailsio/runtime";
import {
	nativeConnectionProvider,
	shellConnect,
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

/** Install a fresh fake `@wailsio/runtime` for a test. Bun's `mock.module`
 *  retroactively updates the live ESM binding, so the statically-imported
 *  binding calls the fake `Call.ByName`/`Events.On` from its next invocation.
 *  The test reads the module-level `subscriptions`/`calls`. */
function installFakeRuntime(): void {
	subscriptions = [];
	calls = [];
	mock.module("@wailsio/runtime", () => ({
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
		});
		const result = await promise;
		expect(result.ok).toBe(true);
		expect(result.kind).toBe("");
		expect(result.accountId).toBe("acc-1");
		expect(result.serverVersion).toBe("1.2.3");
		expect(result.apiVersion).toBe("compass.v1");
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
		});
		const result = await promise;
		expect(result.ok).toBe(false);
		expect(result.kind).toBe("bad-token");
		expect(result.message).toBe("the token was rejected");
	});
});
