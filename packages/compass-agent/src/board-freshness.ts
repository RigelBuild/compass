import { type BatchTimer, realBatchTimer } from "./batch-window";
import type { CommsBroker } from "./comms";
import {
	type AgentBoard,
	type CommsCallRequest,
	CommsCallRequestSchema,
	type CommsCallResult,
	create,
	UpdateAgentBoardRequestSchema,
} from "./compassv1";
import { attr, flat } from "./render-guard";
import type { TurnSequence } from "./turn-sequence";

/** The last agent-written board write, by turn and time, plus the board's tasks. */
export class BoardFreshness {
	// Only this session's writes set a turn; a stored turn may be another session's.
	#writtenTurn: bigint | undefined;
	#writtenAtMs: number | undefined;
	#writes = 0;
	#focusTaskKey = "";
	// Undefined until a read or write returned the board.
	#taskKeys: ReadonlySet<string> | undefined;
	// Any later read or write is newer than the startup seed, even after a forget.
	#boardNoted = false;
	#settled = false;

	// Keeps the stored time and tasks; a seed landing after any later read or write is
	// stale, and only this session's writes set the time.
	seed(
		_writtenTurn: bigint,
		writtenAtMs: number,
		focusTaskKey = "",
		taskKeys: readonly string[] = [],
	): void {
		this.#settled = true;
		if (this.#writes === 0)
			this.#writtenAtMs = writtenAtMs > 0 ? writtenAtMs : undefined;
		if (this.#boardNoted) return;
		this.noteBoard(focusTaskKey, taskKeys);
	}

	/** A failed seed still settles: the board then counts as never written. */
	seedFailed(): void {
		this.#settled = true;
	}

	/** True once the startup read landed or failed, or this session wrote. */
	seeded(): boolean {
		return this.#settled || this.#writes > 0;
	}

	noteWrite(
		turn: bigint,
		atMs: number,
		focusTaskKey?: string,
		taskKeys?: readonly string[],
	): void {
		this.#writtenTurn = turn;
		this.#writtenAtMs = atMs;
		this.#writes += 1;
		if (focusTaskKey !== undefined)
			this.noteBoard(focusTaskKey, taskKeys ?? []);
	}

	/** Records the focus and task keys of a board the server returned. */
	noteBoard(focusTaskKey: string, taskKeys: readonly string[]): void {
		this.#boardNoted = true;
		this.#focusTaskKey = focusTaskKey;
		this.#taskKeys = new Set(taskKeys);
	}

	/** Drops the known tasks so the next sync reads the board first. */
	forgetBoard(): void {
		this.#taskKeys = undefined;
	}

	boardKnown(): boolean {
		return this.#taskKeys !== undefined;
	}

	hasTask(key: string): boolean {
		return this.#taskKeys?.has(key) === true;
	}

	wroteThisTurn(turn: bigint): boolean {
		return this.#writtenTurn === turn;
	}

	/** Counts this session's agent-written writes; a run wrote when it moved. */
	writeCount(): number {
		return this.#writes;
	}

	ageMs(nowMs: number): number | undefined {
		return this.#writtenAtMs === undefined
			? undefined
			: nowMs - this.#writtenAtMs;
	}

	focusTaskKey(): string {
		return this.#focusTaskKey;
	}
}

export function boardError(
	result: CommsCallResult,
	toolName: string,
	expected: string,
): Error {
	const outcome = result.result;
	if (outcome.case === "error") {
		const detail = flat(outcome.value.message).slice(0, 500);
		return new Error(
			`${toolName} failed: ${attr(outcome.value.code)}: ${detail}`,
		);
	}
	return new Error(
		`${toolName}: protocol violation — expected a ${expected} result, got ${outcome.case ?? "none"}`,
	);
}

export function boardOf(result: CommsCallResult, toolName: string): AgentBoard {
	if (result.result.case !== "updateBoard")
		throw boardError(result, toolName, "update_board");
	const board = result.result.value.board;
	if (board === undefined)
		throw new Error(
			`${toolName}: protocol violation — update result carried no board`,
		);
	return board;
}

export const taskKeysOf = (board: AgentBoard): string[] =>
	board.tasks.map((t) => t.key);

// An automatic patch with no fields reads the board without changing it.
export function readBoardRequest(
	callId: string,
	turnSequence: TurnSequence,
): CommsCallRequest {
	return create(CommsCallRequestSchema, {
		callId: `${callId}-${crypto.randomUUID()}`,
		call: {
			case: "updateBoard",
			value: create(UpdateAgentBoardRequestSchema, {
				turnSequence: turnSequence.current(),
				automatic: true,
			}),
		},
	});
}

const SEED_DEADLINE_MS = 10_000;

/** Seeds freshness from the server board with an empty automatic patch. */
export async function seedBoardFreshness(
	broker: CommsBroker,
	turnSequence: TurnSequence,
	freshness: BoardFreshness,
	timer: BatchTimer = realBatchTimer,
): Promise<void> {
	// A reply after the deadline still seeds; the deadline only frees the waiters.
	const call = broker
		.call(readBoardRequest("board-freshness-seed", turnSequence))
		.then((result) => {
			const board = boardOf(result, "board freshness seed");
			freshness.seed(
				board.writtenTurn,
				Number(board.writtenAtUnixMs),
				board.focusTaskKey,
				taskKeysOf(board),
			);
		});
	call.catch(() => {});
	// The unary has no deadline of its own; a hung read must not hold nudges and sync.
	let cancel = (): void => {};
	const deadline = new Promise<never>((_, reject) => {
		cancel = timer.set(SEED_DEADLINE_MS, () =>
			reject(new Error("board freshness seed: no reply within 10 s")),
		);
	});
	try {
		await Promise.race([call, deadline]);
	} finally {
		cancel();
	}
}
