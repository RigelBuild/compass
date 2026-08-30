// The tee seam (RIG-1570 T2): a container-local-filesystem `SessionStorageBackend`
// (indexed-session-storage.ts:25-36 — the ten async methods) that TEES every
// committed write upstream as a durable `TranscriptEntry` frame while keeping the
// authoritative bytes on the local disk the SDK's own session loader reads.
//
// Wrapped in the SDK's `IndexedSessionStorage` and injected at the `cli.ts`
// composition root (`SessionManager.create(cwd, sessionDir, storage)`), so the
// SDK drives writes through this backend exactly as it drives the file backend:
//   - `append(path, line)`  → local append + one DELTA frame (checkpoint: false)
//   - `writeFull(path, …)`  → local atomic write + one CHECKPOINT frame
//     (checkpoint: true) — EVERY SDK full-body rewrite funnels here
//     (session-manager.ts #rewriteAtomically → writeTextAtomic → writeFull), so
//     a checkpoint ALWAYS means "supersedes all prior entries" and a delta
//     ALWAYS rides append.
//   - reads (`readFull`/`readSlices`/`loadIndex`) → REAL local FS: load-bearing
//     for resume, since `setSessionFile` reads the resume file back through the
//     wrapper (session-manager.ts:973-974) and the wrapper throws ENOENT for
//     un-indexed paths, so the Runner-materialized file must appear in the scan.
//     loadIndex therefore ALSO indexes an explicit `resumeFile` (options,
//     RIG-1570 T2 Option B) by its exact absolute path, so a resume file that
//     lives OUTSIDE `sessionDir` is discoverable and need not live under it.
//   - the rest (`updateSessionTitle`/`truncate`/`move`/`remove`) → local-only
//     (titles are a Server-side rendering concern; nothing durable depends on
//     them). Safe because no SDK write path invokes `truncate`/`move`/`remove`
//     on the ACTIVE session file — the active-file write path is exclusively
//     `append` + `writeFull` (`#rewriteAtomically`), verified vs
//     session-manager.ts; if a future SDK compacts/relocates the active session
//     via these, they would have to tee.
//
// ORDERING (design R-Ordering). The backend AWAITS each frame's durable send
// inside the storage op the SDK's per-path tail chain serializes
// (indexed-session-storage.ts:418-433) — so per-session emit order == send order
// == commit order. An internal FIFO chain (#emitChain) serializes the upstream
// sends themselves as belt-and-suspenders, so a buffered-then-retried frame can
// never overtake a later one.
//
// ERRED-EMIT (design R4). The transcript lane does NOT inherit the sink's silent
// give-up: a definitively-erred `emitDurable` (the sink's own bounded transport
// retry, frame-sink.ts DURABLE_RETRY_BACKOFF_MS, already exhausted) enters the
// backend's OWN bounded buffer and keeps retrying the erred transcript send on
// an escalating warn → error schedule; at cap exhaustion the backend latches a
// FATAL session error (surfaced loudly on stderr, mirroring the SDK's
// `#diskFailure` fatal-by-design latch, session-manager.ts:674) that every
// subsequent op re-throws, so the session stops rather than silently dropping
// durable state. This is a production durable-write resilience tier (fails
// closed + loud at the cap), not test-flake papering — cap is a tuning const,
// not freeze-scope (design.md:777).

import { promises as fs } from "node:fs";
import path from "node:path";
import {
	IndexedSessionStorage,
	type SessionStorageBackend,
	type SessionStorageIndexEntry,
} from "@oh-my-pi/pi-coding-agent";
import { serializeTitleSlot } from "@oh-my-pi/pi-coding-agent/session/session-title-slot";
import {
	create,
	type TranscriptEntry,
	TranscriptEntrySchema,
} from "./compassv1";
import type { FrameSink, OutboundFrame } from "./frame";

// The semantic title-update shape the backend's write vectors carry, taken from
// the SDK interface itself (not re-declared) so it can never drift.
type TitleUpdate = Parameters<SessionStorageBackend["writeFull"]>[3];

// Bounded, escalating backoff for the backend's own transcript-send retry (ms).
// The sink already retries transient unary errors to its own cap
// (frame-sink.ts DURABLE_RETRY_BACKOFF_MS) and only REJECTS on definitive
// give-up; this is the SECOND, coarser tier R4 mandates — after the last delay
// the send is treated as unrecoverable and the session fails closed. Cap ==
// length. Tuning, not frozen (design.md:777).
export const TEE_EMIT_BACKOFF_MS: readonly number[] = [100, 500, 2000, 5000];

