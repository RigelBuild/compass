// The agent's board surface: a thin broker over the Runner transport's Board
// call, plus the one native tool an agent registers to drive the Compass-native
// issue board — `board_set_issue_state`. This is the write half of the
// issue-ownership contract the taxonomy role prompts state: an agent moves its
// issue as the work moves and closes it itself; nothing advances board state for
// it (a forge closed/merged badge is consistent with DONE but never auto-advances
// the Compass-native state).
//
// This mirrors comms.ts / lifecycle.ts / forge.ts one leg over: `AgentGateway.Board`
// is a Connect **unary** over the per-container Unix socket (transport/index.ts),
// so correlation and deadlines belong to the RPC and a result is just the awaited
// return value — no pending map, no stdin pump, no deadlock. Cancellation is NOT
// plumbed: `execute`'s `AbortSignal` is not forwarded, so an aborted turn does not
// cancel an in-flight transition — it lands. Unlike `ForgeBroker` there is no
// idempotency key: a board transition is not a create, and the server treats a
// target equal to the current state as an idempotent no-op (a re-issue is
// harmless), so nothing needs to dedup.
//
// IDENTITY. The agent presents no token and asserts no account: the Runner owns
// which container (hence which session) a call arrived on, and the Server resolves
// session -> account and runs the transition under that caller (single-trust-domain
// MVP). Every write attributes to the agent's account with zero new authz code; a
// no-`BoardCaller` deployment fails closed at the relay as a thrown ConnectError,
// never a transport teardown.
//
// The board is a single Compass-native instance — no repo selector and no shared
// cross-repo credential — so the unguarded free-text-`repo` threat forge documents
// (A8) does not arise here: the one input is a Compass-local issue id resolved
// against the caller's own board. The shared call envelope is reused verbatim as
// the RelayBoardCall payload on the Runner->Server leg (DL-049), one wire shape
// for both hops. See packages/compass-agent/AGENTS.md for the package contract.

// The schema builder rides the SDK's own schema stack via its `/ark` compat
// facade — see the comms.ts note; one schema implementation in the graph, so
// there is no two-copy mismatch to catch.
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import {
	type BoardCallRequest,
	BoardCallRequestSchema,
	type BoardCallResult,
	create,
	IssueState,
	SetIssueStateRequestSchema,
} from "./compassv1";
import { attr, flat } from "./render-guard";

/**
 * The one transport method the board tools consume — a structural subset of
 * `RunnerTransport` (transport/index.ts), so `createUnixSocketTransport()`'s
 * result satisfies it directly while a unit test fakes a single method.
 */
export interface BoardTransport {
	board(req: BoardCallRequest): Promise<BoardCallResult>;
}

/**
 * A thin adapter over the board leg of the Runner transport. `call` delegates
 * straight to `transport.board(req)`; the Connect unary owns correlation and
 * deadlines. There is no idempotency key — see the file header (a transition is
 * not a create, and target-equals-current is a server-side no-op).
 */
export class BoardBroker {
	readonly #transport: BoardTransport;

	constructor(transport: BoardTransport) {
		this.#transport = transport;
	}

	call(req: BoardCallRequest): Promise<BoardCallResult> {
		return this.#transport.board(req);
	}
}

// The required-non-blank string idiom (comms/lifecycle/forge precedent): the
// `.narrow` predicate is enforced at runtime but has no JSON Schema form
// (`toJsonSchema` drops it), so the model sees a bare string and learns the rule
// only from the description — hence the description repeats it.
const nonBlank = (description: string) =>
	type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe(description);

// The eight real board states, as the model-facing tokens the tool accepts.
// ISSUE_STATE_UNSPECIFIED is deliberately absent: the closed enum rejects it at
// schema validation, so the sentinel never reaches the wire (the server's own
// UNSPECIFIED -> invalid_argument guard is defense-in-depth the tool never
// triggers).
type StateToken =
	| "backlog"
	| "todo"
	| "queued"
	| "blocked"
	| "in_progress"
	| "in_review"
	| "done"
	| "archived";

// The token -> proto enum map. Exhaustive over StateToken, so a token added to
// the schema without a mapping is a `tsc` error here rather than a silent
// UNSPECIFIED on the wire.
const STATE_BY_TOKEN: Record<StateToken, IssueState> = {
	backlog: IssueState.BACKLOG,
	todo: IssueState.TODO,
	queued: IssueState.QUEUED,
	blocked: IssueState.BLOCKED,
	in_progress: IssueState.IN_PROGRESS,
	in_review: IssueState.IN_REVIEW,
	done: IssueState.DONE,
	archived: IssueState.ARCHIVED,
};

