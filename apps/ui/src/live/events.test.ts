import { describe, expect, test } from "bun:test";
import {
	CompassService,
	create,
	createCompassClient,
	createRouterTransport,
	IssueSchema,
	IssueState,
	type SubscribeEventsResponse,
	SubscribeEventsResponseSchema,
	type Issue as WireIssue,
} from "@compass/client";
import type { Issue as DomainIssue } from "../stub-data";
import { runEventStream } from "./events";

// The SubscribeEvents read-driver seam (SEA-1729 read slice). A fake server is a
// `createRouterTransport` handler serving CompassService.SubscribeEvents — the
// vendor's documented no-HTTP test path — so the round-trip exercises the real
// driver + adapt boundary, never a mock. The handler is an async generator that
// yields a SCRIPTED sequence of responses, then blocks until the test aborts so
// the driver tails (never re-subscribes) meanwhile. Each assertion drains the
// microtask queue until the driver's OWN output (the onIssues sink) satisfies
// the predicate — no wall-clock timer.

// A partial Issue init, matching what `create(IssueSchema, ...)` accepts.
type IssueInit = Parameters<typeof create<typeof IssueSchema>>[1];

// Build a positioned issue-upsert response at `seq` carrying an issue with `id`
// (plus optional overrides). instanceEpoch is a fixed nonzero nonce the client
// echoes on reconnect; the fake ignores the request, so its value is arbitrary.
function issueResp(
	seq: bigint,
	id: string,
	overrides: IssueInit = {},
): SubscribeEventsResponse {
	return create(SubscribeEventsResponseSchema, {
		seq,
		instanceEpoch: 7n,
		payload: {
			case: "issue",
			value: create(IssueSchema, { id, ...overrides }),
		},
	});
}

// A terminal resync directive (seq = 0, not a cursor).
function resyncResp(): SubscribeEventsResponse {
	return create(SubscribeEventsResponseSchema, {
		seq: 0n,
		instanceEpoch: 7n,
		payload: { case: "resyncRequired", value: {} },
	});
}

// A positioned NON-issue tail payload (serverStatus): the class of response the
// shared SubscribeEvents stream is dominated by. The board driver ignores it —
// never mapped, never thrown on — but it still advances the cursor and counts
// as connection-liveness progress (the backoff anti-spin signal).
function serverStatusResp(seq: bigint): SubscribeEventsResponse {
	return create(SubscribeEventsResponseSchema, {
		seq,
		instanceEpoch: 7n,
		payload: { case: "serverStatus", value: {} },
	});
}

// A board-snapshot issue for the ListBoardIssues catch-up read (the durable
// re-snapshot the cold-start boundary unions with the tail).
function boardIssue(id: string, overrides: IssueInit = {}): WireIssue {
	return create(IssueSchema, { id, ...overrides });
}

// A transport whose SubscribeEvents yields `script` then holds the stream open
// (the live tail) until the client aborts, at which point the generator unwinds.
// `catchUp` scripts the ListBoardIssues durable re-snapshot the driver reads at
// the cold-start boundary: an issue array serves that board; `catchUpError`
// makes the read reject (the server-has-not-wired-it-yet path). With neither,
// ListBoardIssues is left unimplemented — the driver's best-effort read fails
// and it tails on, the pre-catch-up behavior.
function scriptedTransport(
	script: readonly SubscribeEventsResponse[],
	opts: { catchUp?: WireIssue[]; catchUpError?: Error } = {},
) {
	return createRouterTransport(({ service }) => {
		service(CompassService, {
			subscribeEvents: async function* (_req, ctx) {
				for (const resp of script) yield resp;
				const { promise, resolve } = Promise.withResolvers<void>();
				ctx.signal.addEventListener("abort", () => resolve(), { once: true });
				await promise;
			},
			...(opts.catchUp || opts.catchUpError
				? {
						listBoardIssues: async () => {
							if (opts.catchUpError) throw opts.catchUpError;
							return { issues: opts.catchUp ?? [] };
						},
					}
				: {}),
		});
	});
}