/** Tuning knobs; all optional so the frozen `(sink, sessionDir)` shape holds. */
export interface TranscriptTeeOptions {
	/** Override the escalating retry schedule (tests inject a fast one). */
	readonly emitBackoffMs?: readonly number[];
	/** An extra absolute session-file path to index beyond the scanned session dir
	    (the Runner-materialized resume file; RIG-1570 T8/T2). Canonicalized with
	    `path.resolve` and indexed under that resolved key so the SDK wrapper's
	    lookup gate (statSync ~:177 / readText ~:205 / readTextSlices ~:213 in
	    indexed-session-storage.ts, all keyed on `setSessionFile`'s
	    `path.resolve(sessionFile)`, session-manager.ts:969) does not ENOENT a
	    resume file that lives outside `sessionDir` or is passed non-canonically. */
	readonly resumeFile?: string;
}

function enoent(p: string): NodeJS.ErrnoException {
	const err = new Error(
		`ENOENT: no such file, '${p}'`,
	) as NodeJS.ErrnoException;
	err.code = "ENOENT";
	err.errno = -2;
	err.path = p;
	err.syscall = "open";
	return err;
}

function isEnoent(err: unknown): boolean {
	return (err as NodeJS.ErrnoException | undefined)?.code === "ENOENT";
}

/**
 * Container-local FS backend that tees every committed write upstream as a
 * durable `TranscriptEntry` frame. Reads are real local reads (the SDK's own
 * loader serves resume); a definitively-erred emit buffers + retries on an
 * escalating schedule and fails the session at the bounded cap (R4).
 */
export class TranscriptTeeBackend implements SessionStorageBackend {
	readonly #sink: FrameSink;
	readonly #sessionDir: string;
	// entry_seq is proto uint64 → bigint. Monotonic from 1 PER CONTAINER
	// LIFETIME (the agent has no durable memory across restarts); the Server
	// rebases it onto the session's stored maximum at bind (T4).
	#seq = 0n;
	// Serializes the upstream sends so per-session emit order == send order even
	// across the awaited storage ops, AND a buffered-then-retried frame can never
	// overtake a later frame (per-backend FIFO — R4 ordering).
	#emitChain: Promise<void> = Promise.resolve();
	// Fatal latch: once the retry cap is exhausted, every op re-throws this
	// (mirrors the SDK's #diskFailure fatal-by-design latch, session-manager.ts:674).
	#fatalError: Error | undefined;
	readonly #backoffMs: readonly number[];
	readonly #resumeFile: string | undefined;

	constructor(
		sink: FrameSink,
		sessionDir: string,
		options?: TranscriptTeeOptions,
	) {
		this.#sink = sink;
		this.#sessionDir = sessionDir;
		this.#backoffMs = options?.emitBackoffMs ?? TEE_EMIT_BACKOFF_MS;
		// Canonicalize once at the trust boundary: the SDK looks the resume file
		// up under `path.resolve(sessionFile)` (session-manager.ts:969), and the
		// index is an exact-string Map, so the index key MUST be the resolved
		// path or a non-canonical-but-absolute env value silently misses the gate.
		this.#resumeFile =
			options?.resumeFile !== undefined
				? path.resolve(options.resumeFile)
				: undefined;
	}

