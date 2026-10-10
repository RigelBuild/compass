import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { STUB_AGENTS } from "../stub-data";
import { flush, mountApp } from "../test-router";

afterEach(cleanup);

const card = (container: HTMLElement, handle: string): HTMLElement => {
	const found = [
		...container.querySelectorAll<HTMLElement>(".agents-view .agent-card"),
	].find((c) => c.querySelector(".ac-name")?.textContent === handle);
	if (!found) throw new Error(`no agent card for ${handle}`);
	return found;
};

// The spine sits in the child's row, so it describes the edge into that child.
const spineInto = (container: HTMLElement, handle: string): HTMLElement => {
	const spine = card(container, handle)
		.closest(".tree-row")
		?.querySelector<HTMLElement>(".tree-spine");
	if (!spine) throw new Error(`no spine into ${handle}`);
	return spine;
};

const handleOf = (id: string): string => {
	const agent = STUB_AGENTS.find((a) => a.account.id === id);
	if (!agent) throw new Error(`fixture missing ${id}`);
	return agent.account.handle;
};

describe("AgentsView", () => {
	test("clicking a card opens that agent", async () => {
		const { store, container } = mountApp("/agents");
		await flush();
		fireEvent.click(card(container, handleOf("acc-compass-ui")));
		await flush();
		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-compass-ui");
	});

	test("Enter on a focused card opens that agent", async () => {
		const { store, container } = mountApp("/agents");
		await flush();
		const target = card(container, handleOf("acc-fleet"));
		target.focus();
		fireEvent.keyDown(target, { key: "Enter" });
		await flush();
		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-fleet");
	});

	test("only a spine into a working child carries the flow pip", async () => {
		const { container } = mountApp("/agents");
		await flush();
		// acc-compass-server is working; acc-compass-native is idle.
		expect(
			spineInto(container, handleOf("acc-compass-server")).dataset.flow,
		).toBe("1");
		expect(
			spineInto(container, handleOf("acc-compass-native")).dataset.flow,
		).toBeUndefined();
	});
});
