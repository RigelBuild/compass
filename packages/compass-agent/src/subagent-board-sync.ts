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

// Server caps (store.MaxBoardSubagents and the subagent field limits), in runes.
const MAX_ROWS_PER_WRITE = 20;
const MAX_OPEN_PER_TASK = 20;
const ID_CHARS = 64;
const AGENT_CHARS = 64;
const DESCRIPTION_CHARS = 200;

// Server faults clear on their own; any other code is a refusal a retry would repeat.
const TRANSIENT_CODES = new Set([
	"internal",
	"unavailable",
	"deadline_exceeded",
	"aborted",
	"resource_exhausted",
	"unknown",
]);

const transient = (result: CommsCallResult): boolean =>
	result.result.case === "error" &&
	TRANSIENT_CODES.has(result.result.value.code);

const boardIn = (result: CommsCallResult) =>
	result.result.case === "updateBoard" ? result.result.value.board : undefined;

const clip = (text: string, max: number): string =>
	[...text.replaceAll("\0", "")].slice(0, max).join("");

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
	// Ids last written as started, by task; the server refuses past 20 per task.
	const open = new Map<string, string>();
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

	// A transient failure keeps the rows pending. A permanent refusal drops them: the
	// write would be refused too, and guessing the tasks could rename one.
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
		const board = boardIn(result);
		if (board !== undefined) {
			freshness.noteBoard(board.focusTaskKey, taskKeysOf(board));
			return true;
		}
		const error = boardError(result, "subagent board read", "update_board");
		if (transient(result)) {
			logSync("read failed", error);
			return false;
		}
		for (const id of changed) drop(id);
		changed.clear();
		logSync("read refused", error);
		return false;
	};

	const drop = (id: string): void => {
		byId.delete(id);
		taskById.delete(id);
		refused.delete(id);
		open.delete(id);
	};

	const forget = (s: BoardSubagent): void => {
		refused.delete(s.id);
		if (byId.get(s.id) !== s || s.endedAtUnixMs === 0n) return;
		drop(s.id);
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
			if (refused.has(s.id)) {
				forget(s);
				open.delete(s.id);
			} else if (byId.get(s.id) === s) {
				refused.add(s.id);
				changed.add(s.id);
			}
		}
		logSync(
			"sync refused",
			boardError(result, "subagent board sync", "update_board"),
		);
	};

	const write = async (
		sent: BoardSubagent[],
		keys: Map<string, string>,
	): Promise<void> => {
		let result: CommsCallResult;
		try {
			result = await broker.call(request(sent, keys));
		} catch (error) {
			requeue(sent);
			logSync("sync failed", error);
			return;
		}
		const board = boardIn(result);
		if (board === undefined) {
			if (!transient(result)) return onRefused(sent, result);
			requeue(sent);
			logSync(
				"sync failed",
				boardError(result, "subagent board sync", "update_board"),
			);
			return;
		}
		freshness.noteBoard(board.focusTaskKey, taskKeysOf(board));
		for (const s of sent) {
			const key = keys.get(s.id) ?? SUBAGENTS_TASK_KEY;
			taskById.set(s.id, key);
			if (s.endedAtUnixMs === 0n) open.set(s.id, key);
			else open.delete(s.id);
			forget(s);
		}
	};

	// A start that would push its task past the server's open-row cap waits for room.
	const hasRoom = (
		row: BoardSubagent,
		key: string,
		opening: Map<string, number>,
	): boolean => {
		if (row.endedAtUnixMs !== 0n || open.get(row.id) === key) return true;
		let count = opening.get(key) ?? 0;
		for (const task of open.values()) if (task === key) count += 1;
		if (count >= MAX_OPEN_PER_TASK) return false;
		opening.set(key, (opening.get(key) ?? 0) + 1);
		return true;
	};

	// Rows refused once retry alone, so a second refusal drops only the bad row.
	const nextBatch = (): {
		sent: BoardSubagent[];
		keys: Map<string, string>;
	} => {
		const ids = [...changed];
		const retry = ids.find((id) => refused.has(id));
		const keys = new Map<string, string>();
		const opening = new Map<string, number>();
		for (const id of retry === undefined ? ids : [retry]) {
			if (keys.size === MAX_ROWS_PER_WRITE) break;
			const row = byId.get(id);
			const key = keyFor(id);
			if (row !== undefined && hasRoom(row, key, opening)) keys.set(id, key);
		}
		for (const id of keys.keys()) changed.delete(id);
		return {
			sent: [...keys.keys()].flatMap((id) => byId.get(id) ?? []),
			keys,
		};
	};

	const send = async (): Promise<void> => {
		if (!freshness.boardKnown() && !(await readBoard())) return;
		if (closed) return;
		const { sent, keys } = nextBatch();
		if (sent.length > 0) await write(sent, keys);
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
			// The id keys the server merge, so one it would refuse cannot be clipped.
			if (
				event === undefined ||
				event.id.includes("\0") ||
				[...event.id].length > ID_CHARS
			)
				return;
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
					agent: clip(event.agent, AGENT_CHARS),
					description: clip(
						event.description ?? previous?.description ?? "",
						DESCRIPTION_CHARS,
					),
					status: event.status,
					startedAtUnixMs: starting ? now : (previous?.startedAtUnixMs ?? 0n),
					endedAtUnixMs: starting ? 0n : now,
				}),
			);
			// A new state is a new row; an earlier refusal was about the old one.
			refused.delete(event.id);
			changed.add(event.id);
			arm();
		},
	);

	// Skips the seed gate: at shutdown the read happens here if the seed has not landed.
	// Each pass sends one batch, or one row retrying alone after a refusal; the budget
	// covers both, so a failing transport cannot spin until the deadline.
	const drain = async (): Promise<void> => {
		await inFlight;
		let passes = 2 * changed.size + 2;
		while (changed.size > 0 && passes > 0 && !closed) {
			passes -= 1;
			await run();
		}
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
