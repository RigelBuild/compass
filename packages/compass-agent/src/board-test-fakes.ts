import { SessionManager } from "@oh-my-pi/pi-coding-agent";
import type { BatchTimer } from "./batch-window";
import type { CommsTransport } from "./comms";
import {
	type AgentBoard,
	AgentBoardSchema,
	type BoardTaskPatch,
	BoardTaskSchema,
	CommsCallErrorSchema,
	type CommsCallRequest,
	type CommsCallResult,
	CommsCallResultSchema,
	create,
	ListAgentBoardsResponseSchema,
	UpdateAgentBoardResponseSchema,
} from "./compassv1";
import { TurnSequence } from "./turn-sequence";

const refusal = (callId: string, message: string): CommsCallResult =>
	create(CommsCallResultSchema, {
		callId,
		result: {
			case: "error",
			value: create(CommsCallErrorSchema, {
				code: "invalid_argument",
				message,
			}),
		},
	});

/** A comms transport over one stored board; updates apply like the server's. */
export class FakeTransport implements CommsTransport {
	readonly requests: CommsCallRequest[] = [];
	failNext = false;
	refuseNext = false;
	/** Refuses this many patches that carry tasks; reads still answer. */
	refuseWrites = 0;
	#gate: Promise<void> | undefined;
	constructor(readonly board: AgentBoard) {}
	set focus(key: string) {
		this.board.focusTaskKey = key;
	}
	addTask(key: string, title = key): void {
		this.board.tasks.push(create(BoardTaskSchema, { key, title }));
	}
	removeTask(key: string): void {
		this.board.tasks = this.board.tasks.filter((t) => t.key !== key);
		if (this.board.focusTaskKey === key) this.board.focusTaskKey = "";
	}
	/** Holds every reply until the returned release runs. */
	hold(): () => void {
		let release = (): void => {};
		this.#gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		return release;
	}
	/** Boards a list call returns; defaults to the stored board. */
	listBoards: AgentBoard[] | undefined;
	async comms(req: CommsCallRequest): Promise<CommsCallResult> {
		this.requests.push(req);
		await this.#gate;
		if (this.failNext) {
			this.failNext = false;
			throw new Error("transport down");
		}
		if (this.refuseNext) {
			this.refuseNext = false;
			return refusal(req.callId, "task cap");
		}
		if (req.call.case !== "updateBoard")
			return create(CommsCallResultSchema, {
				callId: req.callId,
				result: {
					case: "boards",
					value: create(ListAgentBoardsResponseSchema, {
						boards: this.listBoards ?? [this.board],
					}),
				},
			});
		if (req.call.value.upsertTasks.length > 0 && this.refuseWrites > 0) {
			this.refuseWrites -= 1;
			return refusal(req.callId, "task cap");
		}
		const patches = req.call.value.upsertTasks;
		const untitled = patches.find(
			(p) =>
				p.title === undefined && !this.board.tasks.some((t) => t.key === p.key),
		);
		if (untitled !== undefined)
			return refusal(
				req.callId,
				`task ${untitled.key}: a new task needs a title`,
			);
		for (const patch of patches) this.#apply(patch);
		return create(CommsCallResultSchema, {
			callId: req.callId,
			result: {
				case: "updateBoard",
				value: create(UpdateAgentBoardResponseSchema, { board: this.board }),
			},
		});
	}
	// The server keeps 20 rows per task, dropping the oldest settled first.
	#apply(patch: BoardTaskPatch): void {
		let task = this.board.tasks.find((t) => t.key === patch.key);
		if (task === undefined) {
			task = create(BoardTaskSchema, { key: patch.key });
			this.board.tasks.push(task);
		}
		if (patch.title !== undefined) task.title = patch.title;
		for (const s of patch.upsertSubagents) {
			task.subagents = [...task.subagents.filter((o) => o.id !== s.id), s];
			const settled = task.subagents.findIndex((o) => o.endedAtUnixMs !== 0n);
			if (task.subagents.length > 20 && settled >= 0)
				task.subagents.splice(settled, 1);
		}
	}
}

export function board(
	overrides: {
		writtenTurn?: bigint;
		writtenAtUnixMs?: bigint;
		focusTaskKey?: string;
	} = {},
): AgentBoard {
	const focus = overrides.focusTaskKey ?? "focus";
	return create(AgentBoardSchema, {
		handle: "worker",
		focusTaskKey: focus,
		writtenTurn: overrides.writtenTurn ?? 0n,
		writtenAtUnixMs: overrides.writtenAtUnixMs ?? 1_000n,
		tasks:
			focus.length > 0
				? [create(BoardTaskSchema, { key: focus, title: focus })]
				: [],
	});
}

export function sequence(): TurnSequence {
	return new TurnSequence(SessionManager.inMemory());
}

export class FakeTimer implements BatchTimer {
	at = 10_000;
	#nextId = 0;
	readonly entries = new Map<number, { at: number; fire: () => void }>();
	now = (): number => this.at;
	set = (ms: number, fire: () => void): (() => void) => {
		const id = ++this.#nextId;
		this.entries.set(id, { at: this.at + ms, fire });
		return () => this.entries.delete(id);
	};
	advance(ms: number): void {
		this.at += ms;
		for (const [id, entry] of [...this.entries]) {
			if (entry.at > this.at) continue;
			this.entries.delete(id);
			entry.fire();
		}
	}
}

// Drains a chain of awaited calls in an in-flight sync.
export const settle = async (): Promise<void> => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};
