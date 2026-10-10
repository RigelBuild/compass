import {
	type Component,
	createMemo,
	createSignal,
	createStore,
	createUniqueId,
	For,
	reconcile,
	snapshot,
} from "solid-js";
import { useStore } from "../../context";
import type {
	TrackerConfig,
	TrackerKind,
	WorkingIssueState,
} from "../../stub-data";
import { SettingsRow } from "./SettingsRow";

/** The tracker kinds selectable in the identity picker. Linear is the live one;
 *  Jira/GitHub are wired shapes but not yet backed by a real seam. */
const TRACKER_KINDS: readonly { value: TrackerKind; label: string }[] = [
	{ value: "linear", label: "Linear" },
	{ value: "jira", label: "Jira" },
	{ value: "github", label: "GitHub" },
];

/** Compass working states in lifecycle order, with human labels — one
 *  status-mapping row each. Keyed on `WorkingIssueState` (the seven working
 *  states) so a new working state can't ship without a mapping row here;
 *  `archived` carries no tracker status (DL-071) and is excluded. */
const STATE_ROWS: readonly { state: WorkingIssueState; label: string }[] = [
	{ state: "backlog", label: "Backlog" },
	{ state: "todo", label: "Todo" },
	{ state: "queued", label: "Queued" },
	{ state: "blocked", label: "Blocked" },
	{ state: "in_progress", label: "In progress" },
	{ state: "in_review", label: "In review" },
	{ state: "done", label: "Done" },
];

/** Merge the edited forward map back into the committed reverse map
 *  (`fromTracker`), preserving it as the source of truth for the reverse
 *  direction. The reverse map is NOT reconstructible from `toTracker` alone: it
 *  carries tracker-native aliases no forward row produces (e.g. Linear's
 *  `Cancelled`/`Duplicate` → `done`), and when several Compass states share a
 *  status the canonical read-back is a deliberate pick, not last-write-wins. So
 *  we keep every existing entry and only *add* a status the forward map now
 *  targets but the reverse map doesn't yet cover — first writer in lifecycle
 *  order wins (e.g. `Todo` reads back as `todo`, not the later `queued`). */
export function mergeFromTracker(
	toTracker: Record<WorkingIssueState, string>,
	committed: Record<string, WorkingIssueState>,
): Record<string, WorkingIssueState> {
	const fromTracker: Record<string, WorkingIssueState> = { ...committed };
	for (const { state } of STATE_ROWS) {
		const status = toTracker[state];
		if (status && !(status in fromTracker)) fromTracker[status] = state;
	}
	return fromTracker;
}

/** Group the true reverse map (`fromTracker`) by the Compass state each tracker
 *  status reads back to, in lifecycle order — so the read-only view shows the
 *  real many-to-one projection, aliases included (e.g. `Done, Cancelled,
 *  Duplicate → Done`), matching exactly what Save persists. */
function reverseGroups(
	fromTracker: Record<string, WorkingIssueState>,
): { label: string; statuses: string[] }[] {
	const byState = new Map<WorkingIssueState, string[]>();
	for (const [status, state] of Object.entries(fromTracker)) {
		const statuses = byState.get(state);
		if (statuses) statuses.push(status);
		else byState.set(state, [status]);
	}
	return STATE_ROWS.filter(({ state }) => byState.has(state)).map(
		({ state, label }) => ({ label, statuses: byState.get(state) ?? [] }),
	);
}

/** Tracker settings: the tracker-config and status-mapping editor. Edits land
 *  in a local draft and commit through `store.setTrackerConfig` only on Save. */
