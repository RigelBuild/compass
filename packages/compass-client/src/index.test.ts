import { expect, test } from "bun:test";
import { createGrpcWebTransport } from "@connectrpc/connect-web";
import { type CompassClient, createCompassClient } from "./index";

test("createCompassClient wires the generated CompassService onto a transport", () => {
	const transport = createGrpcWebTransport({ baseUrl: "http://localhost" });
	const client: CompassClient = createCompassClient(transport);
	expect(typeof client.getDaemonInfo).toBe("function");
});
