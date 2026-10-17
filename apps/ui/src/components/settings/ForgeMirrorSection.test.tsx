import { describe, expect, test } from "bun:test";
import { ForgeMirrorMode, ForgeProvider } from "@compass/client";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { StoreContext } from "../../context";
import { createFakeCompass } from "../../live/compass-fake";
import { type AppStoreOptions, createAppStore } from "../../store";
import { testQueryClient } from "../../test-support";
import { ForgeMirrorSection } from "./ForgeMirrorSection";

const setting = {
	forge: { provider: ForgeProvider.GITHUB, host: "github.com" },
	repo: "owner/repo",
	mode: ForgeMirrorMode.MANUAL as const,
};

async function mountSettings(
	options: Omit<AppStoreOptions, "queryClient">,
): Promise<{ container: HTMLElement; unmount: () => void }> {
	const { container, unmount } = render(() => (
		<StoreContext
			value={createAppStore({ ...options, queryClient: testQueryClient() })}
		>
			<ForgeMirrorSection />
		</StoreContext>
	));
	for (let i = 0; i < 40; i++) {
		await Promise.resolve();
		flush();
	}
	return { container, unmount };
}

const mirrorSelect = (container: HTMLElement) =>
	container.querySelector<HTMLSelectElement>(
		'select[aria-label="GitHub · github.com/owner/repo"]',
	);

async function settle(): Promise<void> {
	for (let i = 0; i < 20; i++) {
		await Promise.resolve();
		flush();
	}
}

describe("ForgeMirrorSection", () => {
	test("an admin can change a coordinate's mode", async () => {
		const fake = createFakeCompass({
			mirrorSettings: [setting],
			canEditMirrorSettings: true,
		});
		const { container, unmount } = await mountSettings({
			compass: fake.client,
		});
		try {
			const select = mirrorSelect(container);
			if (!select) throw new Error("mirror setting select did not render");
			fireEvent.change(select, {
				target: { value: String(ForgeMirrorMode.AUTO) },
			});
			await settle();

			expect(fake.mirrorSettingWrites).toEqual([
				{ ...setting, mode: ForgeMirrorMode.AUTO },
			]);
		} finally {
			unmount();
		}
	});

	test("a non-admin cannot change a coordinate's mode", async () => {
		const fake = createFakeCompass({
			mirrorSettings: [setting],
			canEditMirrorSettings: false,
		});
		const { container, unmount } = await mountSettings({
			compass: fake.client,
		});
		try {
			const select = mirrorSelect(container);
			expect(select?.disabled).toBe(true);
			expect(fake.mirrorSettingWrites).toEqual([]);
		} finally {
			unmount();
		}
	});

	test("a refused change keeps its prior mode and shows the error", async () => {
		const fake = createFakeCompass({
			mirrorSettings: [setting],
			canEditMirrorSettings: true,
		});
		fake.failNextMirrorSetting(new Error("setting denied"));
		const { container, unmount } = await mountSettings({
			compass: fake.client,
		});
		try {
			const select = mirrorSelect(container);
			if (!select) throw new Error("mirror setting select did not render");
			fireEvent.change(select, {
				target: { value: String(ForgeMirrorMode.AUTO) },
			});
			await settle();

			expect(select.value).toBe(String(ForgeMirrorMode.MANUAL));
			expect(container.querySelector('[role="alert"]')?.textContent).toBe(
				"setting denied",
			);
		} finally {
			unmount();
		}
	});
});
