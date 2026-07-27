// The tracker seam (design D2/T11/T12) — Compass state is canonical; a tracker's
// native status is a projection over it, through a user-editable mapping.
//
// This is a UI-side client seam, NOT a compass.v1 change (design D11): board /
// workstream state is UI-app-state for this record, so the seam lives here and
// the fixture implements it in-memory. When a later daemon board-state milestone
// moves it onto the wire, that PR carries the additive compass.v1 delta under
// the agent-state owner's review — not this module.

import type {
	TrackerConfig,
	TrackerKind,
	TrackerStatusMapping,
	Workstream,
	WorkstreamState,
} from "./stub-data";
import { STUB_ASSIGNED_ISSUES } from "./stub-data";

/**
 * The thin async contract the store calls to read/write the linked tracker. The
 * fixture implements it in-memory now (`createFixtureTrackerSeam`); the real
 * `@compass/client` implements it against the daemon later — a one-module swap.
 */
export interface TrackerSeam {
	/** The user's tracker-assigned issues, for the Backlog view (D3). */
	listAssignedIssues(handle: string): Promise<Workstream[]>;
	/** Mirror a Compass state change onto the tracker (D2), mapping through
	 *  `TrackerStatusMapping.toTracker` before the write. */
	updateIssueStatus(id: string, compassState: WorkstreamState): Promise<void>;
}

/**
 * The default Linear projection (D2). `toTracker` is total over Compass states;
 * `fromTracker` is many-to-one — Linear lacks a `Queued`, so both `todo` and
 * `queued` project to `Todo`, and `Cancelled`/`Duplicate` both read back as
 * `done` (the design's named many-to-one case).
 */
export const LINEAR_STATUS_MAPPING: TrackerStatusMapping = {
	kind: "linear",
	toTracker: {
		backlog: "Backlog",
		todo: "Todo",
		queued: "Todo",
		blocked: "In Progress",
		in_progress: "In Progress",
		in_review: "In Review",
		done: "Done",
	},
	fromTracker: {
		Backlog: "backlog",
		Todo: "todo",
		"In Progress": "in_progress",
		"In Review": "in_review",
		Done: "done",
		Cancelled: "done",
		Duplicate: "done",
	},
};

/** The default tracker wiring — Linear, a placeholder handle, the Linear map. */
export const DEFAULT_TRACKER_CONFIG: TrackerConfig = {
	kind: "linear",
	handle: "matt@sealed",
	mapping: LINEAR_STATUS_MAPPING,
};

/** Project a Compass state onto the tracker's native status name (D2). Total
 *  over the union, so every state has a target. */
export function toTrackerStatus(
	state: WorkstreamState,
	mapping: TrackerStatusMapping,
): string {
	return mapping.toTracker[state];
}

/** Read a tracker's native status back to a Compass state (D2). Many-to-one;
 *  an unmapped status falls back to `backlog` (a safe pre-active default rather
 *  than dropping the issue). */
export function fromTrackerStatus(
	status: string,
	mapping: TrackerStatusMapping,
): WorkstreamState {
	return mapping.fromTracker[status] ?? "backlog";
}

/**
 * The in-memory fixture seam. `listAssignedIssues` returns the user's fixture
 * issues; `updateIssueStatus` maps the state through the config and resolves
 * (the fixture *is* the tracker, so there's nothing to POST). The store swaps
 * this for a `@compass/client`-backed seam when the daemon grows the contract.
 */
export function createFixtureTrackerSeam(
	config: TrackerConfig = DEFAULT_TRACKER_CONFIG,
): TrackerSeam {
	return {
		listAssignedIssues(handle: string): Promise<Workstream[]> {
			// The fixture ignores the handle beyond the contract; the real seam
			// queries the tracker for `handle`'s assigned issues. An empty handle
			// (tracker not configured) yields nothing.
			return Promise.resolve(handle ? STUB_ASSIGNED_ISSUES : []);
		},
		updateIssueStatus(
			_id: string,
			compassState: WorkstreamState,
		): Promise<void> {
			// Map before the (no-op) write so the mapping is exercised, matching
			// the real write path's shape.
			void toTrackerStatus(compassState, config.mapping);
			return Promise.resolve();
		},
	};
}

export type { TrackerKind };
