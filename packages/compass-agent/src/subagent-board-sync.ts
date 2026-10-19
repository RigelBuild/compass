import { TASK_SUBAGENT_LIFECYCLE_CHANNEL } from "@oh-my-pi/pi-coding-agent/task";
import type { EventBus } from "@oh-my-pi/pi-coding-agent/utils/event-bus";
import { type BatchTimer, realBatchTimer } from "./batch-window";
import {
	type BoardFreshness,
	boardError,
	readBoardRequest,
	taskKeysOf,
} from "./board-freshness";
import type { CommsBroker } from "./comms";
import {
	type BoardSubagent,
	BoardSubagentSchema,
	BoardTaskPatchSchema,
	CommsCallRequestSchema,
	type CommsCallResult,
	create,
	UpdateAgentBoardRequestSchema,
} from "./compassv1";
import type { TurnSequence } from "./turn-sequence";

const SUBAGENT_SYNC_MS = 5_000;
const SUBAGENTS_TASK_KEY = "subagents";
const SETTLED_ID_LIMIT = 256;
const SHUTDOWN_FLUSH_MS = 5_000;

interface LifecycleEvent {
	id: string;
	agent: string;
	description?: string;
	status: "started" | "completed" | "failed" | "aborted";
}

function lifecycleEvent(data: unknown): LifecycleEvent | undefined {
	if (typeof data !== "object" || data === null) return undefined;
	const id = Reflect.get(data, "id");
	const agent = Reflect.get(data, "agent");
	const status = Reflect.get(data, "status");
	const description = Reflect.get(data, "description");
	if (
		typeof id !== "string" ||
		id.length === 0 ||
		typeof agent !== "string" ||
		typeof status !== "string" ||
		!(
			status === "started" ||
			status === "completed" ||
			status === "failed" ||
			status === "aborted"
		) ||
		(description !== undefined && typeof description !== "string")
	)
		return undefined;
	return {
		id,
		agent,
		status,
		...(description !== undefined ? { description } : {}),
	};
}

function logSync(what: string, error: unknown): void {
	// biome-ignore lint/suspicious/noConsole: automatic board sync failures are visible in the container log only
	console.error(`[compass-agent] subagent board ${what}:`, error);
}

