import { promises as fs } from "node:fs";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
// biome-ignore lint/style/noRestrictedImports: classify the status returned by the Runner transport
import { Code, ConnectError } from "@connectrpc/connect";
import {
	type PutSessionBlobRequest,
	PutSessionBlobRequestSchema,
} from "./gen/compass/v1/agent_gateway_pb";
import {
	sessionBlobsDropped,
	sessionBlobsFailed,
	sessionBlobsMissing,
	sessionBlobsOversize,
	sessionBlobsUploaded,
} from "./transport/otel-metrics";

export const MAX_SESSION_BLOB_BYTES = 15 * 1024 * 1024;
export const SESSION_BLOB_QUEUE_MAX = 16;

export interface SessionBlobUploader {
	offer(line: string): void;
	close(): Promise<void>;
}

interface SessionBlobUploaderDeps {
	readonly put: (req: PutSessionBlobRequest) => Promise<void>;
	readonly blobsDir: string;
}

const BLOB_REF_RE = /blob:sha256:([a-f0-9]{64})/g;

function increment(
	metric:
		| typeof sessionBlobsDropped
		| typeof sessionBlobsOversize
		| typeof sessionBlobsMissing
		| typeof sessionBlobsFailed
		| typeof sessionBlobsUploaded,
): void {
	metric.unsafeUpdate(1, []);
}

function isEnoent(err: unknown): boolean {
	return (
		typeof err === "object" &&
		err !== null &&
		"code" in err &&
		err.code === "ENOENT"
	);
}

export function createSessionBlobUploader(
	deps: SessionBlobUploaderDeps,
): SessionBlobUploader {
	const queue: string[] = [];
	const queued = new Set<string>();
	const done = new Set<string>();
	let worker: Promise<void> | undefined;
	let disabled = false;
	let closed = false;

	function startWorker(): void {
		if (worker !== undefined || disabled || queue.length === 0) return;
		worker = drainQueue().finally(() => {
			worker = undefined;
			if (queue.length > 0 && !disabled) startWorker();
		});
	}

	async function uploadBlob(sha256: string): Promise<void> {
		try {
			const filePath = join(deps.blobsDir, sha256);
			const info = await fs.stat(filePath);
			if (info.size > MAX_SESSION_BLOB_BYTES) {
				done.add(sha256);
				increment(sessionBlobsOversize);
				return;
			}
			const data = await fs.readFile(filePath);
			if (data.byteLength > MAX_SESSION_BLOB_BYTES) {
				done.add(sha256);
				increment(sessionBlobsOversize);
				return;
			}
			await deps.put(create(PutSessionBlobRequestSchema, { sha256, data }));
			done.add(sha256);
			increment(sessionBlobsUploaded);
		} catch (err) {
			if (isEnoent(err)) {
				increment(sessionBlobsMissing);
			} else if (ConnectError.from(err).code === Code.FailedPrecondition) {
				done.add(sha256);
				disabled = true;
				queue.length = 0;
				queued.clear();
				increment(sessionBlobsFailed);
			} else {
				increment(sessionBlobsFailed);
			}
		} finally {
			queued.delete(sha256);
		}
	}

	async function drainQueue(): Promise<void> {
		while (queue.length > 0 && !disabled) {
			const sha256 = queue.shift();
			if (sha256 !== undefined) await uploadBlob(sha256);
		}
	}
	function enqueueLine(line: string): void {
		BLOB_REF_RE.lastIndex = 0;
		for (const match of line.matchAll(BLOB_REF_RE)) {
			const sha256 = match[1];
			if (sha256 === undefined || done.has(sha256) || queued.has(sha256)) {
				continue;
			}
			if (queue.length === SESSION_BLOB_QUEUE_MAX) {
				const dropped = queue.shift();
				if (dropped !== undefined) {
					queued.delete(dropped);
					increment(sessionBlobsDropped);
				}
			}
			queue.push(sha256);
			queued.add(sha256);
		}
	}

	return {
		offer(line: string): void {
			if (closed || disabled) return;
			enqueueLine(line);
			startWorker();
		},
		async close(): Promise<void> {
			closed = true;
			queue.length = 0;
			queued.clear();
			while (worker !== undefined) await worker;
		},
	};
}
