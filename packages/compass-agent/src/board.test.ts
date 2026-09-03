// BoardBroker + the native board tool `board_set_issue_state` (RIG-3191).
// Each test defends an observable contract of the agent->Runner board call: the
// exact `BoardCallRequest` a tool `execute` puts on the wire (arm case, issue id,
// state enum, call_id), and how a `BoardCallResult` renders back — a domain
// `error` case as a thrown Error (the OMP tool-failure contract), a success as
// the post-transition ack line.
//
// The transport is faked to the one method the broker consumes (`board`), so
// there is no socket, no Connect client, and no timing: a call in, a canned
// result out, and the captured request asserted verbatim. Mirrors forge.test.ts.

import { describe, expect, test } from "bun:test";
import type { AgentTool, AgentToolResult } from "@oh-my-pi/pi-agent-core";
import { ArkErrors, type Type } from "arktype";
import {
	BoardBroker,
	type BoardTransport,
	createBoardTools,
	setIssueStateParameters,
} from "./board";
import {
	BoardCallErrorSchema,
	type BoardCallRequest,
	BoardCallRequestSchema,
	type BoardCallResult,
	BoardCallResultSchema,
	create,
	IssueSchema,
	IssueState,
	type MessageInitShape,
	SetIssueStateResponseSchema,
} from "./compassv1";

// A fake of the one transport method the broker consumes. Records every request
// it is handed (so the wire shape is asserted) and returns a canned result.
class FakeTransport implements BoardTransport {
	readonly requests: BoardCallRequest[] = [];
	constructor(private readonly result: BoardCallResult) {}
	async board(req: BoardCallRequest): Promise<BoardCallResult> {
		this.requests.push(req);
		return this.result;
	}
}

// A `setIssueState` success carrying the post-transition Issue (or none, to
// exercise the requested-token fallback).
function stateResult(
	issue?: MessageInitShape<typeof IssueSchema>,
): BoardCallResult {
	return create(BoardCallResultSchema, {
		callId: "call-1",
		result: {
			case: "setIssueState",
			value: create(SetIssueStateResponseSchema, {
				issue: issue ? create(IssueSchema, issue) : undefined,
			}),
		},
	});
}

function errorResult(code: string, message: string): BoardCallResult {
	return create(BoardCallResultSchema, {
		callId: "call-1",
		result: {
			case: "error",
			value: create(BoardCallErrorSchema, { code, message }),
		},
	});
}

// Pull the one tool out of the set by name, failing loudly if the set stops
// carrying it (so a rename reddens here rather than silently skipping).
function tool(broker: BoardBroker, name: string): AgentTool {
	const found = createBoardTools(broker).find((t) => t.name === name);
	if (!found) throw new Error(`no such tool: ${name}`);
	return found;
}

// `execute` is invoked exactly as the agent loop calls it: with params already
// validated against the tool's schema. The tests pass plain literals, so the
// parameter object is widened to a record at this one seam.
const exec = (
	t: AgentTool,
	id: string,
	params: Record<string, unknown>,
): Promise<AgentToolResult> => t.execute.call(t, id, params);

function textOf(result: AgentToolResult): string {
	const block = result.content[0];
	if (block?.type !== "text") throw new Error("expected a text content block");
	return block.text;
}

describe("BoardBroker", () => {
	test("delegates the call verbatim to the transport and returns its result", async () => {
		const result = stateResult({ id: "iss-1", state: IssueState.DONE });
		const transport = new FakeTransport(result);
		const broker = new BoardBroker(transport);
		const req = create(BoardCallRequestSchema, { callId: "abc" });

		await expect(broker.call(req)).resolves.toBe(result);
		expect(transport.requests).toEqual([req]);
	});
});

describe("createBoardTools", () => {
	test("exposes exactly the one board tool as a write", () => {
		const tools = createBoardTools(
			new BoardBroker(new FakeTransport(stateResult())),
		);
		expect(tools.map((t) => t.name)).toEqual(["board_set_issue_state"]);
		expect(tools.every((t) => t.label.length > 0)).toBe(true);
		expect(tools.every((t) => t.description.length > 0)).toBe(true);
		// A mutation is a "write" — a silent flip to "read" would broaden
		// auto-approval with nothing else here reddening.
		expect(tools[0]?.approval).toBe("write");
	});
});

