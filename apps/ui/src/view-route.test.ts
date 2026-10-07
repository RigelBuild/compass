import { describe, expect, test } from "bun:test";
import type { RouteMatch } from "./view-route";
import { parseRoute, routePath } from "./view-route";

const routes: { name: string; path: string; match: RouteMatch }[] = [
	{ name: "bridge", path: "/", match: { view: "bridge" } },
	{
		name: "channel",
		path: "/channel/ch-1",
		match: { view: "channel", channelId: "ch-1" },
	},
	{
		name: "topic",
		path: "/channel/ch-1/topic/t-9",
		match: { view: "topic", channelId: "ch-1", topicId: "t-9" },
	},
	{
		name: "agent",
		path: "/agent/acc-x",
		match: { view: "agent", agentId: "acc-x" },
	},
	{ name: "backlog", path: "/backlog", match: { view: "backlog" } },
	{ name: "done", path: "/done", match: { view: "done" } },
	{ name: "settings", path: "/settings", match: { view: "settings" } },
];

describe("view routes", () => {
	test.each(routes)("round-trips the $name route", ({ path, match }) => {
		expect(parseRoute(path)).toEqual(match);
		expect(routePath(match)).toBe(path);
	});

	test("maps an unknown path to the bridge route", () => {
		expect(parseRoute("/not-a-view")).toEqual({ view: "bridge" });
		expect(routePath(parseRoute("/not-a-view"))).toBe("/");
	});
});
