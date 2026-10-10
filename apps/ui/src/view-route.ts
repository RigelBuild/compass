export const SETTINGS_SECTIONS = [
	"general",
	"appearance",
	"tracker",
	"models",
] as const;
export type SettingsSection = (typeof SETTINGS_SECTIONS)[number];
export const SETTINGS_SECTION_LABEL: Record<SettingsSection, string> = {
	general: "General",
	appearance: "Appearance",
	tracker: "Tracker",
	models: "Models",
};

export type RouteMatch =
	| { view: "bridge" }
	| { view: "agents" }
	| { view: "backlog" }
	| { view: "done" }
	| { view: "settings"; section: SettingsSection }
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
		case "settings": {
			const section =
				param === undefined
					? SETTINGS_SECTIONS[0]
					: SETTINGS_SECTIONS.find((known) => known === param);
			return section ? { view: "settings", section } : { view: "bridge" };
		}
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
			return `/settings/${match.section}`;
		case "channel":
			return `/channel/${match.channelId}`;
		case "topic":
			return `/channel/${match.channelId}/topic/${match.topicId}`;
		case "agent":
			return `/agent/${match.agentId}`;
	}
}
