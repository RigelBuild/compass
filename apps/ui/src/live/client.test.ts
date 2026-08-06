import { afterEach, describe, expect, type Mock, spyOn, test } from "bun:test";
import type { CompassClient } from "@compass/client";
import * as compassClient from "@compass/client";
import { createLiveClients, resolveCaller } from "./client";
import type { Connection } from "./connection";

// createLiveClients must build ONE gRPC-Web transport and dial both clients over
// it — the invariant the query layer rests on: connect-query-core embeds a
// Transport reference in every query key, so cache identity requires the clients
// and the query layer to share the single instance (query record §A2/§T1). A
// `Client` hides its transport, so the sharing is proven at the seam: the two
// client factories are spied and the transport each was handed is compared to
// the one createLiveClients exposes. `spyOn` (not `mock.module`) keeps the real
// module intact — a whole-module mock leaks process-wide into sibling suites.

describe("createLiveClients (query record T1)", () => {
	const conn: Connection = {
		baseUrl: "https://compass.example:8443",
		token: "tok",
	};

	const spies: Mock<(...args: never[]) => unknown>[] = [];
	afterEach(() => {
		// Restore the real factories so no spy leaks into another suite.
		for (const s of spies.splice(0)) s.mockRestore();
	});

	test("dials both clients over one shared transport instance", () => {
		const commsSpy = spyOn(compassClient, "createCommsClient");
		const compassSpy = spyOn(compassClient, "createCompassClient");
		spies.push(commsSpy, compassSpy);

		const clients = createLiveClients(conn);

		// Exactly one transport was built, and it is the very object both clients
		// were constructed over and the one exposed on LiveClients — the identity
		// the query layer's keys depend on.
		expect(commsSpy).toHaveBeenCalledTimes(1);
		expect(compassSpy).toHaveBeenCalledTimes(1);
		const commsTransport = commsSpy.mock.calls[0]?.[0];
		const compassTransport = compassSpy.mock.calls[0]?.[0];
		expect(commsTransport).toBe(clients.transport);
		expect(compassTransport).toBe(clients.transport);
		expect(commsTransport).toBe(compassTransport);
	});
});

describe("resolveCaller (WhoAmI boot probe)", () => {
	test("returns the accountId the server reports for the caller", async () => {
		// A fake whose whoAmI resolves a known account id — resolveCaller must
		// hand back exactly that string (the caller learned from the server via
		// WhoAmI at boot).
		const client = {
			whoAmI: async (_req: Record<string, never>) => ({ accountId: "acc-x" }),
		} as unknown as CompassClient;

		expect(await resolveCaller(client)).toBe("acc-x");
	});

	// resolveCaller must reject a blank identity: a server that answers WhoAmI
	// with no account id (no-auth door, unauthenticated bearer) has to throw, not
	// return "", so an unknown "me" never silently scopes the store to an empty
	// caller.
	for (const [label, value] of [
		["empty", ""],
		["whitespace-only", "  \t"],
	] as const) {
		test(`throws when the server returns a ${label} account id`, async () => {
			const client = {
				whoAmI: async (_req: Record<string, never>) => ({ accountId: value }),
			} as unknown as CompassClient;

			await expect(resolveCaller(client)).rejects.toThrow(/empty account id/);
		});
	}
});
