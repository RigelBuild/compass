import { expect, test } from "bun:test";
import { createDaemonClient } from "./client";

test("createDaemonClient wires the compass.v1 service onto a gRPC-Web transport", () => {
	const client = createDaemonClient("http://localhost");
	expect(typeof client.getDaemonInfo).toBe("function");
});
