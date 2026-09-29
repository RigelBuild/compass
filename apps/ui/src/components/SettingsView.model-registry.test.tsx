import { describe, expect, test } from "bun:test";
import {
	CompassService,
	create,
	createRouterTransport,
	GetModelRegistryResponseSchema,
	type Transport,
} from "@compass/client";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { StoreContext } from "../context";
import { type AppStoreOptions, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { SettingsView } from "./SettingsView";

type Entries = Record<
	string,
	{ displayName: string; candidates: { provider: string; modelId: string }[] }
>;

const transportFor = (entries: Entries): Transport =>
	createRouterTransport(({ service }) =>
		service(CompassService, {
			getModelRegistry: () =>
				create(GetModelRegistryResponseSchema, {
					version: 3n,
					registry: { entries },
				}),
		}),
	);

/** Mount SettingsView over a store and drain the registry round-trip. */
async function mountSettings(
	options: Omit<AppStoreOptions, "queryClient">,
): Promise<{ container: HTMLElement; unmount: () => void }> {
	const { container, unmount } = render(() => (
		<StoreContext
			value={createAppStore({ ...options, queryClient: testQueryClient() })}
		>
			<SettingsView />
		</StoreContext>
	));
	for (let i = 0; i < 200; i++) {
		await Promise.resolve();
		flush();
	}
	return { container, unmount };
}

const region = (c: HTMLElement) =>
	c.querySelector<HTMLElement>('section[aria-label="Model registry"]');

describe("SettingsView model registry", () => {
	test("renders no registry section for an offline store", async () => {
		const { container, unmount } = await mountSettings({});
		try {
			expect(region(container)).toBeNull();
		} finally {
			unmount();
		}
	});

	test("renders each stable name with its candidate chain in order", async () => {
		const { container, unmount } = await mountSettings({
			transport: transportFor({
				"claude-fast": {
					displayName: "Claude Fast",
					candidates: [
						{ provider: "anthropic", modelId: "sonnet" },
						{ provider: "openrouter", modelId: "claude" },
					],
				},
			}),
		});
		try {
			const section = region(container);
			expect(section?.textContent).toContain("v3");
			const row = section?.querySelector(".settings-registry-row");
			expect(row?.querySelector(".settings-registry-name")?.textContent).toBe(
				"claude-fast",
			);
			expect(
				row?.querySelector(".settings-registry-display")?.textContent,
			).toBe("Claude Fast");
			expect(
				row?.querySelector(".settings-registry-candidates")?.textContent,
			).toBe("anthropic/sonnet→openrouter/claude");
		} finally {
			unmount();
		}
	});

	test("renders the empty state for an unconfigured registry", async () => {
		const { container, unmount } = await mountSettings({
			transport: transportFor({}),
		});
		try {
			expect(region(container)?.textContent).toContain(
				"No model registry configured.",
			);
			expect(container.querySelector(".settings-registry")).toBeNull();
		} finally {
			unmount();
		}
	});
});
