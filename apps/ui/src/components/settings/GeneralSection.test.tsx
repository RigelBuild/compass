import { afterEach, describe, expect, test } from "bun:test";
import {
	CompassService,
	create,
	createCompassClient,
	createRouterTransport,
	GetServerInfoResponseSchema,
	SubscribeEventsResponseSchema,
} from "@compass/client";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { mountApp } from "../../test-router";
import { modeLabel } from "./GeneralSection";

function serverTransport(rev = "abc123") {
	return createRouterTransport(({ service }) => {
		service(CompassService, {
			getServerInfo: () =>
				create(GetServerInfoResponseSchema, {
					version: "1.2.3",
					apiVersion: "compass.v1",
					rev,
				}),
			listBoardIssues: async () => ({ issues: [] }),
			subscribeEvents: async function* (_request, context) {
				yield create(SubscribeEventsResponseSchema, {
					seq: 0n,
					instanceEpoch: 1n,
					snapshotSeq: 0n,
					payload: { case: "serverStatus", value: {} },
				});
				const { promise, resolve } = Promise.withResolvers<void>();
				context.signal.addEventListener("abort", () => resolve(), {
					once: true,
				});
				await promise;
			},
		});
	});
}

async function waitForProbe(isLive: () => boolean): Promise<void> {
	for (let i = 0; i < 200 && !isLive(); i++) {
		await Promise.resolve();
		flush();
	}
}

function settingsRow(
	container: HTMLElement,
	label: string,
): HTMLElement | undefined {
	return [...container.querySelectorAll<HTMLElement>(".settings-row")].find(
		(row) =>
			row
				.querySelector(".settings-row-label > :first-child")
				?.textContent?.trim() === label,
	);
}

afterEach(cleanup);
describe("GeneralSection", () => {
	test("shows connection, server, and caller details from the live store", async () => {
		const transport = serverTransport();
		const { store, container } = mountApp("/settings/general", {
			compass: createCompassClient(transport),
			serverUrl: "https://compass.example",
		});
		await waitForProbe(() => store.daemon().live);

		expect(settingsRow(container, "Server URL")?.textContent).toContain(
			"https://compass.example",
		);
		expect(settingsRow(container, "Server version")?.textContent).toContain(
			"1.2.3",
		);
		expect(settingsRow(container, "Server version")?.textContent).toContain(
			"compass.v1",
		);
		expect(settingsRow(container, "Server version")?.textContent).toContain(
			"abc123",
		);
		expect(settingsRow(container, "Signed in as")?.textContent).toContain(
			"matt",
		);
		expect(settingsRow(container, "Signed in as")?.textContent).toContain(
			"Matt",
		);
	});

	test("an unstamped build shows no empty revision", async () => {
		const { store, container } = mountApp("/settings/general", {
			compass: createCompassClient(serverTransport("")),
		});
		await waitForProbe(() => store.daemon().live);
		expect(
			settingsRow(container, "Server version")
				?.querySelector(".settings-row-control")
				?.textContent?.trim(),
		).toBe("1.2.3 · compass.v1");
	});

	test("read-only rows name their value without an orphan label", () => {
		const { container } = mountApp("/settings/general");
		expect(
			container.querySelectorAll(".settings-view label:not([for])"),
		).toHaveLength(0);
		expect(settingsRow(container, "Server version")?.textContent).toContain(
			"Server version",
		);
	});

	test("reports offline server info as not connected", () => {
		const { container } = mountApp("/settings/general");
		expect(settingsRow(container, "Server URL")?.textContent).toContain(
			"Not connected",
		);
		expect(settingsRow(container, "Server version")?.textContent).toContain(
			"Not connected",
		);
	});

	test("opens the shortcuts overlay from its row button", async () => {
		const { store, container } = mountApp("/settings/general");
		const button = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Open keyboard shortcuts"]',
		);
		if (!button) throw new Error("missing Keyboard shortcuts button");

		fireEvent.click(button);
		await flush();

		expect(store.shortcutsOpen()).toBe(true);
		expect(
			container.querySelector(
				'[role="dialog"][aria-label="Keyboard shortcuts"]',
			),
		).not.toBeNull();
	});

	test.each([
		[undefined, "Browser"],
		["embedded", "Embedded server"],
		["client", "Remote server"],
		["setup", "Remote server"],
		["reopen", "Remote server"],
	] as const)("labels shell mode %s", (mode, expected) => {
		expect(modeLabel(mode)).toBe(expected);
	});
});
