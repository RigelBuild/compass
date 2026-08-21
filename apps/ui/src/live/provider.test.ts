import { afterEach, describe, expect, type Mock, spyOn, test } from "bun:test";
import * as compassClient from "@compass/client";
import { createLiveClients } from "./client";
import { envConnectionProvider, type ResolvedConnection } from "./provider";

// The provider seam (design §A1, §Plan T1): boot resolves a Connection through a
// ConnectionProvider, and the ONLY mode difference above the transport boundary
// is the fetch that provider carries. Two things are proven here:
//   - the default env provider wraps connectionFromEnv() unchanged and leaves
//     fetchImpl undefined (the browser dev path uses the platform fetch);
//   - a provider that DOES carry a fetchImpl threads it into the single shared
//     transport createLiveClients builds — the shell's injected transport.

// import.meta.env is the process-wide Vite env; the env provider reads it via
// connectionFromEnv(). Set the required var for the resolve, then restore the
// prior value so no key leaks into a sibling suite.
type MutableEnv = Record<string, string | undefined>;

describe("envConnectionProvider (browser dev path — env unchanged)", () => {
	const env = import.meta.env as MutableEnv;
	let priorBaseUrl: string | undefined;

	afterEach(() => {
		// Restore by DELETE when the var was originally absent: under Bun ≥1.4
		// `import.meta.env` coerces an assigned value to a string, so writing an
		// `undefined` priorBaseUrl back would leave the literal string
		// "undefined" and leak into sibling suites. Assigning a real prior value
		// is fine; only the absent case needs the delete.
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

		// A Connection (baseUrl from the env resolver) plus an explicit undefined
		// fetchImpl — the seam's signal that boot should use the platform fetch.
		expect(resolved.baseUrl).toBe("https://compass.example:8443");
		expect(resolved.fetchImpl).toBeUndefined();
	});

	test("preserves the env-required-var throw (missing baseUrl)", async () => {
		priorBaseUrl = env.VITE_COMPASS_BASE_URL;
		// DELETE (not assign undefined): under Bun ≥1.4 `import.meta.env` coerces
		// an assigned value to a string, so `= undefined` would leave the literal
		// "undefined" — a truthy door URL that resolves instead of throwing.
		// Deleting the key is the faithful "var absent" simulation and matches
		// 1.3 behavior too.
		delete env.VITE_COMPASS_BASE_URL;

		// Env parity with today: a missing door URL still throws by design, so
		// boot's bootConnection catches it at the same boundary as before.
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
		// The transport factory is spied so we can read exactly what boot handed
		// it: a fake shell fetch, threaded straight through to the single
		// transport both clients dial over.
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
