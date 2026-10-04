import type { SessionEntry, SessionManager } from "@oh-my-pi/pi-coding-agent";

const ENTRY_TYPE = "compass_turn_sequence";
const MAX_SEQUENCE = 9_223_372_036_854_775_807n;

function storedSequence(entry: SessionEntry): bigint | undefined {
	if (entry.type !== "custom" || entry.customType !== ENTRY_TYPE)
		return undefined;
	const data: unknown = entry.data;
	if (typeof data !== "object" || data === null) return undefined;
	const value = Reflect.get(data, "turnSequence");
	if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value))
		return undefined;
	const sequence = BigInt(value);
	return sequence <= MAX_SEQUENCE ? sequence : undefined;
}

/** One session-identity's monotonic turn sequence, restored from its transcript. */
export class TurnSequence {
	readonly #manager: SessionManager;
	#identity: string;
	#current: bigint;

	constructor(manager: SessionManager) {
		this.#manager = manager;
		this.#identity = manager.getSessionId();
		this.#current = this.#restore();
	}

	#restore(): bigint {
		return this.#manager
			.getEntries()
			.flatMap((entry) => {
				const sequence = storedSequence(entry);
				return sequence === undefined ? [] : [sequence];
			})
			.reduce(
				(highest, sequence) => (sequence > highest ? sequence : highest),
				0n,
			);
	}

	current(): bigint {
		return this.#current;
	}

	start(): bigint {
		if (this.#manager.getSessionId() !== this.#identity) {
			this.#identity = this.#manager.getSessionId();
			this.#current = this.#restore();
		}
		if (this.#current === MAX_SEQUENCE) {
			throw new Error("compass-agent: turn sequence exceeds int64 maximum");
		}
		this.#current += 1n;
		this.#manager.appendCustomEntry(ENTRY_TYPE, {
			turnSequence: this.#current.toString(),
		});
		return this.#current;
	}
}
