/// <reference types="bun" />
import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { bootNativeClient, type NativeBootDeps } from "./boot-native";
import { bootSetup, type SetupBootDeps } from "./boot-setup";
import type {
	ConnectResult,
	SetupResult,
	ShellState,
} from "./daemon-transport";
import type { ResolvedConnection } from "./live/provider";

const CONNECTION: ResolvedConnection = {
	baseUrl: "https://compass.example",
	token: undefined,
	fetchImpl: undefined,
};

let root: HTMLElement;
let state: {
	mode: "setup" | "client" | "reopen" | "embedded";
	serverUrl: string;
};
let stateCalls: number;
let chooseResult: Promise<SetupResult>;
let setupDecided: (() => void) | undefined;
let unsubscribeCalls: number;
let nativeCalls: Array<{
	root: HTMLElement;
	entry: "setup" | "configured";
	signal: AbortSignal | undefined;
}>;
let nativeResolvers: Array<(result: ResolvedConnection | undefined) => void>;
let deps: SetupBootDeps;

beforeEach(() => {
	root = document.createElement("div");
	state = { mode: "setup", serverUrl: "" };
	stateCalls = 0;
	chooseResult = Promise.resolve({ ok: false, message: "preflight failed" });
	setupDecided = undefined;
	unsubscribeCalls = 0;
	nativeCalls = [];
	nativeResolvers = [];
	deps = {
		chooseEmbedded: () => chooseResult,
		shellState: async () => {
			expect(setupDecided).toBeDefined();
			stateCalls++;
			return state;
		},
		onSetupDecided: (fn) => {
			setupDecided = fn;
			return () => {
				unsubscribeCalls++;
			};
		},
		quitApp: async () => {},
		bootNativeClient: (receivedRoot, entry, signal) => {
			nativeCalls.push({ root: receivedRoot, entry, signal });
			const { promise, resolve } = Promise.withResolvers<
				ResolvedConnection | undefined
			>();
			nativeResolvers.push(resolve);
			return promise;
		},
	};
});

afterEach(() => {
	window.__COMPASS_SERVER_URL__ = undefined;
});

async function flush(): Promise<void> {
	for (let i = 0; i < 8; i++) await Promise.resolve();
}

function button(label: string): HTMLButtonElement {
	const found = [...root.querySelectorAll("button")].find(
		(item) => item.textContent === label,
	);
	if (!(found instanceof HTMLButtonElement))
		throw new Error(`missing button ${label}`);
	return found;
}

function emitDecision(): void {
	if (!setupDecided) throw new Error("setup:decided listener is not installed");
	setupDecided();
}

function connectDeps(
	pending: Array<(result: ConnectResult) => void>,
): NativeBootDeps {
	return {
		shellConnect: () => {
			const { promise, resolve } = Promise.withResolvers<ConnectResult>();
			pending.push(resolve);
			return promise;
		},
		pickCACert: async () => ({ ref: "", name: "" }),
		nativeConnectionProvider: (baseUrl) => ({
			resolve: async () => ({
				baseUrl,
				token: undefined,
				fetchImpl: undefined,
			}),
		}),
	};
}

function connectResult(over: Partial<ConnectResult>): ConnectResult {
	return {
		ok: false,
		kind: "other",
		message: "",
		accountId: "",
		serverVersion: "",
		apiVersion: "",
		serverUrl: "https://compass.example",
		...over,
	};
}

