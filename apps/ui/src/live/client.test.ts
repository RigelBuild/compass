import { afterEach, describe, expect, type Mock, spyOn, test } from "bun:test";
import * as compassClient from "@compass/client";
import { createLiveClients } from "./client";
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
		callerId: "acc-me",
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
