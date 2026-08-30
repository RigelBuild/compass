// The AsyncBuffer single-consumer contract (RIG-1364 C4b follow-up hardening).
//
// AsyncBuffer is the queue behind the socket ControlSource's iterable, and it
// holds AT MOST ONE parked consumer (`#waiting`). Before the guard, a second
// concurrent `pull()` overwrote that record and abandoned the first promise
// forever: no error, no rejection, just an agent wedged on a control op that
// would never arrive. The class is exported now, so the invariant is no longer
// protected by being file-local — it has to be enforced and defended.
//
// These are direct unit tests rather than more socket-level ControlSource
// cases: the violation is a SECOND consumer, and the source deliberately only
// ever has one, so nothing driven through the live server can reach this path.
//
// No timers anywhere (ts-no-test-timers): every settlement here is either
// synchronous inside `pull()`'s executor or driven by an explicit
// push/close/fail on the same tick, so draining the microtask queue observes
// it exactly. That is what lets a pull that WEDGES (the pre-guard bug) fail a
// named assertion instead of hanging the suite until timeout.

import { expect, test } from "bun:test";
import { AsyncBuffer, type Queued } from "./buffer";

function queued(seq: bigint, input: string): Queued {
	return { op: { kind: "prompt", input }, seq };
}

type Settled<T> =
	| { state: "pending" }
	| { state: "fulfilled"; value: T }
	| { state: "rejected"; reason: unknown };

// Observe a promise's settlement WITHOUT awaiting it, so a promise that never
// settles is a value this test can assert on rather than a hang. Draining a
// handful of microtask turns is exact here (see the file header): nothing on
// these paths defers past the microtask queue. Attaching the handler also means
// a rejection under test is never an unhandled rejection.
async function settled<T>(p: Promise<T>): Promise<Settled<T>> {
	let out: Settled<T> = { state: "pending" };
	void p.then(
		(value) => {
			out = { state: "fulfilled", value };
		},
		(reason: unknown) => {
			out = { state: "rejected", reason };
		},
	);
	for (let i = 0; i < 8; i++) await Promise.resolve();
	return out;
}

// The rejection reason, or a description of what happened INSTEAD — so a pull
// that fulfilled or (the regression) parked forever reds with a readable value
// rather than an undefined dereference.
function reasonOf(s: Settled<unknown>): unknown {
	return s.state === "rejected" ? s.reason : `did not reject (${s.state})`;
}

test("a second concurrent pull() is refused BY NAME — it rejects as single-consumer instead of silently displacing the first", async () => {
	// Non-vacuity: restore the plain `this.#waiting = { resolve, reject }` body
	// (drop the guard) → the second pull parks instead of rejecting, `settled`
	// reports "pending", and the message assertion reds.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	const second = buf.pull();
	expect(String(reasonOf(await settled(second)))).toContain("single-consumer");
	// The refusal is the SECOND caller's alone: the first is still a live,
	// unsettled pull, not collateral damage.
	expect(await settled(first)).toEqual({ state: "pending" });
});

test("the refused second pull() does NOT abandon the first — a later push() still resolves the FIRST pull with that value", async () => {
	// The contract that actually matters. The pre-guard bug was not a bad error
	// message, it was a permanent hang: the displaced first promise had no
	// remaining route to settle, so the control loop awaiting it stopped forever.
	// Non-vacuity: drop the guard → the second pull overwrites `#waiting`, the
	// push resolves the SECOND promise, and `first` is still "pending" here → red.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	const second = buf.pull();
	expect(String(reasonOf(await settled(second)))).toContain("single-consumer");
	buf.push(queued(7n, "hello"));
	expect(await settled(first)).toEqual({
		state: "fulfilled",
		value: { value: queued(7n, "hello"), done: false },
	});
});

test("the real single-consumer path is untouched: sequential pull()/push() resolve in order, twice in a row", async () => {
	// The negative control — proof the guard does not over-fire. `#waiting` is
	// cleared by the handoff, so the NEXT pull is a first pull, not a second one.
	// Non-vacuity: make the guard unconditional (`if (true) reject(...)`) or have
	// push() leave `#waiting` set → the second sequential pull rejects → red.
	const buf = new AsyncBuffer();
	const one = buf.pull();
	buf.push(queued(1n, "one"));
	expect(await settled(one)).toEqual({
		state: "fulfilled",
		value: { value: queued(1n, "one"), done: false },
	});
	const two = buf.pull();
	buf.push(queued(2n, "two"));
	expect(await settled(two)).toEqual({
		state: "fulfilled",
		value: { value: queued(2n, "two"), done: false },
	});
});

test("close() after a refused concurrent pull() still ends the FIRST parked pull with done: true", async () => {
	// The guard must leave `#waiting` holding the FIRST caller's record, reachable
	// by every terminal path — not just by push(). A clean Runner close reaching
	// the wrong (or a displaced) record is the same hang wearing a different hat.
	// Non-vacuity: drop the guard → close() resolves the second, displaced record
	// and `first` stays "pending" → red.
	const buf = new AsyncBuffer();
	const first = buf.pull();
	expect(String(reasonOf(await settled(buf.pull())))).toContain(
		"single-consumer",
	);
	buf.close();
	expect(await settled(first)).toEqual({
		state: "fulfilled",
		value: { value: undefined, done: true },
	});
});

test("fail(err) after a refused concurrent pull() still rejects the FIRST parked pull with that same error", async () => {
	// The drop path (reconnect budget spent) must surface the REAL cause to the
	// parked consumer, by identity — not the guard's error, and not a hang.
	// Non-vacuity: drop the guard → fail() rejects the second, displaced record
	// and `first` stays "pending", so the identity assertion reds.
	const boom = new Error("reconnect budget spent");
	const buf = new AsyncBuffer();
	const first = buf.pull();
	expect(String(reasonOf(await settled(buf.pull())))).toContain(
		"single-consumer",
	);
	buf.fail(boom);
	expect(reasonOf(await settled(first))).toBe(boom);
});