// The inverse, for rendering the post-transition truth the server returns. A
// value outside the eight (UNSPECIFIED, or an enum the server grows before this
// map does) is absent, and the caller falls back to the requested token — never
// a raw number in the model's transcript.
const TOKEN_BY_STATE: Partial<Record<IssueState, StateToken>> = {
	[IssueState.BACKLOG]: "backlog",
	[IssueState.TODO]: "todo",
	[IssueState.QUEUED]: "queued",
	[IssueState.BLOCKED]: "blocked",
	[IssueState.IN_PROGRESS]: "in_progress",
	[IssueState.IN_REVIEW]: "in_review",
	[IssueState.DONE]: "done",
	[IssueState.ARCHIVED]: "archived",
};

const ISSUE_ID_DESC =
	"The Compass-local issue id (Issue.id) — NOT a forge issue number; must not be blank";
const STATE_DESC =
	"Target lifecycle state. Any of the eight is a legal target from any current state (the flow backlog -> todo -> queued -> in_progress -> in_review -> done, with blocked and archived as side states, is normative guidance, not server-enforced). A target equal to the current state is an idempotent no-op.";

/** Exported so a test can validate the wire contract the agent loop enforces. */
export const setIssueStateParameters = type({
	issue_id: nonBlank(ISSUE_ID_DESC),
	state: type(
		"'backlog' | 'todo' | 'queued' | 'blocked' | 'in_progress' | 'in_review' | 'done' | 'archived'",
	).describe(STATE_DESC),
});

/**
 * The `Error` a non-`setIssueState` `BoardCallResult` deserves — both shapes are
 * tool failures under the OMP contract:
 *   - `error` — an in-band domain failure (not_found, invalid_argument). The code
 *     and detail go into the message so the model can act on them. A line break
 *     in server text would forge a second line of authoritative tool output, so
 *     the detail passes through the shared `flat` (never a second copy of its
 *     regex — see render-guard.ts); the bound runs AFTER the collapse so slicing
 *     cannot re-expose a break the collapse removed.
 *   - anything else — the Server set no case at all. That is a protocol
 *     violation; succeeding silently would hand the model a fabricated result.
 */
function boardFailure(
	result: BoardCallResult,
	toolName: string,
	expected: string,
): Error {
	const outcome = result.result;
	if (outcome.case === "error") {
		const detail = flat(outcome.value.message).slice(0, 500);
		return new Error(
			`${toolName} failed: ${attr(outcome.value.code)}: ${detail}`,
		);
	}
	return new Error(
		`${toolName}: protocol violation — expected a ${expected} result, got ${outcome.case ?? "none"}`,
	);
}

/**
 * The native board tool set. One tool today — `board_set_issue_state`, the one
 * arm of `BoardCallRequest` — registered beside the comms/lifecycle/forge natives
 * in cli.ts. `approval: "write"` because it mutates canonical state (auto-executing
 * headless, the same class as the forge writes).
 */
export function createBoardTools(broker: BoardBroker): AgentTool[] {
	const setIssueState: AgentTool<typeof setIssueStateParameters> = {
		name: "board_set_issue_state",
		label: "Set board issue state",
		approval: "write",
		description: `Set a Compass board issue's canonical lifecycle state. This is the Compass-native board — a MANUAL lifecycle you own: move an issue as its work moves and close it (done) yourself; a forge closed/merged badge never advances board state for you. ${STATE_DESC}`,
		parameters: setIssueStateParameters,
		execute: async (toolCallId, params) => {
			const result = await broker.call(
				create(BoardCallRequestSchema, {
					callId: toolCallId,
					call: {
						case: "setIssueState",
						value: create(SetIssueStateRequestSchema, {
							issueId: params.issue_id,
							state: STATE_BY_TOKEN[params.state],
						}),
					},
				}),
			);
			if (result.result.case !== "setIssueState")
				throw boardFailure(result, "board_set_issue_state", "setIssueState");
			// The post-transition truth the server returns; fall back to the
			// requested token if the response carries no issue or an enum this map
			// does not yet name. Both branches are fixed known tokens, so the ack
			// line needs no render guard on the state; the issue id is model-supplied
			// and id-shaped, so it passes through `attr`.
			const resultState = result.result.value.issue?.state;
			const label =
				(resultState !== undefined ? TOKEN_BY_STATE[resultState] : undefined) ??
				params.state;
			return {
				content: [
					{
						type: "text",
						text: `Set issue ${attr(params.issue_id)} to ${label}.`,
					},
				],
			};
		},
	};

	return [setIssueState];
}