// Sends at most one automatic patch per 5 s, one at a time, carrying each changed
// subagent's latest state. A transport failure re-queues; a refusal re-reads the
// board and retries once. The returned detach sends what is pending, within 5 s.
export function attachSubagentBoardSync(
	subagentBus: EventBus,
	broker: CommsBroker,
	turnSequence: TurnSequence,
	freshness: BoardFreshness,
	timer: BatchTimer = realBatchTimer,
): () => Promise<void> {
	const byId = new Map<string, BoardSubagent>();
	// The task a subagent's rows were last written to, so a focus change cannot strand it.
	const taskById = new Map<string, string>();
	// Ids already sent as settled; a late event for one would blank its stored fields.
	const settled = new Set<string>();
	// Ids in a refused batch; a second refusal drops them.
	const refused = new Set<string>();
	const changed = new Set<string>();
	let cancel: (() => void) | undefined;
	let inFlight: Promise<void> | undefined;
	let detached = false;
	let closed = false;

	const arm = (): void => {
		if (detached || inFlight || cancel !== undefined || changed.size === 0)
			return;
		cancel = timer.set(SUBAGENT_SYNC_MS, () => {
			cancel = undefined;
			// Wait for the startup read so the patch lands on the stored focus task.
			if (freshness.seeded()) void run();
			else arm();
		});
	};

	const keyFor = (id: string): string => {
		const pinned = taskById.get(id);
		if (pinned !== undefined && freshness.hasTask(pinned)) return pinned;
		const focus = freshness.focusTaskKey();
		return focus.length > 0 && freshness.hasTask(focus)
			? focus
			: SUBAGENTS_TASK_KEY;
	};

	const request = (sent: BoardSubagent[], keys: Map<string, string>) => {
		const byTask = new Map<string, BoardSubagent[]>();
		for (const s of sent) {
			const key = keys.get(s.id) ?? SUBAGENTS_TASK_KEY;
			byTask.set(key, [...(byTask.get(key) ?? []), s]);
		}
		const tasks = [...byTask].map(([key, upsertSubagents]) =>
			create(BoardTaskPatchSchema, {
				key,
				// A new task needs a title; a present title would rename an existing one.
				...(freshness.hasTask(key) ? {} : { title: "Subagents" }),
				upsertSubagents,
			}),
		);
		return create(CommsCallRequestSchema, {
			callId: `subagent-board-sync-${crypto.randomUUID()}`,
			call: {
				case: "updateBoard",
				value: create(UpdateAgentBoardRequestSchema, {
					upsertTasks: tasks,
					turnSequence: turnSequence.current(),
					automatic: true,
				}),
			},
		});
	};

	// A failed transport read keeps the rows pending; a refused or empty read falls
	// back to no focus, since a retry would get the same answer.
	const readBoard = async (): Promise<boolean> => {
		let result: CommsCallResult;
		try {
			result = await broker.call(
				readBoardRequest("subagent-board-read", turnSequence),
			);
		} catch (error) {
			logSync("read failed", error);
			return false;
		}
		const board =
			result.result.case === "updateBoard"
				? result.result.value.board
				: undefined;
		if (board === undefined) {
			freshness.noteBoard("", []);
			logSync(
				"read refused",
				boardError(result, "subagent board read", "update_board"),
			);
			return true;
		}
		freshness.noteBoard(board.focusTaskKey, taskKeysOf(board));
		return true;
	};

	const forget = (s: BoardSubagent): void => {
		refused.delete(s.id);
		if (byId.get(s.id) !== s || s.endedAtUnixMs === 0n) return;
		byId.delete(s.id);
		taskById.delete(s.id);
		settled.add(s.id);
		if (settled.size > SETTLED_ID_LIMIT)
			settled.delete(settled.values().next().value ?? "");
	};

	const requeue = (sent: BoardSubagent[]): void => {
		for (const s of sent) if (byId.get(s.id) === s) changed.add(s.id);
	};

	const onRefused = (sent: BoardSubagent[], result: CommsCallResult): void => {
		// The known tasks may be stale (a task was removed), so re-read before retrying.
		freshness.forgetBoard();
		for (const s of sent) {
			if (refused.has(s.id)) forget(s);
			else if (byId.get(s.id) === s) {
				refused.add(s.id);
				changed.add(s.id);
			}
		}
		logSync(
			"sync refused",
			boardError(result, "subagent board sync", "update_board"),
		);
	};

	const write = async (sent: BoardSubagent[]): Promise<void> => {
		const keys = new Map(sent.map((s) => [s.id, keyFor(s.id)]));
		let result: CommsCallResult;
		try {
			result = await broker.call(request(sent, keys));
		} catch (error) {
			requeue(sent);
			logSync("sync failed", error);
			return;
		}
		const board =
			result.result.case === "updateBoard"
				? result.result.value.board
				: undefined;
		if (board === undefined) {
			onRefused(sent, result);
			return;
		}
		freshness.noteBoard(board.focusTaskKey, taskKeysOf(board));
		for (const [id, key] of keys) taskById.set(id, key);
		for (const s of sent) forget(s);
	};

	const send = async (): Promise<void> => {
		if (!freshness.boardKnown() && !(await readBoard())) return;
		if (closed) return;
		const sent = [...changed].flatMap((id) => byId.get(id) ?? []);
		changed.clear();
		if (sent.length > 0) await write(sent);
	};

	function run(): Promise<void> {
		const done = send()
			.catch((error: unknown) => logSync("sync failed", error))
			.finally(() => {
				inFlight = undefined;
				arm();
			});
		inFlight = done;
		return done;
	}

	const unsubscribe = subagentBus.on(
		TASK_SUBAGENT_LIFECYCLE_CHANNEL,
		(data) => {
			const event = lifecycleEvent(data);
			if (event === undefined) return;
			const starting = event.status === "started";
			// A follow-up run reuses its id and starts again; only late settles are stale.
			if (starting) settled.delete(event.id);
			else if (settled.has(event.id)) return;
			const now = BigInt(Math.trunc(timer.now()));
			const previous = byId.get(event.id);
			byId.set(
				event.id,
				create(BoardSubagentSchema, {
					id: event.id,
					agent: event.agent,
					description: event.description ?? previous?.description ?? "",
					status: event.status,
					startedAtUnixMs: starting ? now : (previous?.startedAtUnixMs ?? 0n),
					endedAtUnixMs: starting ? 0n : now,
				}),
			);
			changed.add(event.id);
			arm();
		},
	);

	// Skips the seed gate: at shutdown the read happens here if the seed has not landed.
	const drain = async (): Promise<void> => {
		await inFlight;
		if (changed.size > 0 && !closed) await run();
	};

	return async () => {
		unsubscribe();
		cancel?.();
		cancel = undefined;
		detached = true;
		let stop = (): void => {};
		const deadline = new Promise<void>((resolve) => {
			stop = timer.set(SHUTDOWN_FLUSH_MS, resolve);
		});
		try {
			await Promise.race([drain(), deadline]);
		} finally {
			// A drain still running past the deadline must not start another call.
			closed = true;
			stop();
		}
	};
}