	#nextFrame(entryJson: string, checkpoint: boolean): OutboundFrame {
		this.#seq += 1n;
		const value: TranscriptEntry = create(TranscriptEntrySchema, {
			entryJson,
			checkpoint,
			entrySeq: this.#seq,
		});
		return { kind: "transcriptEntry", value };
	}

	#throwIfFatal(): void {
		if (this.#fatalError) throw this.#fatalError;
	}

	// Deliver one transcript frame upstream, awaited inside the op. Serialized
	// through #emitChain (FIFO) so per-session emit order == send order and a
	// buffered-then-retried frame can never overtake a later one; erred sends
	// buffer + retry inside #deliver, latching fatal at the cap (R4).
	#tee(frame: OutboundFrame): Promise<void> {
		const run = this.#emitChain.then(() => this.#deliver(frame));
		// Keep the chain alive past a rejection so ordering survives an error —
		// the next op still queues behind this one's completion (or fatal latch).
		this.#emitChain = run.then(
			() => {},
			() => {},
		);
		return run;
	}

	async #deliver(frame: OutboundFrame): Promise<void> {
		this.#throwIfFatal();
		const seq = frame.kind === "transcriptEntry" ? frame.value.entrySeq : 0n;
		for (let attempt = 0; ; attempt++) {
			try {
				await this.#sink.emitDurable(frame);
				return;
			} catch (err) {
				if (attempt >= this.#backoffMs.length) {
					// Cap exhausted: a durable transcript write cannot be delivered.
					// Latch fatal (mirrors the SDK #diskFailure latch) so this op and
					// every later one re-throw — the session fails closed rather than
					// silently losing durable state.
					const fatal =
						err instanceof Error
							? err
							: new Error(
									`transcript tee gave up delivering entry_seq=${seq}: ${String(err)}`,
								);
					this.#fatalError = fatal;
					console.error(
						`[compass-agent] transcript tee FATAL: entry_seq=${seq} undeliverable after ${this.#backoffMs.length} retries — failing session`,
						fatal,
					);
					throw fatal;
				}
				// Escalate: warn on the earlier attempts, error on the last one
				// before the cap, so an operator sees the severity climb.
				const level =
					attempt >= this.#backoffMs.length - 1 ? console.error : console.warn;
				level(
					`[compass-agent] transcript tee send failed (entry_seq=${seq}, attempt ${attempt + 1}/${this.#backoffMs.length}), retrying`,
					err,
				);
				const { promise, resolve } = Promise.withResolvers<void>();
				// biome-ignore lint/style/noRestrictedGlobals: production retry backoff between transcript-tee send attempts; not a test wait
				setTimeout(resolve, this.#backoffMs[attempt]);
				await promise;
			}
		}
	}

	// ── SessionStorageBackend ─────────────────────────────────────────────────

	async init(): Promise<void> {
		// Called by IndexedSessionStorage.initialize() BEFORE any read; make the
		// local session dir exist so loadIndex can scan it (empty → empty index).
		await fs.mkdir(this.#sessionDir, { recursive: true });
	}

	async loadIndex(): Promise<SessionStorageIndexEntry[]> {
		let names: string[];
		try {
			names = await fs.readdir(this.#sessionDir);
		} catch (err) {
			if (isEnoent(err)) names = [];
			else throw err;
		}
		const out: SessionStorageIndexEntry[] = [];
		for (const name of names) {
			if (!name.endsWith(".jsonl")) continue;
			const full = path.join(this.#sessionDir, name);
			try {
				const stat = await fs.stat(full);
				if (!stat.isFile()) continue;
				out.push({ path: full, size: stat.size, mtimeMs: stat.mtimeMs });
			} catch (err) {
				if (isEnoent(err)) continue;
				throw err;
			}
		}
		// RIG-1570 T2 (Option B): also index the explicit Runner-materialized
		// resume file, which lives OUTSIDE the scanned session dir. Dedup by exact
		// path (a resume file that happens to live in sessionDir is already
		// listed). A not-yet-materialized resume file (ENOENT) is a valid fresh
		// start → skip silently.
		if (this.#resumeFile && !out.some((e) => e.path === this.#resumeFile)) {
			try {
				const stat = await fs.stat(this.#resumeFile);
				if (stat.isFile()) {
					out.push({
						path: this.#resumeFile,
						size: stat.size,
						mtimeMs: stat.mtimeMs,
					});
				}
			} catch (err) {
				if (!isEnoent(err)) throw err;
			}
		}
		return out;
	}

	async readFull(filePath: string): Promise<string | null> {
		try {
			return await fs.readFile(filePath, "utf-8");
		} catch (err) {
			if (isEnoent(err)) return null;
			throw err;
		}
	}

	async readSlices(
		filePath: string,
		prefixBytes: number,
		suffixBytes: number,
	): Promise<[string, string]> {
		let handle: fs.FileHandle;
		try {
			handle = await fs.open(filePath, "r");
		} catch (err) {
			if (isEnoent(err)) throw enoent(filePath);
			throw err;
		}
		try {
			const { size } = await handle.stat();
			const headLen = prefixBytes > 0 ? Math.min(prefixBytes, size) : 0;
			const tailLen = suffixBytes > 0 ? Math.min(suffixBytes, size) : 0;
			const head = headLen > 0 ? Buffer.allocUnsafe(headLen) : Buffer.alloc(0);
			// Slice to bytesRead so a short read never surfaces uninitialized heap
			// (allocUnsafe) or trailing zeros — only the bytes actually read decode.
			// Belt-and-suspenders: headLen/tailLen are clamped to the file size, so
			// a regular-file read of an in-bounds range always fully populates the
			// buffer (bytesRead === requested); there is no deterministic short-read
			// to test here, which is why no test exercises these bytesRead branches.
			const headBytes =
				headLen > 0 ? (await handle.read(head, 0, headLen, 0)).bytesRead : 0;
			const headStr = head.subarray(0, headBytes).toString("utf-8");
			if (tailLen <= 0) return [headStr, ""];
			if (size <= headLen) {
				return [
					headStr,
					head
						.subarray(Math.max(0, headBytes - tailLen), headBytes)
						.toString("utf-8"),
				];
			}
			const tail = Buffer.allocUnsafe(tailLen);
			const tailBytes = (await handle.read(tail, 0, tailLen, size - tailLen))
				.bytesRead;
			return [headStr, tail.subarray(0, tailBytes).toString("utf-8")];
		} finally {
			await handle.close();
		}
	}

	async writeFull(
		filePath: string,
		content: string,
		_mtimeMs: number,
		_title?: TitleUpdate,
	): Promise<void> {
		this.#throwIfFatal();
		// Local atomic write (temp + rename), then the CHECKPOINT frame — content
		// is the full body verbatim (title slot + header + entries), so the
		// upstream store supersedes every prior entry on it (T4).
		const dir = path.dirname(filePath);
		await fs.mkdir(dir, { recursive: true });
		const tempPath = path.join(
			dir,
			`.${path.basename(filePath)}.${process.pid}.${this.#seq}.tmp`,
		);
		// Temp-name uniqueness rests on PATH-uniqueness: the SDK wrapper serializes
		// same-path ops (indexed-session-storage.ts tail chain), so no two writeFull
		// on this path overlap. NOT on `#seq` — it is read here BEFORE `#nextFrame`
		// increments it, so it is stale and shared, never a per-op discriminator.
		try {
			await fs.writeFile(tempPath, content);
			await fs.rename(tempPath, filePath);
		} catch (err) {
			await fs.rm(tempPath, { force: true });
			throw err;
		}
		await this.#tee(this.#nextFrame(content, true));
	}

	async append(
		filePath: string,
		line: string,
		_mtimeMs: number,
	): Promise<void> {
		this.#throwIfFatal();
		// Local append (creates the file on first write), then the DELTA frame —
		// the line verbatim (its trailing newline included) so concatenation
		// reconstructs the JSONL body exactly (T5).
		await fs.appendFile(filePath, line);
		await this.#tee(this.#nextFrame(line, false));
	}

	async updateSessionTitle(
		filePath: string,
		title: NonNullable<TitleUpdate>,
		_mtimeMs: number,
	): Promise<void> {
		this.#throwIfFatal();
		// Local-only: overwrite the fixed-width title slot in place (mirrors
		// FileSessionStorage.updateSessionTitle, session-storage.ts:196-213). No
		// upstream frame — titles are a Server-side rendering concern.
		const handle = await fs.open(filePath, "r+");
		try {
			// Loop on bytesWritten so a short write can never leave the fixed-width
			// slot partially overwritten (faithful to the SDK mirror, which loops).
			const buf = Buffer.from(serializeTitleSlot(title), "utf-8");
			let offset = 0;
			while (offset < buf.length) {
				const { bytesWritten } = await handle.write(
					buf,
					offset,
					buf.length - offset,
					offset,
				);
				if (bytesWritten === 0) {
					throw new Error("compass-agent: session title slot short write");
				}
				offset += bytesWritten;
			}
		} finally {
			await handle.close();
		}
	}

	async truncate(filePath: string, _mtimeMs: number): Promise<void> {
		this.#throwIfFatal();
		await fs.writeFile(filePath, "");
	}

	async remove(paths: string[]): Promise<void> {
		this.#throwIfFatal();
		for (const p of paths) await fs.rm(p, { force: true });
	}

	async move(src: string, dst: string, _mtimeMs: number): Promise<void> {
		this.#throwIfFatal();
		await fs.mkdir(path.dirname(dst), { recursive: true });
		await fs.rm(dst, { force: true });
		await fs.rename(src, dst);
	}
}

/**
 * Build the tee backend, wrap it in the SDK adapter, and `initialize()` it —
 * ready to inject at `SessionManager.create(cwd, sessionDir, storage)`.
 *
 * Mirrors `RedisSessionStorage.create` (redis-session-storage.ts:84-88):
 * `initialize()` MUST be awaited BEFORE `SessionManager.create` so synchronous
 * lookups (resume, recent sessions) see the existing keyspace.
 */
export async function createTeeSessionStorage(
	sink: FrameSink,
	sessionDir: string,
	options?: TranscriptTeeOptions,
): Promise<{ storage: IndexedSessionStorage; backend: TranscriptTeeBackend }> {
	const backend = new TranscriptTeeBackend(sink, sessionDir, options);
	const storage = new IndexedSessionStorage(backend);
	await storage.initialize();
	return { storage, backend };
}
