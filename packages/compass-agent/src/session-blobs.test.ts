import { afterEach, expect, setDefaultTimeout, spyOn, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { mkdir, truncate, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
// biome-ignore lint/style/noRestrictedImports: construct the Runner status error in this transport test
import { Code, ConnectError } from "@connectrpc/connect";
import { Effect, Metric } from "effect";
import {
	createSessionBlobUploader,
	MAX_SESSION_BLOB_BYTES,
	SESSION_BLOB_QUEUE_MAX,
	type SessionBlobUploader,
} from "./session-blobs";
import { TranscriptTeeBackend } from "./session-tee";
import {
	sessionBlobsDropped,
	sessionBlobsFailed,
	sessionBlobsMissing,
	sessionBlobsOversize,
	sessionBlobsUploaded,
} from "./transport/otel-metrics";

setDefaultTimeout(60_000);

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-session-blobs-"));
	tmpdirs.push(dir);
	return dir;
}

afterEach(() => {
	while (tmpdirs.length > 0) {
		const dir = tmpdirs.pop();
		if (dir) rmSync(dir, { recursive: true, force: true });
	}
}, 60_000);

async function writeBlob(
	dir: string,
	sha: string,
	data = Uint8Array.of(1),
): Promise<void> {
	await mkdir(dir, { recursive: true });
	await writeFile(join(dir, sha), data);
}

function hash(n: number): string {
	return n.toString(16).padStart(64, "0");
}

function line(...hashes: string[]): string {
	return JSON.stringify(hashes.map((sha) => `blob:sha256:${sha}`));
}

function counterCount(metric: Metric.Metric.Counter<number>): number {
	return Effect.runSync(Metric.value(metric)).count;
}

function waitForCounter(
	metric: Metric.Metric.Counter<number>,
	baseline: number,
	count: number,
): Promise<void> {
	if (counterCount(metric) - baseline >= count) return Promise.resolve();
	return new Promise((resolve) => {
		const poll = (): void => {
			if (counterCount(metric) - baseline >= count) resolve();
			else setImmediate(poll);
		};
		setImmediate(poll);
	});
}

test("an appended line and checkpoint body upload each hash once, serially", async () => {
	const dir = scratch();
	const blobsDir = join(dir, "blobs");
	const first = hash(1);
	const second = hash(2);
	await writeBlob(blobsDir, first, Uint8Array.of(11));
	await writeBlob(blobsDir, second, Uint8Array.of(22));
	const firstEntered = Promise.withResolvers<void>();
	const releaseFirst = Promise.withResolvers<void>();
	const secondEntered = Promise.withResolvers<void>();
	const sent: { sha256: string; byte: number }[] = [];
	let active = 0;
	let maxActive = 0;
	const uploader = createSessionBlobUploader({
		blobsDir,
		put: async (req) => {
			active += 1;
			maxActive = Math.max(maxActive, active);
			sent.push({ sha256: req.sha256, byte: req.data[0] ?? 0 });
			if (sent.length === 1) {
				firstEntered.resolve();
				await releaseFirst.promise;
			} else {
				secondEntered.resolve();
			}
			active -= 1;
		},
	});
	const sink = {
		emit: () => {},
		emitDurable: async () => {},
		drain: async () => {},
	};
	const backend = new TranscriptTeeBackend(sink, dir, {
		onCommittedLine: (entry) => uploader.offer(entry),
	});
	const sessionFile = join(dir, "session.jsonl");

	await backend.append(sessionFile, `${line(first, first)}\n`, 1);
	await firstEntered.promise;
	await backend.writeFull(sessionFile, `${line(second)}\n`, 2);
	releaseFirst.resolve();
	await secondEntered.promise;
	await uploader.close();

	expect(sent).toEqual([
		{ sha256: first, byte: 11 },
		{ sha256: second, byte: 22 },
	]);
	expect(maxActive).toBe(1);
});

test("failed hashes retry later and uploaded hashes stay done", async () => {
	const dir = scratch();
	const failedHash = hash(3);
	const uploadedHash = hash(4);
	await writeBlob(dir, failedHash);
	await writeBlob(dir, uploadedHash);
	const failedBefore = counterCount(sessionBlobsFailed);
	const uploadedBefore = counterCount(sessionBlobsUploaded);
	const sent: string[] = [];
	let uploader: SessionBlobUploader | undefined;
	uploader = createSessionBlobUploader({
		blobsDir: dir,
		put: async (req) => {
			sent.push(req.sha256);
			if (
				req.sha256 === failedHash &&
				sent.filter((sha) => sha === failedHash).length === 1
			) {
				throw new Error("temporary failure");
			}
			if (req.sha256 === uploadedHash)
				uploader?.offer(line(failedHash, uploadedHash));
		},
	});

	uploader.offer(line(failedHash, uploadedHash));
	await waitForCounter(sessionBlobsFailed, failedBefore, 1);
	await waitForCounter(sessionBlobsUploaded, uploadedBefore, 1);
	await uploader.close();

	expect(sent).toEqual([failedHash, uploadedHash, failedHash]);
	expect(counterCount(sessionBlobsFailed) - failedBefore).toBe(1);
	expect(counterCount(sessionBlobsUploaded) - uploadedBefore).toBe(2);
});

test("oversize blobs are done while ENOENT remains retryable", async () => {
	const dir = scratch();
	const oversized = hash(5);
	const missingHash = hash(6);
	const signal = hash(7);
	const oversizeBefore = counterCount(sessionBlobsOversize);
	const missingBefore = counterCount(sessionBlobsMissing);
	const failedBefore = counterCount(sessionBlobsFailed);
	await writeBlob(dir, oversized);
	await truncate(join(dir, oversized), MAX_SESSION_BLOB_BYTES + 1);
	await writeBlob(dir, signal);
	const signalEntered = Promise.withResolvers<void>();
	const uploader = createSessionBlobUploader({
		blobsDir: dir,
		put: async (req) => {
			if (req.sha256 === signal) signalEntered.resolve();
		},
	});

	uploader.offer(line(oversized, missingHash, signal));
	await signalEntered.promise;
	await waitForCounter(sessionBlobsMissing, missingBefore, 1);
	uploader.offer(line(oversized, missingHash));
	await waitForCounter(sessionBlobsMissing, missingBefore, 2);
	await uploader.close();

	expect(counterCount(sessionBlobsOversize) - oversizeBefore).toBe(1);
	expect(counterCount(sessionBlobsMissing) - missingBefore).toBe(2);
	expect(counterCount(sessionBlobsFailed) - failedBefore).toBe(0);
});

test("queue overflow drops the oldest pending entry", async () => {
	const dir = scratch();
	const hashes = Array.from({ length: SESSION_BLOB_QUEUE_MAX + 2 }, (_, i) =>
		hash(i + 10),
	);
	for (const sha of hashes) await writeBlob(dir, sha);
	const droppedBefore = counterCount(sessionBlobsDropped);
	const firstEntered = Promise.withResolvers<void>();
	const releaseFirst = Promise.withResolvers<void>();
	const allUploads = Promise.withResolvers<void>();
	const expectedUploads = hashes.length - 1;
	const sent: string[] = [];
	const uploader = createSessionBlobUploader({
		blobsDir: dir,
		put: async (req) => {
			sent.push(req.sha256);
			if (sent.length === expectedUploads) allUploads.resolve();
			if (sent.length === 1) {
				firstEntered.resolve();
				await releaseFirst.promise;
			}
		},
	});

	uploader.offer(line(hashes[0] ?? ""));
	await firstEntered.promise;
	for (const sha of hashes.slice(1)) uploader.offer(line(sha));
	releaseFirst.resolve();
	await allUploads.promise;
	await uploader.close();
	expect(counterCount(sessionBlobsDropped) - droppedBefore).toBe(1);
});

test("close drops queued uploads and waits only for the active request", async () => {
	const dir = scratch();
	const active = hash(30);
	const queued = hash(31);
	await writeBlob(dir, active);
	await writeBlob(dir, queued);
	const entered = Promise.withResolvers<void>();
	const release = Promise.withResolvers<void>();
	const sent: string[] = [];
	const uploader = createSessionBlobUploader({
		blobsDir: dir,
		put: async (req) => {
			sent.push(req.sha256);
			entered.resolve();
			await release.promise;
		},
	});

	uploader.offer(line(active));
	await entered.promise;
	uploader.offer(line(queued));
	const closed = uploader.close();
	release.resolve();
	await closed;

	expect(sent).toEqual([active]);
});

test("FailedPrecondition disables uploads and clears the pending queue", async () => {
	const dir = scratch();
	const first = hash(40);
	const queued = hash(41);
	await writeBlob(dir, first);
	await writeBlob(dir, queued);
	const failedBefore = counterCount(sessionBlobsFailed);
	const entered = Promise.withResolvers<void>();
	const release = Promise.withResolvers<void>();
	const sent: string[] = [];
	const uploader = createSessionBlobUploader({
		blobsDir: dir,
		put: async (req) => {
			sent.push(req.sha256);
			entered.resolve();
			await release.promise;
			throw new ConnectError("no blob store", Code.FailedPrecondition);
		},
	});

	uploader.offer(line(first));
	await entered.promise;
	uploader.offer(line(queued));
	release.resolve();
	await uploader.close();

	expect(sent).toEqual([first]);
	expect(counterCount(sessionBlobsFailed) - failedBefore).toBe(1);
});

test("a rejected put and throwing offer never fail a local append", async () => {
	const dir = scratch();
	const blobsDir = join(dir, "blobs");
	const sha = hash(50);
	await writeBlob(blobsDir, sha);
	const failedBefore = counterCount(sessionBlobsFailed);
	const uploader = createSessionBlobUploader({
		blobsDir,
		put: () => Promise.reject(new Error("Runner unavailable")),
	});
	const sink = {
		emit: () => {},
		emitDurable: async () => {},
		drain: async () => {},
	};
	const backend = new TranscriptTeeBackend(sink, dir, {
		onCommittedLine: (entry) => uploader.offer(entry),
	});
	const throwing = new TranscriptTeeBackend(sink, dir, {
		onCommittedLine: () => {
			throw new Error("offer failed");
		},
	});
	const sessionFile = join(dir, "session.jsonl");
	const logError = spyOn(console, "error");

	await backend.append(sessionFile, `${line(sha)}\n`, 1);
	await uploader.close();
	await throwing.append(sessionFile, "second\n", 2);

	expect(await Bun.file(sessionFile).text()).toBe(`${line(sha)}\nsecond\n`);
	expect(counterCount(sessionBlobsFailed) - failedBefore).toBe(1);
	expect(logError).toHaveBeenCalled();
	logError.mockRestore();
});
