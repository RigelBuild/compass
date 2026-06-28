import { expect, test } from "bun:test";
import { type CompassClient, createCompassWebClient } from "./index";

test("createCompassWebClient exposes the compass.v1 surface over gRPC-Web", () => {
	const client: CompassClient = createCompassWebClient("http://localhost");
	expect(typeof client.getDaemonInfo).toBe("function");
	expect(typeof client.subscribeEvents).toBe("function");
});
