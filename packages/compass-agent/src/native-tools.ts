// Registers the native AgentTools through the SDK's `customTools` CustomTool arm.

import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import type { TSchema } from "@oh-my-pi/pi-ai";
import type { CustomTool } from "@oh-my-pi/pi-coding-agent";

/**
 * Adapts a native `AgentTool` to the SDK's `CustomTool` shape. The SDK calls a
 * CustomTool as `(id, params, onUpdate, ctx, signal)`; this restores the
 * AgentTool order `(id, params, signal, onUpdate)`, so a native may use both.
 * Render hooks are not carried: AgentTool types them over an unknown theme, which
 * CustomTool cannot accept, and natives run headless. Nor are concurrency, intent
 * or matcher hooks, which CustomTool has no field for.
 */
export function nativeAsCustomTool<TParams extends TSchema, TDetails>(
	tool: AgentTool<TParams, TDetails>,
): CustomTool<TParams, TDetails> {
	return {
		name: tool.name,
		label: tool.label,
		description: tool.description,
		parameters: tool.parameters,
		strict: tool.strict,
		hidden: tool.hidden,
		loadMode: tool.loadMode,
		deferrable: tool.deferrable,
		approval: tool.approval,
		formatApprovalDetails: tool.formatApprovalDetails,
		execute: (toolCallId, params, onUpdate, _ctx, signal) =>
			tool.execute(toolCallId, params, signal, onUpdate),
	};
}