describe("bootSetup", () => {
	test("an embedded failure returns to both choices with its message", async () => {
		const { promise, resolve } = Promise.withResolvers<SetupResult>();
		chooseResult = promise;
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		expect(root.textContent).toContain(
			"Checking this computer… The first check on a Mac can take several minutes.",
		);
		resolve({ ok: false, message: "preflight could not start" });
		await flush();

		expect(root.textContent).toContain("preflight could not start");
		expect(root.textContent).toContain("Connect to a server");
		expect(root.textContent).toContain("Run Compass on this computer");
		expect(unsubscribeCalls).toBe(0);
	});

	test("embedded success is terminal and Quit calls quitApp", async () => {
		let quitCalls = 0;
		deps.quitApp = async () => {
			quitCalls++;
		};
		chooseResult = Promise.resolve({
			ok: true,
			message:
				"Compass is set up to run on this computer. Quit and reopen it to start.",
		});
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		await flush();

		expect(root.textContent).toContain(
			"Compass is set up to run on this computer. Quit and reopen it to start.",
		);
		button("Quit").click();
		await flush();
		expect(quitCalls).toBe(1);
		expect(unsubscribeCalls).toBe(1);
	});

	test("setup decisions show reopen or boot the configured client once", async () => {
		void bootSetup(root, deps);
		await flush();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		await flush();
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(nativeCalls).toHaveLength(0);
		expect(unsubscribeCalls).toBe(1);

		root = document.createElement("div");
		state = { mode: "setup", serverUrl: "" };
		stateCalls = 0;
		unsubscribeCalls = 0;
		nativeCalls = [];
		nativeResolvers = [];
		void bootSetup(root, deps);
		await flush();
		state = { mode: "client", serverUrl: "https://compass.example" };
		emitDecision();
		await flush();
		expect(nativeCalls.map((call) => call.entry)).toEqual(["configured"]);
		expect(nativeCalls[0]?.root).toBe(root);
		nativeResolvers[0]?.(CONNECTION);
		await flush();
		expect(unsubscribeCalls).toBe(1);
	});

	test("a sibling client decision passes its server URL to configured boot", async () => {
		const nativeDeps: NativeBootDeps = {
			...connectDeps([]),
			shellConnect: async () => connectResult({ ok: true, kind: "" }),
		};
		const setupDeps: SetupBootDeps = {
			...deps,
			bootNativeClient: (receivedRoot, entry, signal) =>
				bootNativeClient(receivedRoot, nativeDeps, entry, signal),
		};
		const booted = bootSetup(root, setupDeps);
		await flush();
		state = { mode: "client", serverUrl: "https://sibling.example" };
		emitDecision();
		expect((await booted)?.baseUrl).toBe("https://sibling.example");
	});

	test("a rejected shell-state call renders recoverable setup choices", async () => {
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			if (calls === 1) throw new Error("state unavailable");
			return { mode: "setup", serverUrl: "" };
		};
		void bootSetup(root, deps);
		await flush();
		expect(root.textContent).toContain("state unavailable");
		button("Connect to a server").click();
		await flush();
		expect(nativeCalls.map((call) => call.entry)).toEqual(["setup"]);
	});
	test("a decision during a state read that then fails is retried, keeping the error", async () => {
		const first = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = () => {
			calls++;
			if (calls === 1) return Promise.resolve({ mode: "setup", serverUrl: "" });
			if (calls === 2) return first.promise;
			return Promise.resolve({ mode: "reopen", serverUrl: "" });
		};
		const booted = bootSetup(root, deps);
		await flush();
		emitDecision();
		await flush();
		emitDecision();
		first.reject(new Error("state unavailable"));
		await flush();
		expect(calls).toBe(3);
		expect(await booted).toBeUndefined();
		expect(root.textContent).toContain("already set up");
	});
	test("a decision queued by a stale state read aborts the idle connect form", async () => {
		const read = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = () => {
			calls++;
			if (calls === 2) return read.promise;
			return Promise.resolve({ mode: "setup", serverUrl: "" });
		};
		void bootSetup(root, deps);
		await flush();
		emitDecision();
		await flush();
		button("Connect to a server").click();
		await flush();
		const signal = nativeCalls[0]?.signal;
		expect(signal?.aborted).toBe(false);
		read.resolve({ mode: "setup", serverUrl: "" });
		await flush();
		expect(signal?.aborted).toBe(true);
	});
	test("a rejected embedded choice restores both choices", async () => {
		deps.chooseEmbedded = async () => {
			throw new Error("preflight unavailable");
		};
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		await flush();
		expect(root.textContent).toContain("preflight unavailable");
		button("Connect to a server").click();
		await flush();
		expect(nativeCalls.map((call) => call.entry)).toEqual(["setup"]);
	});
	test("a rejected embedded choice rereads a queued sibling decision", async () => {
		const choose = Promise.withResolvers<SetupResult>();
		deps.chooseEmbedded = () => choose.promise;
		const booted = bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		choose.reject(new Error("preflight unavailable"));
		await flush();

		expect(stateCalls).toBe(2);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(root.textContent).not.toContain("preflight unavailable");
		expect(unsubscribeCalls).toBe(1);
		expect(await booted).toBeUndefined();
	});

	test("a queued sibling event does not overwrite an embedded failure", async () => {
		const read = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			return calls === 2 ? read.promise : state;
		};
		const choose = Promise.withResolvers<SetupResult>();
		deps.chooseEmbedded = () => choose.promise;
		void bootSetup(root, deps);
		await flush();
		emitDecision();
		await flush();
		button("Run Compass on this computer").click();
		read.resolve({ mode: "setup", serverUrl: "" });
		await flush();
		choose.reject(new Error("preflight failed after sibling event"));
		await flush();
		expect(calls).toBe(3);
		expect(root.textContent).toContain("preflight failed after sibling event");
		expect(root.textContent).toContain("Run Compass on this computer");
		expect(root.textContent).toContain("Connect to a server");
	});
	test("a non-terminal embedded failure rereads a queued sibling decision", async () => {
		const choose = Promise.withResolvers<SetupResult>();
		chooseResult = choose.promise;
		const booted = bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		emitDecision();
		state = { mode: "reopen", serverUrl: "" };
		choose.resolve({ ok: false, message: "preflight failed" });
		await flush();

		expect(stateCalls).toBe(2);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(root.textContent).not.toContain("preflight failed");
		expect(await booted).toBeUndefined();
		expect(unsubscribeCalls).toBe(1);
	});
	test("a deferred embedded failure stays visible after shellState returns setup", async () => {
		const pendingRead = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			return calls === 2 ? pendingRead.promise : state;
		};
		const pendingChoice = Promise.withResolvers<SetupResult>();
		deps.chooseEmbedded = () => pendingChoice.promise;
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		emitDecision();
		await flush();
		pendingChoice.resolve({ ok: false, message: "embedded retry message" });
		await flush();
		pendingRead.resolve({ mode: "setup", serverUrl: "" });
		await flush();

		expect(root.textContent).toContain("embedded retry message");
		expect(root.textContent).toContain("Connect to a server");
	});

	test("a state read completing during an embedded attempt defers until failure", async () => {
		const read = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			return calls === 2 ? read.promise : state;
		};
		const choose = Promise.withResolvers<SetupResult>();
		deps.chooseEmbedded = () => choose.promise;
		void bootSetup(root, deps);
		await flush();
		emitDecision();
		await flush();
		button("Run Compass on this computer").click();
		read.resolve({ mode: "setup", serverUrl: "" });
		await flush();

		expect(root.textContent).toContain("Checking this computer");
		expect(calls).toBe(2);
		choose.resolve({ ok: false, message: "preflight failed" });
		await flush();
		expect(calls).toBe(3);
		expect(root.textContent).toContain("preflight failed");
		expect(root.textContent).toContain("Connect to a server");
	});

	test("a state read completing during connect aborts and rereads terminal state", async () => {
		const read = Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			return calls === 2 ? read.promise : state;
		};
		const booted = bootSetup(root, deps);
		await flush();
		emitDecision();
		await flush();
		button("Connect to a server").click();
		state = { mode: "reopen", serverUrl: "" };
		read.resolve(state);
		await flush();

		const signal = nativeCalls[0]?.signal;
		expect(signal?.aborted).toBe(true);
		nativeResolvers[0]?.(undefined);
		await flush();
		expect(calls).toBe(3);
		expect(await booted).toBeUndefined();
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
	});

	test("a decision made before subscription is found by the first shellState", async () => {
		state = { mode: "reopen", serverUrl: "" };
		const booted = bootSetup(root, deps);
		await flush();

		expect(stateCalls).toBe(1);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(nativeCalls).toHaveLength(0);
		expect(await booted).toBeUndefined();
		expect(unsubscribeCalls).toBe(1);
	});

	test("the window that chose embedded keeps its success screen when its event arrives", async () => {
		const { promise, resolve } = Promise.withResolvers<SetupResult>();
		chooseResult = promise;
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		expect(stateCalls).toBe(1);
		resolve({
			ok: true,
			message:
				"Compass is set up to run on this computer. Quit and reopen it to start.",
		});
		await flush();

		expect(root.textContent).toContain(
			"Compass is set up to run on this computer. Quit and reopen it to start.",
		);
		expect(root.textContent).not.toContain("Compass is already set up.");
		expect(stateCalls).toBe(1);
		expect(unsubscribeCalls).toBe(1);
	});

	test("failed and refused attempts keep listening for a sibling decision", async () => {
		const results = [
			{ ok: false, message: "preflight failed" },
			{ ok: false, message: "Another window is setting up Compass." },
		];
		let index = 0;
		deps.chooseEmbedded = async () => {
			const result = results[index];
			if (!result) throw new Error("no configured result");
			index++;
			return result;
		};
		void bootSetup(root, deps);
		await flush();
		button("Run Compass on this computer").click();
		await flush();
		expect(root.textContent).toContain("preflight failed");
		expect(unsubscribeCalls).toBe(0);
		button("Run Compass on this computer").click();
		await flush();
		expect(root.textContent).toContain("Another window is setting up Compass.");
		expect(unsubscribeCalls).toBe(0);

		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		await flush();
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(stateCalls).toBe(2);
		expect(unsubscribeCalls).toBe(1);
	});

	test("a sibling decision during an idle setup form aborts and applies new state", async () => {
		const pending: Array<(result: ConnectResult) => void> = [];
		const nativeDeps = connectDeps(pending);
		const setupDeps: SetupBootDeps = {
			...deps,
			bootNativeClient: (receivedRoot, entry, signal) =>
				bootNativeClient(receivedRoot, nativeDeps, entry, signal),
		};
		void bootSetup(root, setupDeps);
		await flush();
		button("Connect to a server").click();
		await flush();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		expect(root.childElementCount).toBe(0);
		await flush();

		expect(stateCalls).toBe(2);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
	});
	test("a failed state read during an active connect preserves feedback and queued decisions", async () => {
		const { promise: read, reject: rejectRead } =
			Promise.withResolvers<ShellState>();
		let calls = 0;
		deps.shellState = async () => {
			calls++;
			if (calls === 1) return { mode: "setup", serverUrl: "" };
			if (calls === 2) return read;
			return { mode: "reopen", serverUrl: "" };
		};
		const pending: Array<(result: ConnectResult) => void> = [];
		const nativeDeps = connectDeps(pending);
		const setupDeps: SetupBootDeps = {
			...deps,
			bootNativeClient: (receivedRoot, entry, signal) =>
				bootNativeClient(receivedRoot, nativeDeps, entry, signal),
		};
		const booted = bootSetup(root, setupDeps);
		await flush();
		expect(root.textContent).toContain("Connect to a server");
		button("Connect to a server").click();
		await flush();
		const fields = root.querySelectorAll("input");
		const url = fields.item(0);
		const token = fields.item(1);
		if (
			!(url instanceof HTMLInputElement) ||
			!(token instanceof HTMLInputElement)
		)
			throw new Error("missing setup fields");
		url.value = "https://sibling.example";
		token.value = "token";
		token.dispatchEvent(new Event("input"));
		button("Connect").click();
		await flush();
		emitDecision();
		const settle = pending[0];
		if (!settle) throw new Error("no in-flight shellConnect");
		rejectRead(new Error("state read failed during connect"));
		await flush();
		emitDecision();
		settle(connectResult({ kind: "invalid-url", message: "bad URL" }));
		await flush();

		expect(root.textContent).toContain("state read failed during connect");
		emitDecision();
		await flush();
		expect(calls).toBe(3);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
		expect(await booted).toBeUndefined();
	});

	test("a non-ok in-flight connect returns undefined before the state reread", async () => {
		const pending: Array<(result: ConnectResult) => void> = [];
		const nativeDeps = connectDeps(pending);
		const setupDeps: SetupBootDeps = {
			...deps,
			bootNativeClient: (receivedRoot, entry, signal) =>
				bootNativeClient(receivedRoot, nativeDeps, entry, signal),
		};
		void bootSetup(root, setupDeps);
		await flush();
		button("Connect to a server").click();
		await flush();
		const fields = root.querySelectorAll("input");
		const url = fields.item(0);
		const token = fields.item(1);
		if (
			!(url instanceof HTMLInputElement) ||
			!(token instanceof HTMLInputElement)
		)
			throw new Error("missing setup fields");
		url.value = "https://new.example";
		token.value = "token";
		token.dispatchEvent(new Event("input"));
		button("Connect").click();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		expect(stateCalls).toBe(1);
		expect(root.textContent).toContain("Connect to a server");
		const settle = pending[0];
		if (!settle) throw new Error("no in-flight shellConnect");
		settle(connectResult({ kind: "invalid-url", message: "bad URL" }));
		await flush();
		expect(stateCalls).toBe(2);
		expect(root.textContent).toContain(
			"Compass is already set up. Quit and reopen it to change this.",
		);
	});

	test("an ok in-flight connect wins over an abort and does not read sibling state", async () => {
		const pending: Array<(result: ConnectResult) => void> = [];
		const nativeDeps = connectDeps(pending);
		const setupDeps: SetupBootDeps = {
			...deps,
			bootNativeClient: (receivedRoot, entry, signal) =>
				bootNativeClient(receivedRoot, nativeDeps, entry, signal),
		};
		const booted = bootSetup(root, setupDeps);
		await flush();
		button("Connect to a server").click();
		await flush();
		const fields = root.querySelectorAll("input");
		const url = fields.item(0);
		const token = fields.item(1);
		if (
			!(url instanceof HTMLInputElement) ||
			!(token instanceof HTMLInputElement)
		)
			throw new Error("missing setup fields");
		url.value = "https://new.example";
		token.value = "token";
		token.dispatchEvent(new Event("input"));
		button("Connect").click();
		state = { mode: "reopen", serverUrl: "" };
		emitDecision();
		const settle = pending[0];
		if (!settle) throw new Error("no in-flight shellConnect");
		settle(
			connectResult({ ok: true, kind: "", serverUrl: "https://new.example" }),
		);
		expect(await booted).toEqual({
			baseUrl: "https://new.example",
			token: undefined,
			fetchImpl: undefined,
		});
		expect(stateCalls).toBe(1);
		expect(root.textContent).not.toContain("Compass is already set up.");
	});

	test("connect hands off setup entry with a live abort signal", async () => {
		const booted = bootSetup(root, deps);
		await flush();
		button("Connect to a server").click();
		await flush();
		expect(nativeCalls).toHaveLength(1);
		expect(nativeCalls[0]?.entry).toBe("setup");
		expect(nativeCalls[0]?.root).toBe(root);
		expect(nativeCalls[0]?.signal?.aborted).toBe(false);
		nativeResolvers[0]?.(CONNECTION);
		expect(await booted).toBe(CONNECTION);
		expect(unsubscribeCalls).toBe(1);
	});
});
