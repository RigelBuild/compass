// The offline fixture boot (T2). Reached ONLY via the dynamic `import()` in index.tsx's
// `import.meta.env.MODE === "fixture"` branch, so in every non-fixture build that branch
// dead-code-eliminates and this chunk is never emitted (the hard wall, §A1). The PROD
// tripwire below is defense-in-depth behind the build-scan gate `fixture-wall.test.ts`.

import { AgentPresence, TourOutcome } from "@compass/client";
import { createEffect, createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import {
	REPLAY_AGENT_ID,
	REPLAY_BOARD,
	REPLAY_COMMS_SNAPSHOT,
	REPLAY_EVENTS,
} from "./fixture-wire";
import { createFakeComms } from "./live/comms-fake";
import { createFakeCompass } from "./live/compass-fake";
import { mountShell, newAppQueryClient } from "./mount";
import { createAppStore, type TourClient } from "./store";
import { sessionLayoutStorage } from "./window-layout";

/** The unique build-scan sentinel — the literal `fixture-wall.test.ts` asserts
 *  is ABSENT from a production bundle. Referenced by the PROD tripwire so the
 *  literal is guaranteed present in this module. */
export const FIXTURE_SENTINEL = "COMPASS-FIXTURE-BOOT-SENTINEL-7f3a";

/** Per-page-load tour state: the fixture has no server or account, so nothing
 *  persists past a reload. The boot claims only once a consumer sets `claimFirstRun`. */
export function createMemoryTourClient(): TourClient {
	let outcome = TourOutcome.UNSPECIFIED;
	let stepId = "";
	return {
		getTourState: async () => ({ outcome, stepId }),
		claimTourStart: async (req) => {
			if (outcome !== TourOutcome.UNSPECIFIED) return { claimed: false };
			outcome = TourOutcome.STARTED;
			stepId = req.stepId;
			return { claimed: true };
		},
		setTourState: async (req) => {
			outcome = req.outcome;
			stepId = req.stepId;
			return {};
		},
	};
}
/** Boot the UI fully offline, seeded from the existing fixtures, and mount the
 *  same shell the live boot mounts. `?empty` seeds an empty board; `?replay`
 *  hands the store fake compass and comms clients, so its stream drivers run
 *  against scripted frames. Returns a disposer for the shell and the store root;
 *  production's dynamic-import boot ignores it (the app owns the page). */
export function bootFixture(root: HTMLElement): () => void {
	// Runtime tripwire (§A1): a build that gained `--mode fixture` while defaulting
	// NODE_ENV to production trips here. It is insurance — the build-scan gate is
	// the wall — but references FIXTURE_SENTINEL so the sentinel literal is present.
	if (import.meta.env.PROD) {
		throw new Error(
			`${FIXTURE_SENTINEL} must never boot in a production build`,
		);
	}

	const queryClient = newAppQueryClient();

	const params = new URLSearchParams(location.search);
	const emptyBoard = params.has("empty");
	const replay = params.has("replay");
	const compass = replay
		? createFakeCompass({ board: REPLAY_BOARD, events: REPLAY_EVENTS })
		: undefined;
	const comms = replay ? createFakeComms(REPLAY_COMMS_SNAPSHOT) : undefined;
	let disposeStore: (() => void) | undefined;
	const store = createRoot((dispose) => {
		disposeStore = dispose;
		const store = createAppStore({
			queryClient,
			initialComms: STUB_COMMS_STATE,
			workspaceKey: "fixture",
			layoutStorage: sessionLayoutStorage(),
			tour: createMemoryTourClient(),
			...(emptyBoard ? { initialIssues: [] } : {}),
			...(compass ? { compass: compass.client } : {}),
			...(comms ? { comms: comms.client } : {}),
		});
		if (replay && comms) {
			createEffect(
				() => store.agentSessionById(REPLAY_AGENT_ID)?.running,
				(running) => {
					if (!running) return;
					void comms.emit(
						{
							case: "agentPresenceChanged",
							value: {
								agentAccountId: REPLAY_AGENT_ID,
								presence: AgentPresence.WAITING,
								activity: "",
							},
						},
						1n,
					);
				},
			);
		}
		return store;
	});
	// Search keeps its fixture fallback: the fakes do not implement the search RPCs.
	const disposeShell = mountShell(root, store, queryClient);
	return () => {
		disposeShell();
		disposeStore?.();
	};
}
