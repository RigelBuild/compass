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

interface Run {
	row: BoardSubagent;
	// The task focused when the run started; unset when the board was not yet known.
	key?: string;
}

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
// Bounds memory when one id restarts faster than its settles are written.
const MAX_QUEUED_RUNS = 8;

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
	// Follow-up runs under an id whose settle is not yet written, oldest first; each
	// takes the id once the run before it lands, so no task's row stays open.
	const next = new Map<string, Run[]>();
	const changed = new Set<string>();
	let cancel: (() => void) | undefined;
	let inFlight: Promise<void> | undefined;
	let detached = false;
	let closed = false;
	// Open rows stored before this session belong to a process that is gone; the first
	// send closes them, so an idle sync makes no calls.
	let sweepPending = true;

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

	const focusKey = (): string => {
		const focus = freshness.focusTaskKey();
		return focus.length > 0 && freshness.hasTask(focus)
			? focus
			: SUBAGENTS_TASK_KEY;
	};

	const keyFor = (id: string): string => {
		const pinned = taskById.get(id);
		return pinned !== undefined && freshness.hasTask(pinned)
			? pinned
			: focusKey();
	};

	// The start-time task, when the board is known; otherwise the send picks it.
	const startKey = (): string | undefined =>
		freshness.boardKnown() ? focusKey() : undefined;

	const requestFor = (rows: { key: string; row: BoardSubagent }[]) => {
		const byTask = new Map<string, BoardSubagent[]>();
		for (const { key, row } of rows)
			byTask.set(key, [...(byTask.get(key) ?? []), row]);
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

	const request = (sent: BoardSubagent[], keys: Map<string, string>) =>
		requestFor(
			sent.map((row) => ({ key: keys.get(row.id) ?? SUBAGENTS_TASK_KEY, row })),
		);

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
		for (const id of changed) {
			drop(id);
			next.delete(id);
		}
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
		promote(s.id);
	};

	const promote = (id: string): void => {
		const queue = next.get(id);
		const run = queue?.shift();
		if (run === undefined) return;
		if (queue?.length === 0) next.delete(id);
		settled.delete(id);
		byId.set(id, run.row);
		if (run.key === undefined) taskById.delete(id);
		else taskById.set(id, run.key);
		changed.add(id);
		arm();
	};

	const requeue = (sent: BoardSubagent[]): void => {
		for (const s of sent) if (byId.get(s.id) === s) changed.add(s.id);
	};

	const onRefused = (sent: BoardSubagent[], error: Error): void => {
		// The known tasks may be stale (a task was removed), so re-read before retrying.
		freshness.forgetBoard();
		for (const s of sent) {
			if (refused.has(s.id)) forget(s);
			else if (byId.get(s.id) === s) {
				refused.add(s.id);
				changed.add(s.id);
			}
		}
		logSync("sync refused", error);
	};

	// Thrown and in-band transient faults share one bound, so an outage cannot grow
	// the pending rows without limit.
	const onFault = (sent: BoardSubagent[], error: Error): void => {
		let exhausted = false;
		for (const s of sent) {
			// A newer row under the id has its own retry allowance.
			if (byId.get(s.id) !== s) continue;
			const n = (faults.get(s.id) ?? 0) + 1;
			faults.set(s.id, n);
			if (n > TRANSIENT_RETRIES) exhausted = true;
		}
		if (exhausted) {
			onRefused(sent, error);
			return;
		}
		requeue(sent);
		logSync("sync failed", error);
	};

	const onWritten = (
		board: AgentBoard,
		sent: BoardSubagent[],
		keys: Map<string, string>,
	): void => {
		freshness.noteBoard(board);
		const trimmed: string[] = [];
		for (const s of sent) {
			const key = keys.get(s.id) ?? SUBAGENTS_TASK_KEY;
			// byId holds one run per id, so a newer row of this run keeps this task.
			if (byId.has(s.id)) taskById.set(s.id, key);
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

	const write = async (
		sent: BoardSubagent[],
		keys: Map<string, string>,
	): Promise<void> => {
		let result: CommsCallResult;
		try {
			result = await broker.call(request(sent, keys));
		} catch (error) {
			onFault(sent, error instanceof Error ? error : new Error(String(error)));
			return;
		}
		const board = boardIn(result);
		if (board !== undefined) {
			onWritten(board, sent, keys);
			return;
		}
		const error = boardError(result, "subagent board sync", "update_board");
		if (transient(result)) onFault(sent, error);
		else onRefused(sent, error);
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

	const aborted = (row: BoardSubagent): BoardSubagent =>
		create(BoardSubagentSchema, {
			...row,
			status: "aborted",
			endedAtUnixMs: BigInt(Math.trunc(timer.now())),
		});

	// A stored open row this session never started cannot settle, and it holds a slot
	// in the task's open-row cap, so it is closed once as aborted. An id this session
	// reuses on the same task needs nothing: the live row replaces it.
	const sweep = (): { key: string; row: BoardSubagent }[] => {
		sweepPending = false;
		const strays: { key: string; row: BoardSubagent }[] = [];
		for (const { key, row } of freshness.allOpenRows()) {
			if (!byId.has(row.id) && !next.has(row.id)) {
				byId.set(row.id, aborted(row));
				taskById.set(row.id, key);
				changed.add(row.id);
			} else if (keyFor(row.id) !== key)
				strays.push({ key, row: aborted(row) });
		}
		return strays;
	};

	// A stray row shares a live run's id, so it cannot join the pending rows; it is
	// closed in one best-effort write of its own.
	const closeStrays = async (
		strays: { key: string; row: BoardSubagent }[],
	): Promise<void> => {
		try {
			const result = await broker.call(requestFor(strays));
			const board = boardIn(result);
			if (board !== undefined) freshness.noteBoard(board);
			else
				logSync(
					"stray close failed",
					boardError(result, "subagent board sync", "update_board"),
				);
		} catch (error) {
			logSync("stray close failed", error);
		}
	};

	// Resolves true when a call answered, so the drain stops when nothing can move.
	const send = async (): Promise<boolean> => {
		if (!freshness.boardKnown() && !(await readBoard())) return false;
		if (closed) return false;
		if (sweepPending) {
			const strays = sweep();
			if (strays.length > 0) await closeStrays(strays);
			if (closed) return false;
		}
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

	const rowFor = (
		event: LifecycleEvent,
		starting: boolean,
		previous: BoardSubagent | undefined,
	): BoardSubagent => {
		const now = BigInt(Math.trunc(timer.now()));
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

	// True when the event belongs to a follow-up run that must wait for the id's settle.
	const queueBehindSettle = (
		event: LifecycleEvent,
		starting: boolean,
	): boolean => {
		const queue = next.get(event.id) ?? [];
		const current = byId.get(event.id);
		const last = queue.at(-1);
		if (
			last === undefined &&
			!(starting && current !== undefined && current.endedAtUnixMs !== 0n)
		)
			return false;
		// A start before the last queued run settled restarts that run on its own task,
		// as a live row does; only a settled run makes way for a new one.
		if (last !== undefined && (!starting || last.row.endedAtUnixMs === 0n)) {
			last.row = rowFor(event, starting, last.row);
			return true;
		}
		const key = startKey();
		// The server keys rows by id, so a later run on the same known task replaces it.
		if (last !== undefined && key !== undefined && last.key === key)
			queue.pop();
		queue.push({ row: rowFor(event, true, undefined), key });
		if (queue.length > MAX_QUEUED_RUNS) {
			queue.shift();
			logSync("run dropped", new Error(`id ${event.id} restarted too often`));
		}
		next.set(event.id, queue);
		return true;
	};

	// Each run belongs to the task focused when it started, not when it is sent.
	const pin = (id: string): void => {
		const key = startKey();
		if (key === undefined) taskById.delete(id);
		else taskById.set(id, key);
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
			if (queueBehindSettle(event, starting)) return;
			if (starting && !byId.has(event.id)) pin(event.id);
			byId.set(event.id, rowFor(event, starting, byId.get(event.id)));
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
		let queued = 0;
		for (const q of next.values()) queued += q.length;
		let passes = 2 * (changed.size + queued) + 2;
		while (changed.size > 0 && passes > 0 && !closed) {
			passes -= 1;
			if (!(await run())) return;
		}
	};

	// Subagents die with this process, so rows still open at shutdown close as aborted.
	const abortOpen = (): void => {
		for (const [id, row] of byId) {
			if (row.endedAtUnixMs !== 0n) continue;
			byId.set(id, aborted(row));
			refused.delete(id);
			faults.delete(id);
			changed.add(id);
		}
		for (const queue of next.values())
			for (const run of queue)
				if (run.row.endedAtUnixMs === 0n) run.row = aborted(run.row);
	};

	return async () => {
		unsubscribe();
		cancel?.();
		cancel = undefined;
		detached = true;
		abortOpen();
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
