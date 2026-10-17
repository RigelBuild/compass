import { ForgeMirrorMode, ForgeProvider } from "@compass/client";
import {
	type Component,
	createMemo,
	createSignal,
	createUniqueId,
	For,
	Show,
} from "solid-js";
import { useStore } from "../../context";
import type { ForgeMirrorModeValue } from "../../store";
import { SettingsRow } from "./SettingsRow";

const providerLabel: Record<ForgeProvider, string> = {
	[ForgeProvider.UNSPECIFIED]: "Unknown",
	[ForgeProvider.GITHUB]: "GitHub",
	[ForgeProvider.GITLAB]: "GitLab",
	[ForgeProvider.FORGEJO]: "Forgejo",
	[ForgeProvider.LINEAR]: "Linear",
};

export const ForgeMirrorSection: Component = () => {
	const store = useStore();
	const ready = createMemo(() => {
		const state = store.forgeMirrorSettings();
		return state.status === "ready" ? state : undefined;
	});
	const failed = createMemo(() => {
		const state = store.forgeMirrorSettings();
		return state.status === "error" ? state : undefined;
	});
	const uid = createUniqueId();
	const [errors, setErrors] = createSignal<Record<string, string>>({});
	const [pending, setPending] = createSignal<ReadonlySet<string>>(new Set());
	const keyOf = (setting: { provider: number; host: string; repo: string }) =>
		`${setting.provider}:${setting.host}:${setting.repo}`;
	const modeFor = (key: string, fallback: ForgeMirrorModeValue) => {
		const current = store.forgeMirrorSettings();
		return current.status === "ready"
			? (current.settings.find((setting) => keyOf(setting) === key)?.mode ??
					fallback)
			: fallback;
	};

	const changeMode = async (
		setting: {
			provider: number;
			host: string;
			repo: string;
			mode: ForgeMirrorModeValue;
		},
		mode: ForgeMirrorModeValue,
	) => {
		const key = keyOf(setting);
		if (pending().has(key)) return;
		setErrors((previous) => {
			const next = { ...previous };
			delete next[key];
			return next;
		});
		setPending((previous) => new Set(previous).add(key));
		try {
			await store.setForgeMirrorSetting({
				provider: setting.provider,
				host: setting.host,
				repo: setting.repo,
				mode,
			});
		} catch (error) {
			setErrors((previous) => ({
				...previous,
				[key]: error instanceof Error ? error.message : String(error),
			}));
		} finally {
			setPending((previous) => {
				const next = new Set(previous);
				next.delete(key);
				return next;
			});
		}
	};

	return (
		<section
			class="settings-content"
			aria-labelledby={`${uid}-forge-mirror-title`}
		>
			<header class="settings-content-head">
				<h2 class="settings-heading" id={`${uid}-forge-mirror-title`}>
					Issue mirroring
				</h2>
				<p class="settings-description">
					Choose how each enabled issue tracker receives asks and topics.
				</p>
			</header>
			<Show when={store.forgeMirrorSettings().status !== "offline"}>
				<section class="settings-group" aria-label="Issue mirror settings">
					<Show when={store.forgeMirrorSettings().status === "pending"}>
						<span class="settings-group-help">Loading…</span>
					</Show>
					<Show when={failed()}>
						{(state) => <span role="alert">{state().message}</span>}
					</Show>
					<Show when={ready()}>
						{(state) => (
							<Show
								when={state().settings.length > 0}
								fallback={
									<p class="settings-empty">No enabled forge repositories.</p>
								}
							>
								<For each={state().settings}>
									{(setting) => {
										const key = keyOf(setting);
										const id = `forge-mirror-${key}`;
										return (
											<SettingsRow
												label={`${providerLabel[setting.provider] ?? "Forge"} · ${setting.host}/${setting.repo}`}
												for={id}
											>
												<select
													id={id}
													class="cx-select"
													aria-label={`${providerLabel[setting.provider] ?? "Forge"} · ${setting.host}/${setting.repo}`}
													value={String(modeFor(key, setting.mode))}
													disabled={!state().canEdit || pending().has(key)}
													onChange={(event) => {
														const mode = Number(event.currentTarget.value);
														if (
															mode === ForgeMirrorMode.OFF ||
															mode === ForgeMirrorMode.MANUAL ||
															mode === ForgeMirrorMode.AUTO
														) {
															void changeMode(setting, mode);
														}
													}}
												>
													<option value={ForgeMirrorMode.OFF}>Off</option>
													<option value={ForgeMirrorMode.MANUAL}>Manual</option>
													<option value={ForgeMirrorMode.AUTO}>Auto</option>
												</select>
												<Show when={errors()[key]}>
													{(message) => <span role="alert">{message()}</span>}
												</Show>
											</SettingsRow>
										);
									}}
								</For>
							</Show>
						)}
					</Show>
				</section>
			</Show>
		</section>
	);
};