// Drain the microtask queue until `predicate` holds or a bounded hop count
// elapses. createRouterTransport's in-memory server yields across several
// microtask hops, so this polls the real signal (the collected onIssues output)
// rather than guessing a fixed count — no wall-clock timer.
async function drainUntil(predicate: () => boolean): Promise<void> {
	for (let i = 0; i < 500 && !predicate(); i++) {
		await Promise.resolve();
	}
}

describe("runEventStream (SEA-1729 read driver)", () => {
	test("pushes adapted domain issues; a repeat id REPLACES (upsert)", async () => {
		const transport = scriptedTransport([
			issueResp(1n, "a", { title: "first", state: IssueState.TODO }),
			issueResp(2n, "b", { title: "second", state: IssueState.BACKLOG }),
			// Re-send id "a" with a new title/state: an upsert, not an append.
			issueResp(3n, "a", { title: "first-v2", state: IssueState.IN_PROGRESS }),
		]);
		const client = createCompassClient(transport);
		const abort = new AbortController();
		const snapshots: string[][] = [];
		let latest: DomainIssue[] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => {
				latest = issues;
				snapshots.push(issues.map((i) => i.id));
			},
			signal: abort.signal,
		});
		try {
			await drainUntil(() => {
				const last = snapshots.at(-1);
				return (last?.length ?? 0) === 2 && snapshots.length >= 3;
			});
			// Two ids after three upserts → the repeat replaced, not appended.
			expect([...(snapshots.at(-1) ?? [])].sort()).toEqual(["a", "b"]);
			// The replacement carried the new field values (real adapt round-trip).
			const a = latest.find((i) => i.id === "a");
			expect(a?.title).toBe("first-v2");
			expect(a?.state).toBe("in_progress");
		} finally {
			abort.abort();
			await run;
		}
	});

	test("resyncRequired clears the board then re-snapshots", async () => {
		const transport = scriptedTransport([
			issueResp(1n, "a"),
			issueResp(2n, "b"),
			// Server directive: discard state and re-snapshot from a cold start.
			resyncResp(),
		]);
		const client = createCompassClient(transport);
		const abort = new AbortController();
		const snapshots: string[][] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => snapshots.push(issues.map((i) => i.id)),
			signal: abort.signal,
		});
		try {
			// A pre-clear snapshot held both ids; the resync then pushed an empty
			// board; the reconnect replays the script and re-snapshots.
			await drainUntil(
				() =>
					snapshots.some((s) => s.length === 0) &&
					snapshots.some((s) => s.length === 2),
			);
			expect(snapshots.some((s) => s.length === 0)).toBe(true);
			expect(
				snapshots.some(
					(s) => s.length === 2 && s.includes("a") && s.includes("b"),
				),
			).toBe(true);
		} finally {
			abort.abort();
			await run;
		}
	});

	test("cold-start reads ListBoardIssues and unions it with the tail (dedup by id)", async () => {
		// The durable board carries "old" (ring-evicted) + "a" (also in the tail).
		// The tail then upserts "a" (a newer state) and adds "b". The union is the
		// three ids; the shared "a" dedups to ONE row carrying the tail's value.
		const transport = scriptedTransport(
			[
				issueResp(1n, "a", { title: "a-tail", state: IssueState.IN_PROGRESS }),
				issueResp(2n, "b", { title: "b-tail", state: IssueState.TODO }),
			],
			{
				catchUp: [
					boardIssue("old", { title: "durable-only", state: IssueState.DONE }),
					boardIssue("a", { title: "a-snapshot", state: IssueState.BACKLOG }),
				],
			},
		);
		const client = createCompassClient(transport);
		const abort = new AbortController();
		let latest: DomainIssue[] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => {
				latest = issues;
			},
			signal: abort.signal,
		});
		try {
			// Wait until all three ids are present (catch-up "old" + tail "a"/"b").
			await drainUntil(() => latest.length === 3);
			expect([...latest.map((i) => i.id)].sort()).toEqual(["a", "b", "old"]);
			// The durable-only issue survives (the ring would have evicted it).
			expect(latest.find((i) => i.id === "old")?.title).toBe("durable-only");
			// The shared id dedups to the TAIL's value, not the snapshot's — the
			// tail upsert of "a" lands after the catch-up read into the same map.
			const a = latest.find((i) => i.id === "a");
			expect(a?.title).toBe("a-tail");
			expect(a?.state).toBe("in_progress");
		} finally {
			abort.abort();
			await run;
		}
	});

	test("catch-up surfaces ARCHIVED issues the tail ring omits", async () => {
		// ARCHIVED issues are hidden from the active board but the Done view's
		// Archived section needs them; only the durable ListBoardIssues read
		// carries them (the live tail fans upserts of active work).
		const transport = scriptedTransport([issueResp(1n, "active")], {
			catchUp: [
				boardIssue("archived-1", { state: IssueState.ARCHIVED }),
				boardIssue("active", { state: IssueState.IN_PROGRESS }),
			],
		});
		const client = createCompassClient(transport);
		const abort = new AbortController();
		let latest: DomainIssue[] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => {
				latest = issues;
			},
			signal: abort.signal,
		});
		try {
			await drainUntil(() => latest.some((i) => i.id === "archived-1"));
			expect(latest.find((i) => i.id === "archived-1")?.state).toBe("archived");
		} finally {
			abort.abort();
			await run;
		}
	});

	test("a failing catch-up read degrades gracefully: onError fires, the tail still populates", async () => {
		// The server has not wired ListBoardIssues yet (Unimplemented). The
		// best-effort read must report the error and keep tailing — a missing
		// durable read never blanks the board the live tail can still fill.
		const transport = scriptedTransport(
			[issueResp(1n, "a"), issueResp(2n, "b")],
			{ catchUpError: new Error("ListBoardIssues unimplemented") },
		);
		const client = createCompassClient(transport);
		const abort = new AbortController();
		const errors: unknown[] = [];
		let latest: DomainIssue[] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => {
				latest = issues;
			},
			onError: (error) => errors.push(error),
			signal: abort.signal,
		});
		try {
			// The tail still delivers both ids despite the failed catch-up.
			await drainUntil(() => latest.length === 2);
			expect([...latest.map((i) => i.id)].sort()).toEqual(["a", "b"]);
			// The failure was surfaced, not swallowed.
			expect(errors.length).toBeGreaterThanOrEqual(1);
		} finally {
			abort.abort();
			await run;
		}
	});

	test("a non-issue tail payload is ignored without disturbing the board", async () => {
		// The shared stream interleaves agent-lifecycle / server-status frames the
		// board doesn't consume. One lands here between two issue upserts: it must
		// not throw, not blank the board, and not stop the later issue landing.
		// A successful (empty) catch-up isolates the variable under test to the
		// interleaved non-issue frame — no Unimplemented read to muddy `errors`.
		const transport = scriptedTransport(
			[
				issueResp(1n, "a", { state: IssueState.TODO }),
				serverStatusResp(2n),
				issueResp(3n, "b", { state: IssueState.BACKLOG }),
			],
			{ catchUp: [] },
		);
		const client = createCompassClient(transport);
		const abort = new AbortController();
		const errors: unknown[] = [];
		let latest: DomainIssue[] = [];
		const run = runEventStream({
			client,
			onIssues: (issues) => {
				latest = issues;
			},
			onError: (error) => errors.push(error),
			signal: abort.signal,
		});
		try {
			// Both issues land; the interleaved non-issue frame neither threw nor
			// dropped "a" from the board.
			await drainUntil(() => latest.length === 2);
			expect([...latest.map((i) => i.id)].sort()).toEqual(["a", "b"]);
			expect(errors).toEqual([]);
		} finally {
			abort.abort();
			await run;
		}
	});
});
