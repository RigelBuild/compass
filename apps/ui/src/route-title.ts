import { dmLabel, isDm } from "./comms";
import type { AppStore } from "./store";
import type { RouteMatch } from "./view-route";

/** A view's title: the channel, topic or agent name, or the fixed
 *  view name. An id the store cannot resolve yet titles as the raw id. */
export function routeTitle(
	match: RouteMatch,
	store: Pick<
		AppStore,
		"channels" | "topics" | "accounts" | "caller" | "agentById"
	>,
): string {
	switch (match.view) {
		case "bridge":
			return "Bridge";
		case "agents":
			return "Agents";
		case "backlog":
			return "Backlog";
		case "done":
			return "Done";
		case "settings":
			return "Settings";
		case "channel": {
			const channel = store.channels().find((c) => c.id === match.channelId);
			if (!channel) return match.channelId;
			if (!isDm(channel)) return channel.name;
			const byId = new Map(store.accounts().map((a) => [a.id, a]));
			return dmLabel(channel, store.caller().id, byId);
		}
		case "topic":
			return (
				store.topics().find((t) => t.id === match.topicId)?.name ??
				match.topicId
			);
		case "agent":
			return (
				store.agentById(match.agentId)?.account.displayName ?? match.agentId
			);
	}
}
