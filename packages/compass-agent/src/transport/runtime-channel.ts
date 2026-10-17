// The module-private channel that threads the transport-owned ManagedRuntime from
// createUnixSocketTransport into its sibling factories (frame-sink, control-source) without
// putting an `effect` type on any exported signature (design compass-agent-effect-adoption
// Global Constraints; §T5: threaded through a module-private channel, never a factory param).

// A module-private WeakMap keyed by the transport instance, the tightest of the record's
// three options: `RunnerTransport` and the object literal stay untouched (no `.d.ts` trace),
// the map is unreachable outside this file, and GC of a transport reclaims its entry. A fake
// transport is never a key, so the borrowing factory falls back to its own default runtime.

import { Logger, type ManagedRuntime } from "effect";
import type { RunnerTransport } from "./index";

// The single runtime type the transport constructs and siblings borrow. It
// retains tracer logging but replaces the console loggers in the default set.
export type TransportRuntime = ManagedRuntime.ManagedRuntime<never, never>;

// Every transport runtime's logger set: log lines become span events, never console output.
export const transportLoggerLayer = Logger.layer([Logger.tracerLogger]);

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
