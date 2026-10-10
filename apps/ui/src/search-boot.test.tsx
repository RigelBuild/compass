import { afterEach, expect, test } from "bun:test";
import {
	CommsService,
	create,
	createCommsClient,
	createCompassClient,
	createRouterTransport,
	MessageBlockSchema,
	MessageSchema,
} from "@compass/client";
import { cleanup, fireEvent, waitFor } from "@solidjs/testing-library";
import { createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import {
	resetSearchDebounceForTest,
	setSearchDebounceMsForTest,
} from "./keyboard/destination-surface";
import type { LiveClients } from "./live/client";
import { mountShell } from "./mount";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

// Every hop index → mount → App → surface takes `clients` as an optional prop,
// so only a mounted shell catches a dropped hop.

function liveClients(
	onCall: () => void,
): Pick<LiveClients, "comms" | "compass"> {
	const transport = createRouterTransport(({ service }) => {
		service(CommsService, {
			searchMessages: () => {
				onCall();
				return {
					messages: [
						create(MessageSchema, {
							id: "msg-boot",
							topicId: "top-ann-posture",
							blocks: [
								create(MessageBlockSchema, {
									block: { case: "text", value: "Boot search hit" },
								}),
							],
						}),
					],
				};
			},
		});
	});
	return {
		comms: createCommsClient(transport),
		compass: createCompassClient(transport),
	};
}

function withShell(
	clients: Pick<LiveClients, "comms" | "compass"> | undefined,
	body: (store: AppStore, root: HTMLElement) => Promise<void>,
): Promise<void> {
	const queryClient = testQueryClient();
	let disposeStore!: () => void;
	const store = createRoot((d) => {
		disposeStore = d;
		return createAppStore({ initialComms: STUB_COMMS_STATE, queryClient });
	});
	const root = document.createElement("div");
	document.body.append(root);
	const unmount = mountShell(root, store, queryClient, clients);
	return body(store, root).finally(() => {
		unmount();
		root.remove();
		disposeStore();
	});
}

function groupLabels(root: HTMLElement, selector: string): string[] {
	return [...root.querySelectorAll(selector)].map((el) => el.textContent ?? "");
}

// "sett" matches the local Settings view, so the Views group marks a settled query.
async function search(
	root: HTMLElement,
	input: HTMLInputElement,
	groupSelector: string,
): Promise<string[]> {
	input.focus();
	input.value = "sett";
	fireEvent.input(input);
	await waitFor(() =>
		expect(groupLabels(root, groupSelector)).toContain("Views"),
	);
	return groupLabels(root, groupSelector);
}

afterEach(() => {
	cleanup();
	resetSearchDebounceForTest();
	window.location.hash = "";
});

test("a booted shell routes message search to the topbar and the palette", async () => {
	setSearchDebounceMsForTest(0);
	let calls = 0;
	await withShell(
		liveClients(() => calls++),
		async (store, root) => {
			const topbar = root.querySelector<HTMLInputElement>(
				".topbar-search .cx-search",
			);
			if (!topbar) throw new Error("topbar search input missing");
			await search(root, topbar, ".topbar-search-group");
			await waitFor(() =>
				expect(groupLabels(root, ".topbar-search-group")).toContain("Messages"),
			);
			expect(root.textContent).toContain("Boot search hit");
			const afterTopbar = calls;
			expect(afterTopbar).toBeGreaterThan(0);

			store.openPalette();
			const palette = await waitFor(() => {
				const el = root.querySelector<HTMLInputElement>(
					'.cx-palette input[placeholder^="Search commands"]',
				);
				if (!el) throw new Error("palette input missing");
				return el;
			});
			await search(root, palette, ".cx-palette-group");
			await waitFor(() =>
				expect(groupLabels(root, ".cx-palette-group")).toContain("Messages"),
			);
			expect(calls).toBeGreaterThan(afterTopbar);
		},
	);
});

test("a shell booted without clients shows no message group and makes no RPC", async () => {
	setSearchDebounceMsForTest(0);
	await withShell(undefined, async (store, root) => {
		const topbar = root.querySelector<HTMLInputElement>(
			".topbar-search .cx-search",
		);
		if (!topbar) throw new Error("topbar search input missing");
		expect(await search(root, topbar, ".topbar-search-group")).not.toContain(
			"Messages",
		);
		store.openPalette();
		const palette = await waitFor(() => {
			const el = root.querySelector<HTMLInputElement>(
				'.cx-palette input[placeholder^="Search commands"]',
			);
			if (!el) throw new Error("palette input missing");
			return el;
		});
		expect(await search(root, palette, ".cx-palette-group")).not.toContain(
			"Messages",
		);
	});
});