export const TrackerSection: Component = () => {
	const store = useStore();
	// Two Settings views can be mounted at once, so ids are per instance.
	const uid = createUniqueId();
	const seed = (): TrackerConfig =>
		structuredClone(snapshot(store.trackerConfig()));
	const [draft, setDraft] = createStore<TrackerConfig>(seed());
	// Bump on every draft mutation so the Save-enabled derivation re-runs; the
	// store's fine-grained reads don't otherwise notify a whole-object compare.
	const [dirtyTick, setDirtyTick] = createSignal(0);
	const touch = () => setDirtyTick((n) => n + 1);

	const reset = () => {
		setDraft(reconcile(seed()));
		touch();
	};

	const save = () => {
		const cfg: TrackerConfig = {
			kind: draft.kind,
			handle: draft.handle,
			mapping: {
				kind: draft.kind,
				toTracker: { ...draft.mapping.toTracker },
				fromTracker: mergeFromTracker(
					draft.mapping.toTracker,
					draft.mapping.fromTracker,
				),
			},
		};
		store.setTrackerConfig(cfg);
		touch();
	};

	const dirty = () => {
		dirtyTick();
		const live = store.trackerConfig();
		if (draft.kind !== live.kind || draft.handle !== live.handle) return true;
		return STATE_ROWS.some(
			({ state }) =>
				draft.mapping.toTracker[state] !== live.mapping.toTracker[state],
		);
	};

	// A blank mapping input would leave a state → "" gap in the forward map, so
	// Save is blocked; Reset stays live to recover.
	const hasEmptyMapping = () =>
		STATE_ROWS.some(({ state }) => !draft.mapping.toTracker[state]);

	// Preview the reverse map the way Save persists it; memoized so read-back
	// rows stay stable across unrelated keystrokes.
	const reverse = createMemo(() =>
		reverseGroups(
			mergeFromTracker(draft.mapping.toTracker, draft.mapping.fromTracker),
		),
	);

	return (
		<section class="settings-content" aria-labelledby={`${uid}-tracker-title`}>
			<header class="settings-content-head">
				<h2 class="settings-heading" id={`${uid}-tracker-title`}>
					Tracker
				</h2>
				<p class="settings-description">
					Configure how Compass reads and writes tracker status.
				</p>
				<p class="settings-current sub">
					{store.trackerConfig().handle} · {store.trackerConfig().kind}
				</p>
			</header>

			<section class="settings-group" aria-labelledby={`${uid}-identity-title`}>
				<h3 class="settings-group-title" id={`${uid}-identity-title`}>
					Identity
				</h3>
				<SettingsRow
					label="Handle"
					help="Who Compass lists assigned issues for."
					for={`${uid}-tracker-handle`}
				>
					<input
						id={`${uid}-tracker-handle`}
						class="cx-input"
						aria-describedby={`${uid}-tracker-handle-description`}
						value={draft.handle}
						placeholder="you@org"
						onInput={(event) => {
							const value = event.currentTarget.value;
							setDraft((state) => {
								state.handle = value;
							});
							touch();
						}}
					/>
				</SettingsRow>
				<SettingsRow label="Kind" for={`${uid}-tracker-kind`}>
					<select
						id={`${uid}-tracker-kind`}
						class="cx-select"
						value={draft.kind}
						onChange={(event) => {
							const kind = event.currentTarget.value;
							const tracker = TRACKER_KINDS.find((item) => item.value === kind);
							if (!tracker) return;
							setDraft((state) => {
								state.kind = tracker.value;
							});
							touch();
						}}
					>
						<For each={TRACKER_KINDS}>
							{(tracker) => (
								<option value={tracker.value}>{tracker.label}</option>
							)}
						</For>
					</select>
				</SettingsRow>
			</section>

			<section class="settings-group" aria-labelledby={`${uid}-mapping-title`}>
				<h3 class="settings-group-title" id={`${uid}-mapping-title`}>
					Status mapping
				</h3>
				<p class="settings-group-help">Compass state → tracker status.</p>
				<ul class="settings-map">
					<For each={STATE_ROWS}>
						{(row) => {
							const id = `${uid}-mapping-${row.state}`;
							return (
								<li>
									<SettingsRow
										label={row.label}
										help={`Tracker status for ${row.label}.`}
										for={id}
									>
										<input
											id={id}
											class="cx-input"
											aria-describedby={`${id}-description`}
											value={draft.mapping.toTracker[row.state]}
											placeholder="tracker status"
											onInput={(event) => {
												const value = event.currentTarget.value;
												setDraft((state) => {
													state.mapping.toTracker[row.state] = value;
												});
												touch();
											}}
										/>
									</SettingsRow>
								</li>
							);
						}}
					</For>
				</ul>
			</section>

			<section class="settings-group" aria-labelledby={`${uid}-readback-title`}>
				<h3 class="settings-group-title" id={`${uid}-readback-title`}>
					Read-back
				</h3>
				<p class="settings-group-help">
					Tracker status → Compass state (derived, many-to-one).
				</p>
				<ul class="settings-reverse">
					<For each={reverse()}>
						{(group) => (
							<li class="settings-reverse-row">
								<span class="settings-reverse-status">
									{group.statuses.join(", ")}
								</span>
								<span class="settings-map-arrow" aria-hidden="true">
									→
								</span>
								<span class="settings-reverse-states">{group.label}</span>
							</li>
						)}
					</For>
				</ul>
			</section>

			<div class="settings-actions">
				<button
					type="button"
					class="cx-btn"
					data-variant="primary"
					disabled={!dirty() || hasEmptyMapping()}
					onClick={save}
				>
					Save
				</button>
				<button
					type="button"
					class="cx-btn"
					disabled={!dirty()}
					onClick={reset}
				>
					Reset
				</button>
			</div>
		</section>
	);
};
