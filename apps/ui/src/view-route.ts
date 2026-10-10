export type RouteMatch =
	| { view: "bridge" }
	| { view: "agents" }
	| { view: "backlog" }
	| { view: "done" }
	| { view: "settings" }
	| { view: "channel"; channelId: string }
	| { view: "topic"; channelId: string; topicId: string }
	| { view: "agent"; agentId: string };

export function parseRoute(path: string): RouteMatch {
	const [head, param, sub, subParam] = path
		.split("/")
		.filter((segment) => segment.length > 0);

	switch (head) {
		case undefined:
			return { view: "bridge" };
		case "channel":
			if (param && sub === "topic" && subParam) {
				return { view: "topic", channelId: param, topicId: subParam };
			}
			return param ? { view: "channel", channelId: param } : { view: "bridge" };
		case "agent":
			return param ? { view: "agent", agentId: param } : { view: "bridge" };
		case "agents":
			return { view: "agents" };
		case "backlog":
			return { view: "backlog" };
		case "done":
			return { view: "done" };
		case "settings":
			return { view: "settings" };
		default:
			return { view: "bridge" };
	}
}

export function routePath(match: RouteMatch): string {
	switch (match.view) {
		case "bridge":
			return "/";
		case "agents":
			return "/agents";
		case "backlog":
			return "/backlog";
		case "done":
			return "/done";
		case "settings":
			return "/settings";
		case "channel":
			return `/channel/${match.channelId}`;
		case "topic":
			return `/channel/${match.channelId}/topic/${match.topicId}`;
		case "agent":
			return `/agent/${match.agentId}`;
	}
}
