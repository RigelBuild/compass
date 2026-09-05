/// <reference types="bun" />
import { beforeEach, describe, expect, test } from "bun:test";
import { bootConnection } from "./boot";
import { type BootModeDeps, bootForMode, defaultDeps } from "./boot-mode";
import { bootNativeClient } from "./boot-native";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { envConnectionProvider } from "./live/provider";

const CONNECTION: ResolvedConnection = {
	baseUrl: "",
	token: undefined,
	fetchImpl: fetch,
};

function provider(connection: ResolvedConnection): ConnectionProvider {
	return { resolve: async () => connection };
}

describe("bootForMode", () => {
	let root: HTMLElement;
	let clientCalls: number;
	let embeddedFactoryCalls: number;
	let envFactoryCalls: number;
	let connectionBootCalls: number;
	let deps: BootModeDeps;

	beforeEach(() => {
		root = document.createElement("div");
		clientCalls = 0;
		embeddedFactoryCalls = 0;
		envFactoryCalls = 0;
		connectionBootCalls = 0;
		deps = {
			bootNativeClient: async (receivedRoot) => {
				expect(receivedRoot).toBe(root);
				clientCalls++;
				return CONNECTION;
			},
			embeddedConnectionProvider: () => {
				embeddedFactoryCalls++;
				return provider(CONNECTION);
			},
			envConnectionProvider: () => {
				envFactoryCalls++;
				return provider({ ...CONNECTION, fetchImpl: undefined });
			},
			bootConnection: async (receivedRoot, resolve) => {
				expect(receivedRoot).toBe(root);
				connectionBootCalls++;
				return resolve();
			},
		};
	});

	test("client invokes only the native client boot gate", async () => {
		const connection = await bootForMode("client", root, deps)();

		expect(connection).toBe(CONNECTION);
		expect(clientCalls).toBe(1);
		expect(embeddedFactoryCalls).toBe(0);
		expect(envFactoryCalls).toBe(0);
		expect(connectionBootCalls).toBe(0);
	});

	test("embedded dispatches through bootConnection, never the client probe", async () => {
		const connection = await bootForMode("embedded", root, deps)();

		expect(connection?.token).toBeUndefined();
		expect(connection?.fetchImpl).toBe(fetch);
		expect(embeddedFactoryCalls).toBe(1);
		expect(clientCalls).toBe(0);
		expect(envFactoryCalls).toBe(0);
		expect(connectionBootCalls).toBe(1);
	});

	test("undefined resolves the browser environment provider", async () => {
		const connection = await bootForMode(undefined, root, deps)();

		expect(connection?.fetchImpl).toBeUndefined();
		expect(envFactoryCalls).toBe(1);
		expect(embeddedFactoryCalls).toBe(0);
		expect(clientCalls).toBe(0);
		expect(connectionBootCalls).toBe(1);
	});
});

describe("defaultDeps production wiring", () => {
	test("binds the real boot functions", () => {
		expect(defaultDeps.bootNativeClient).toBe(bootNativeClient);
		expect(defaultDeps.bootConnection).toBe(bootConnection);
		expect(defaultDeps.envConnectionProvider).toBe(envConnectionProvider);
	});

	test("embedded provider is the bridge provider (fetchImpl set, no bearer), NOT the env provider", async () => {
		const resolved = await defaultDeps.embeddedConnectionProvider().resolve();
		// DL-111 ambient-admin: no bearer crosses the IPC seam.
		expect(resolved.token).toBeUndefined();
		// The IPC tunnel fetch — defined for nativeConnectionProvider, undefined
		// for envConnectionProvider; this is what discriminates the two, so a
		// revert of embedded→env would fail here.
		expect(resolved.fetchImpl).toBeDefined();
	});
});
