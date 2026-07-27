/**
 * restart — cooperatively recycle this session to pick up host-staged changes
 * that a live `refresh` cannot reach (new extensions, project context, slash
 * commands, prompt templates, the tool roster, the model/provider registry).
 *
 * Restart recycles ONLY this session (same loaded engine code): the host
 * quiesces the running turn, flushes the transcript to disk, disposes the
 * session, then re-opens it from its file. Picking up a new engine *binary* is a
 * host-process operation, never this per-agent tool.
 *
 * Deadlock-critical shape: `execute()` returns an acknowledgement immediately,
 * then fires `requestRestart()` from an UNTRACKED continuation — never inline
 * (its `waitForIdle()` cannot resolve while the tool blocks the turn) and never
 * via `#schedulePostPromptTask` (that scheduler always tracks the task in
 * `#postPromptTasks`, and `requestRestart()`'s own `waitForIdle()` / `dispose()`
 * await that very set → self-deadlock). Absent from `#postPromptTasks`, the
 * continuation sits in neither set the restart drains; `requestRestart()`'s
 * internal `waitForIdle()` supplies the "let this turn settle" wait.
 *
 * Result reporting splits on dispose ordering. Pre-dispose refusals
 * (`busy`/`unavailable`/`no-session-file`, all returned before teardown) are
 * surfaced to the still-open transcript so the model sees them. A post-dispose
 * host-callback throw has no awaiting caller on this path — dispose already
 * closed the transcript — so it is caught and logged (recovery via the durable
 * session file), never left unhandled and never silently swallowed.
 */
import type { AgentTool, AgentToolContext, AgentToolResult, AgentToolUpdateCallback } from "@oh-my-pi/pi-agent-core";
import { logger, prompt } from "@oh-my-pi/pi-utils";
import { type } from "arktype";
import restartDescription from "../prompts/tools/restart.md" with { type: "text" };
import { createCustomMessage } from "../session/messages";
import type { ToolSession } from "./index";
import type { OutputMeta } from "./output-meta";
import { toolResult } from "./tool-result";

const restartSchema = type({});

/** Details payload for TUI rendering of a restart acknowledgement. */
export interface RestartToolDetails {
	scheduled: boolean;
	meta?: OutputMeta;
}

/** One-line notice describing why a scheduled restart refused before teardown. */
function refusalNotice(reason: "unavailable" | "no-session-file" | "busy"): string {
	switch (reason) {
		case "unavailable":
			return "Restart was not performed: restart is unavailable in this session.";
		case "no-session-file":
			return "Restart was not performed: this session has no session file to re-attach.";
		case "busy":
			return "Restart was not performed: input is still queued. Retry once the session is idle.";
	}
}

export class RestartTool implements AgentTool<typeof restartSchema, RestartToolDetails> {
	readonly name = "restart";
	// `exec` tier: restart disposes and recycles the session. As a
	// model-discoverable tool it must NOT auto-run in always-ask/write modes —
	// same tier and reasoning as `refresh`. Auto-runs only in yolo.
	readonly approval = "exec" as const;
	readonly label = "Restart";
	readonly summary = "Recycle this session to pick up host-staged changes refresh cannot reach";
	readonly description: string;
	readonly parameters = restartSchema;
	readonly strict = true;
	readonly loadMode = "discoverable" as const;

	constructor(private readonly session: ToolSession) {
		this.description = prompt.render(restartDescription);
	}

	/**
	 * Offer the tool only when a host `onRestartRequested` callback is wired
	 * (`session.requestRestart` bound) — mirrors how the SDK binds the method.
	 * Absent the callback there is nothing to drive, so the tool is not created
	 * rather than presented as one that always errors.
	 */
	static createIf(session: ToolSession): RestartTool | null {
		if (!session.requestRestart) return null;
		return new RestartTool(session);
	}

	async execute(
		_toolCallId: string,
		_params: typeof restartSchema.infer,
		_signal?: AbortSignal,
		_onUpdate?: AgentToolUpdateCallback<RestartToolDetails>,
		_context?: AgentToolContext,
	): Promise<AgentToolResult<RestartToolDetails>> {
		const requestRestart = this.session.requestRestart;
		if (!requestRestart) {
			return {
				content: [{ type: "text", text: "Restart is unavailable in this session." }],
				isError: true,
				details: { scheduled: false },
			};
		}

		// Fire from an UNTRACKED continuation (a detached promise, never a tracked
		// post-prompt task). requestRestart()'s own waitForIdle() defers the actual
		// recycle until this turn settles, so returning the ack first does not race
		// teardown. Report the outcome; never leave the promise unhandled.
		void requestRestart()
			.then(result => {
				if (result.ok) return;
				// Pre-dispose refusal: the session is still alive, so surface it to
				// the transcript. queueDeferredMessage triggers a turn so the model
				// reacts to the refusal instead of silently believing it restarted.
				this.session.queueDeferredMessage?.(
					createCustomMessage(
						"restart-refused",
						refusalNotice(result.reason),
						true,
						undefined,
						new Date().toISOString(),
					),
				);
			})
			.catch((err: unknown) => {
				// A rejected requestRestart() is either a recoverable pre-dispose
				// throw (flush/ensureOnDisk/drainedRefresh failed, session still
				// alive and unlatched — decision (h)) or a terminal post-dispose
				// throw (the callback threw; old session gone). The tool cannot
				// append either way — the pre-dispose refusals it *can* surface
				// arrive as a returned { ok: false } in .then() above, not as a
				// rejection — so log dispose-agnostically. Recovery is via the
				// durable session file; never swallow.
				logger.error("restart tool: requestRestart failed", {
					error: err instanceof Error ? err.message : String(err),
				});
			});

		return toolResult<RestartToolDetails>({ scheduled: true })
			.text("Restart scheduled. It runs once this turn settles; the conversation resumes in the recycled session.")
			.done();
	}
}
