import { describe, expect, test } from "bun:test";
import {
	assertReconcilableDecisionFiles,
	type ReconcileRequest,
	reconcile,
} from "./index.ts";

function decisionFile(
	id: string,
	area = "repo",
): { path: string; text: string } {
	return {
		path: `docs/designs/decisions/${area}/${id}.md`,
		text: [
			"---",
			`id: ${id}`,
			`decision: "Decision ${id}"`,
			'status: "Active (Matt, 2026-01-01)"',
			`record: ../../${area}/record.md`,
			"---",
		].join("\n"),
	};
}

function dependencies(files: readonly { path: string; text: string }[]) {
	return {
		listDecisionFiles: async (_root: string) => files.map(({ path }) => path),
		readDecisionFile: async (_root: string, path: string) => {
			const file = files.find((candidate) => candidate.path === path);
			if (file === undefined) throw new Error(`unexpected path: ${path}`);
			return file.text;
		},
	};
}

const EMPTY_BODY = { repo: "compass", landed: [] } satisfies ReconcileRequest;

describe("decision-file reconciliation", () => {
	test("builds the request body and keeps duplicate IDs across areas", async () => {
		const files = [
			decisionFile("DL-010", "agent"),
			decisionFile("DL-002", "ui"),
			decisionFile("DL-002", "server"),
		];
		const roots: string[] = [];
		const deps = {
			...dependencies(files),
			listDecisionFiles: async (root: string) => {
				roots.push(root);
				return files.map(({ path }) => path);
			},
		};
		expect(await assertReconcilableDecisionFiles("/repo", deps)).toEqual({
			repo: "compass",
			landed: [
				{ id: "DL-002", surface: "designs", ref: "none" },
				{ id: "DL-002", surface: "designs", ref: "none" },
				{ id: "DL-010", surface: "designs", ref: "none" },
			],
		});
		expect(roots).toEqual(["/repo"]);
	});

	test("sorts IDs numerically rather than lexically", async () => {
		const files = [
			decisionFile("DL-100"),
			decisionFile("DL-010"),
			decisionFile("DL-002"),
		];
		const body = await assertReconcilableDecisionFiles(
			"/repo",
			dependencies(files),
		);
		expect(body.landed.map(({ id }) => id)).toEqual([
			"DL-002",
			"DL-010",
			"DL-100",
		]);
	});

	test("refuses a malformed decision file and names its path", async () => {
		const file = decisionFile("DL-001");
		await expect(
			assertReconcilableDecisionFiles("/repo", {
				...dependencies([file]),
				readDecisionFile: async () =>
					file.text.replace("id: DL-001", "id: DL-002"),
			}),
		).rejects.toThrow(`${file.path}: line 2`);
	});

	test("refuses an empty decision-file frontier", async () => {
		await expect(
			assertReconcilableDecisionFiles("/repo", dependencies([])),
		).rejects.toThrow(
			"decision file list is empty; refusing to post an empty frontier",
		);
	});
});

describe("reconcile", () => {
	test("posts the exact request and body", async () => {
		const requests: Request[] = [];
		const files = [
			decisionFile("DL-001", "first"),
			decisionFile("DL-001", "second"),
		];
		const body = await assertReconcilableDecisionFiles(
			"/repo",
			dependencies(files),
		);
		await reconcile(body, "test-token", {
			fetchFn: async (input, init) => {
				requests.push(new Request(String(input), init));
				return new Response(null, { status: 204 });
			},
		});
		expect(requests).toHaveLength(1);
		const request = requests[0];
		expect(request?.url).toBe("https://dl.rigel.build/reconcile");
		expect(request?.method).toBe("POST");
		expect(request?.headers.get("Authorization")).toBe("Bearer test-token");
		expect(await request?.json()).toEqual({
			repo: "compass",
			landed: [
				{ id: "DL-001", surface: "designs", ref: "none" },
				{ id: "DL-001", surface: "designs", ref: "none" },
			],
		});
	});
	test("rejects redirects and trims the token", async () => {
		let init: RequestInit | undefined;
		await reconcile(EMPTY_BODY, "  token  ", {
			fetchFn: async (_input, requestInit) => {
				init = requestInit;
				return new Response(null, { status: 204 });
			},
		});
		expect(init?.redirect).toBe("error");
		expect(new Headers(init?.headers).get("Authorization")).toBe(
			"Bearer token",
		);
	});
	test("zero timeout uses the default deadline", async () => {
		let signal: AbortSignal | null | undefined;
		let requestedTimeout = 0;
		const control = new AbortController();
		await reconcile(EMPTY_BODY, "token", {
			timeoutMs: 0,
			timeoutSignal: (timeoutMs) => {
				requestedTimeout = timeoutMs;
				return control.signal;
			},
			fetchFn: async (_input, requestInit) => {
				signal = requestInit?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(requestedTimeout).toBe(30_000);
		expect(signal).toBe(control.signal);
		control.abort();
	});

	test("rejects a non-2xx response", async () => {
		await expect(
			reconcile(EMPTY_BODY, "token", {
				fetchFn: async () => new Response(null, { status: 503 }),
			}),
		).rejects.toThrow("HTTP 503");
	});

	test("rejects an empty token without making a request", async () => {
		let called = false;
		await expect(
			reconcile(EMPTY_BODY, "", {
				fetchFn: async () => {
					called = true;
					return new Response(null, { status: 204 });
				},
			}),
		).rejects.toThrow("DL_CLAIM_TOKEN is required");
		expect(called).toBe(false);
	});

	test("passes an already-aborted seam signal to fetch and surfaces rejection", async () => {
		const control = new AbortController();
		control.abort();
		let receivedSignal: AbortSignal | null | undefined;
		let fetchStartedResolve: (() => void) | undefined;
		const fetchStarted = new Promise<void>((resolve) => {
			fetchStartedResolve = resolve;
		});
		const pending = reconcile(EMPTY_BODY, "token", {
			timeoutMs: 5,
			timeoutSignal: () => control.signal,
			fetchFn: async (_input, init) => {
				receivedSignal = init?.signal;
				fetchStartedResolve?.();
				if (init?.signal?.aborted)
					throw new DOMException("The operation was aborted", "AbortError");
				return new Response(null, { status: 204 });
			},
		});
		await fetchStarted;
		expect(receivedSignal).toBe(control.signal);
		await expect(pending).rejects.toThrow("The operation was aborted");
	});

	test("builds the deadline signal from the caller's timeout", async () => {
		let requestedTimeout = 0;
		let signal: AbortSignal | null | undefined;
		const control = new AbortController();
		await reconcile(EMPTY_BODY, "token", {
			timeoutMs: 5,
			timeoutSignal: (timeoutMs) => {
				requestedTimeout = timeoutMs;
				return control.signal;
			},
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(requestedTimeout).toBe(5);
		expect(signal).toBe(control.signal);
	});

	test("defaults to a real firing deadline when none is injected", async () => {
		let signal: AbortSignal | null | undefined;
		await reconcile(EMPTY_BODY, "token", {
			timeoutMs: 1,
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return new Response(null, { status: 204 });
			},
		});
		if (!signal?.aborted)
			await new Promise<void>((resolve) =>
				signal?.addEventListener("abort", () => resolve(), { once: true }),
			);
		expect(signal?.aborted).toBe(true);
	});
});
