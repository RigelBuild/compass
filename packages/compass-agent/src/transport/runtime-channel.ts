// The module-private channel that threads the transport-owned ManagedRuntime
// from createUnixSocketTransport into its sibling factories (frame-sink,
// control-source) without putting an `effect` type on any exported signature
// (design docs/designs/repo/compass-agent-effect-adoption/design.md, Global
// Constraints: no `Effect<>`/`ManagedRuntime`/`Runtime` in a signature exported
// from src/transport/; §T5: the runtime is threaded through a module-private
// channel, never an exported factory parameter).
//
// Mechanism = a module-private WeakMap keyed by the transport instance, chosen
// over the two alternatives in the record (a Symbol-keyed non-enumerable
// property on the returned object; an internal-only interface the sibling
// factories narrow to). The WeakMap is strictly the tightest of the three here:
//   - `RunnerTransport` and the concrete object literal are untouched — no extra
//     property, no cast at the read site, so the public shape and its `.d.ts`
//     carry no trace of the channel or of `effect`.
//   - The map is unreachable outside this file, so the channel cannot be read or
//     written from anywhere but the src/transport/ modules — "private to the
//     module boundary" is enforced by scope, not convention.
//   - A fake transport (the unit-test carriers over `spineTransport`/`spySpine`
//     and the in-process carriers in cli.test.ts) is simply never a key, so
//     `getTransportRuntime` returns undefined and the borrowing factory falls
//     back to its own default runtime — exactly the owned-vs-borrowed split §T5
//     requires, with no sentinel to thread.
//   - GC of a transport reclaims its map entry, so no runtime is retained past
//     the transport that owns it.

import type { ManagedRuntime } from "effect";
import type { RunnerTransport } from "./index";

// The single runtime type the transport constructs and the siblings borrow:
// `ManagedRuntime.make(Logger.remove(Logger.defaultLogger))` yields a runtime
// with no residual requirements and no construction error
// (`Logger.remove` is a `Layer.Layer<never>`).
export type TransportRuntime = ManagedRuntime.ManagedRuntime<never, never>;

const channel = new WeakMap<RunnerTransport, TransportRuntime>();

// Record the transport-owned runtime against its transport. Called once by
// createUnixSocketTransport; the transport's close() owns the runtime's disposal.
export function setTransportRuntime(
	transport: RunnerTransport,
	runtime: TransportRuntime,
): void {
	channel.set(transport, runtime);
}

// Read the transport-owned runtime a sibling factory should BORROW. Absent (a
// fake transport) => the factory makes and owns its own default runtime instead.
export function getTransportRuntime(
	transport: RunnerTransport,
): TransportRuntime | undefined {
	return channel.get(transport);
}
