import { TASK_SUBAGENT_LIFECYCLE_CHANNEL } from "@oh-my-pi/pi-coding-agent/task";
import type { EventBus } from "@oh-my-pi/pi-coding-agent/utils/event-bus";
import { type BatchTimer, realBatchTimer } from "./batch-window";
import {
	type BoardFreshness,
	boardError,
	readBoardRequest,
} from "./board-freshness";
import type { CommsBroker } from "./comms";
import {
	type AgentBoard,
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

// Connect code tokens (`connect.CodeOf(err).String()`) for faults that clear on
// their own; any other code is a refusal a retry would repeat.
const TRANSIENT_CODES = new Set([
	"canceled",
	"unknown",
	"deadline_exceeded",
	"aborted",
	"resource_exhausted",
	"internal",
	"unavailable",
]);
// The server also reports some lasting faults as internal, so a batch gets this many
// transient retries before its rows retry alone, then drop.
const TRANSIENT_RETRIES = 3;

const transient = (result: CommsCallResult): boolean =>
	result.result.case === "error" &&
	TRANSIENT_CODES.has(result.result.value.code);

const boardIn = (result: CommsCallResult) =>
	result.result.case === "updateBoard" ? result.result.value.board : undefined;

function onBoard(board: AgentBoard, key: string, id: string): boolean {
	const task = board.tasks.find((t) => t.key === key);
	return task?.subagents.some((s) => s.id === id) === true;
}

// The id keys the server merge, so one it would refuse cannot be clipped.
function storableId(id: string): boolean {
	return (
		!id.includes("\0") &&
		clip(id, ID_CHARS + 1).length === clip(id, ID_CHARS).length
	);
}

// Reads at most `max` code points, so a huge input costs only what is kept.
function clip(text: string, max: number): string {
	let out = "";
	let kept = 0;
	for (const ch of text) {
		if (kept === max) break;
		if (ch === "\0") continue;
		out += ch;
		kept += 1;
	}
	return out;
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
	// Transient faults per id; past the limit the rows take the refusal path.
	const faults = new Map<string, number>();
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
			freshness.noteBoard(board);
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
		faults.delete(id);
	};

	const forget = (s: BoardSubagent): void => {
		refused.delete(s.id);
		faults.delete(s.id);
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

	const onFault = (sent: BoardSubagent[], result: CommsCallResult): void => {
		let exhausted = false;
		for (const s of sent) {
			const n = (faults.get(s.id) ?? 0) + 1;
			faults.set(s.id, n);
			if (n > TRANSIENT_RETRIES) exhausted = true;
		}
		if (exhausted) {
			onRefused(sent, result);
			return;
		}
		requeue(sent);
		logSync(
			"sync failed",
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
			if (transient(result)) onFault(sent, result);
			else onRefused(sent, result);
			return;
		}
		freshness.noteBoard(board);
		const trimmed: string[] = [];
		for (const s of sent) {
			const key = keys.get(s.id) ?? SUBAGENTS_TASK_KEY;
			// A newer run under the same id keeps the pin its start set.
			if (byId.get(s.id) === s) taskById.set(s.id, key);
			if (!onBoard(board, key, s.id)) trimmed.push(s.id);
			forget(s);
		}
		// A task full of open rows from elsewhere makes the store drop the incoming row.
		if (trimmed.length > 0)
			logSync(
				"rows trimmed",
				new Error(`full task dropped ${trimmed.join(", ")}`),
			);
	};

	// The server refuses a task with more than 20 open rows, so a start that would add
	// one waits until the board shows room. Counts come from the last board returned.
	const hasRoom = (
		row: BoardSubagent,
		key: string,
		opening: Map<string, number>,
	): boolean => {
		const stored = freshness.openRows(key);
		if (row.endedAtUnixMs !== 0n || stored.has(row.id)) return true;
		const count = stored.size + (opening.get(key) ?? 0);
		if (count >= MAX_OPEN_PER_TASK) return false;
		opening.set(key, (opening.get(key) ?? 0) + 1);
		return true;
	};

	const pick = (
		ids: Iterable<string>,
		limit: number,
		keys: Map<string, string>,
		opening: Map<string, number>,
	): void => {
		for (const id of ids) {
			if (keys.size >= limit) return;
			const row = byId.get(id);
			const key = keyFor(id);
			if (row !== undefined && hasRoom(row, key, opening)) keys.set(id, key);
		}
	};

	// A refused row retries alone, so a second refusal drops only that row. One with no
	// room waits, and the other rows go out without it.
	const nextBatch = (): {
		sent: BoardSubagent[];
		keys: Map<string, string>;
	} => {
		const keys = new Map<string, string>();
		const opening = new Map<string, number>();
		pick(
			[...refused].filter((id) => changed.has(id)),
			1,
			keys,
			opening,
		);
		if (keys.size === 0)
			pick(
				[...changed].filter((id) => !refused.has(id)),
				MAX_ROWS_PER_WRITE,
				keys,
				opening,
			);
		for (const id of keys.keys()) changed.delete(id);
		return {
			sent: [...keys.keys()].flatMap((id) => byId.get(id) ?? []),
			keys,
		};
	};

	// Resolves true when a call answered, so the drain stops when nothing can move.
	const send = async (): Promise<boolean> => {
		if (!freshness.boardKnown() && !(await readBoard())) return false;
		if (closed) return false;
		const { sent, keys } = nextBatch();
		if (sent.length === 0) return false;
		await write(sent, keys);
		return true;
	};

	function run(): Promise<boolean> {
		const done = send()
			.catch((error: unknown) => {
				logSync("sync failed", error);
				return true;
			})
			.finally(() => {
				inFlight = undefined;
				arm();
			});
		inFlight = done.then(() => {});
		return done;
	}

	const rowFor = (event: LifecycleEvent, starting: boolean): BoardSubagent => {
		const now = BigInt(Math.trunc(timer.now()));
		const previous = byId.get(event.id);
		return create(BoardSubagentSchema, {
			id: event.id,
			agent: clip(event.agent, AGENT_CHARS),
			description: clip(
				event.description ?? previous?.description ?? "",
				DESCRIPTION_CHARS,
			),
			status: event.status,
			startedAtUnixMs: starting ? now : (previous?.startedAtUnixMs ?? 0n),
			endedAtUnixMs: starting ? 0n : now,
		});
	};

	const unsubscribe = subagentBus.on(
		TASK_SUBAGENT_LIFECYCLE_CHANNEL,
		(data) => {
			const event = lifecycleEvent(data);
			if (event === undefined || !storableId(event.id)) return;
			const starting = event.status === "started";
			// A follow-up run reuses its id and starts again; only late settles are stale.
			if (starting) settled.delete(event.id);
			else if (settled.has(event.id)) return;
			// Each run belongs to the task focused when it started, not when it is sent;
			// a follow-up run under a reused id gets a fresh pin.
			if (starting) {
				taskById.delete(event.id);
				if (freshness.boardKnown()) taskById.set(event.id, keyFor(event.id));
			}
			byId.set(event.id, rowFor(event, starting));
			// A new state is a new row; an earlier refusal was about the old one.
			refused.delete(event.id);
			faults.delete(event.id);
			changed.add(event.id);
			arm();
		},
	);

	// Skips the seed gate: at shutdown the read happens here if the seed has not landed.
	// Each pass sends one batch, or one refused row alone; the budget covers both, and
	// a pass that sends nothing ends the drain.
	const drain = async (): Promise<void> => {
		await inFlight;
		let passes = 2 * changed.size + 2;
		while (changed.size > 0 && passes > 0 && !closed) {
			passes -= 1;
			if (!(await run())) return;
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