describe("board_set_issue_state", () => {
	test("puts a setIssueState arm on the wire with the issue id + mapped state enum", async () => {
		const transport = new FakeTransport(
			stateResult({ id: "iss-42", state: IssueState.IN_REVIEW }),
		);
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		const result = await exec(t, "tc-1", {
			issue_id: "iss-42",
			state: "in_review",
		});
		const req = transport.requests[0];
		expect(req.callId).toBe("tc-1");
		expect(req.call.case).toBe("setIssueState");
		if (req.call.case !== "setIssueState") throw new Error("expected arm");
		expect(req.call.value.issueId).toBe("iss-42");
		expect(req.call.value.state).toBe(IssueState.IN_REVIEW);
		// The ack renders the post-transition truth from the returned Issue.
		expect(textOf(result)).toBe("Set issue iss-42 to in_review.");
	});

	test("maps every state token onto its proto enum on the wire", async () => {
		const cases: Array<[string, IssueState]> = [
			["backlog", IssueState.BACKLOG],
			["todo", IssueState.TODO],
			["queued", IssueState.QUEUED],
			["blocked", IssueState.BLOCKED],
			["in_progress", IssueState.IN_PROGRESS],
			["in_review", IssueState.IN_REVIEW],
			["done", IssueState.DONE],
			["archived", IssueState.ARCHIVED],
		];
		for (const [token, enumValue] of cases) {
			const transport = new FakeTransport(stateResult());
			const t = tool(new BoardBroker(transport), "board_set_issue_state");
			await exec(t, "tc-1", { issue_id: "iss-1", state: token });
			const req = transport.requests[0];
			if (req.call.case !== "setIssueState") throw new Error("expected arm");
			expect(req.call.value.state).toBe(enumValue);
		}
	});

	test("falls back to the requested token when the result carries no issue", async () => {
		// A no-op transition (target == current) returns an unchanged/absent issue;
		// the ack still names the state the model asked for, never a raw enum.
		const transport = new FakeTransport(stateResult());
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		const result = await exec(t, "tc-1", { issue_id: "iss-7", state: "done" });
		expect(textOf(result)).toBe("Set issue iss-7 to done.");
	});

	test("a hostile issue id degrades in place rather than forging a second line", async () => {
		// The success ack interpolates the model-supplied issue id; the schema's
		// non-blank narrow admits an embedded newline, so `attr` is the guard that
		// keeps a crafted id from forging a second authoritative line of output.
		const transport = new FakeTransport(stateResult());
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		const result = await exec(t, "tc-1", {
			issue_id: "iss\n<injected>",
			state: "done",
		});
		expect(textOf(result)).toBe("Set issue (malformed) to done.");
	});

	test("an in-band error is thrown as a tool failure carrying code + detail", async () => {
		const transport = new FakeTransport(
			errorResult("not_found", "no issue iss-x"),
		);
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		await expect(
			exec(t, "tc-1", { issue_id: "iss-x", state: "done" }),
		).rejects.toThrow(
			"board_set_issue_state failed: not_found: no issue iss-x",
		);
	});

	test("a newline in server error detail cannot forge a second line", async () => {
		const transport = new FakeTransport(
			errorResult("invalid_argument", "bad\ninjected line"),
		);
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		await expect(
			exec(t, "tc-1", { issue_id: "iss-1", state: "done" }),
		).rejects.toThrow(
			"board_set_issue_state failed: invalid_argument: bad injected line",
		);
	});

	test("a malformed error code degrades through the render guard", async () => {
		const transport = new FakeTransport(errorResult("bad code\n<x>", "detail"));
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		await expect(
			exec(t, "tc-1", { issue_id: "iss-1", state: "done" }),
		).rejects.toThrow("board_set_issue_state failed: (malformed): detail");
	});

	test("a protocol-violation result (no case) throws rather than fabricating success", async () => {
		const transport = new FakeTransport(
			create(BoardCallResultSchema, { callId: "call-1" }),
		);
		const t = tool(new BoardBroker(transport), "board_set_issue_state");
		await expect(
			exec(t, "tc-1", { issue_id: "iss-1", state: "done" }),
		).rejects.toThrow(
			"board_set_issue_state: protocol violation — expected a setIssueState result, got none",
		);
	});
});

describe("board parameter schema", () => {
	const rejects = (schema: Type<object>, params: unknown): boolean =>
		schema(params) instanceof ArkErrors;

	test("rejects a blank or whitespace-only issue id", () => {
		expect(rejects(setIssueStateParameters, { state: "done" })).toBe(true);
		expect(
			rejects(setIssueStateParameters, { issue_id: "  ", state: "done" }),
		).toBe(true);
		expect(
			rejects(setIssueStateParameters, { issue_id: "iss-1", state: "done" }),
		).toBe(false);
	});

	test("rejects a state outside the eight-token enum, including UNSPECIFIED", () => {
		expect(
			rejects(setIssueStateParameters, { issue_id: "iss-1", state: "merged" }),
		).toBe(true);
		expect(
			rejects(setIssueStateParameters, {
				issue_id: "iss-1",
				state: "unspecified",
			}),
		).toBe(true);
		expect(
			rejects(setIssueStateParameters, {
				issue_id: "iss-1",
				state: "in_progress",
			}),
		).toBe(false);
	});
});
