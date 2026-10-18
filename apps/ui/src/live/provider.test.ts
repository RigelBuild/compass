import { afterEach, describe, expect, type Mock, spyOn, test } from "bun:test";
import * as compassClient from "@compass/client";
import { createLiveClients } from "./client";
import { envConnectionProvider, type ResolvedConnection } from "./provider";

// The provider passes fetchImpl to the shared client transport.

// Restore env after each test to avoid leaking config.
type MutableEnv = Record<string, string | undefined>;

describe("envConnectionProvider (browser dev path — env unchanged)", () => {
	const env = import.meta.env as MutableEnv;
	let priorBaseUrl: string | undefined;

	afterEach(() => {
		// Delete absent env vars because Bun coerces assigned values to strings.
		if (priorBaseUrl === undefined) {
			delete env.VITE_COMPASS_BASE_URL;
		} else {
			env.VITE_COMPASS_BASE_URL = priorBaseUrl;
		}
	});

	test("resolves the env connection with fetchImpl undefined (platform fetch)", async () => {
		priorBaseUrl = env.VITE_COMPASS_BASE_URL;
		env.VITE_COMPASS_BASE_URL = "https://compass.example:8443";

		const resolved = await envConnectionProvider().resolve();

		// The explicit undefined fetchImpl selects platform fetch.
		expect(resolved.baseUrl).toBe("https://compass.example:8443");
		expect(resolved.fetchImpl).toBeUndefined();
	});

	test("preserves the env-required-var throw (missing baseUrl)", async () => {
		priorBaseUrl = env.VITE_COMPASS_BASE_URL;
		// Delete rather than assign undefined to simulate a missing required value.
		delete env.VITE_COMPASS_BASE_URL;

		// A missing door URL still fails at the env resolver boundary.
		await expect(envConnectionProvider().resolve()).rejects.toThrow(
			/VITE_COMPASS_BASE_URL/,
		);
	});
});

describe("createLiveClients threads a provider's fetchImpl (native shell seam)", () => {
	const spies: Mock<(...args: never[]) => unknown>[] = [];
	afterEach(() => {
		for (const s of spies.splice(0)) s.mockRestore();
	});

	test("a resolved fetchImpl is passed into the one shared transport", () => {
		// Inspect the fake fetch passed to the single shared transport.
		const transportSpy = spyOn(compassClient, "createCompassWebTransport");
		spies.push(transportSpy);

		const fetchImpl = (async () => new Response()) as unknown as typeof fetch;
		const conn: ResolvedConnection = {
			baseUrl: "https://compass.example:8443",
			token: "tok",
			fetchImpl,
		};

		createLiveClients(conn);

		expect(transportSpy).toHaveBeenCalledTimes(1);
		const [baseUrl, token, opts] = transportSpy.mock.calls[0] ?? [];
		expect(baseUrl).toBe("https://compass.example:8443");
		expect(token).toBe("tok");
		// The seam: the provider's fetch is the transport's fetch.
		expect((opts as { fetch?: typeof fetch })?.fetch).toBe(fetchImpl);
	});
});
