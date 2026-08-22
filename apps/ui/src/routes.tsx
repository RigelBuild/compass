// The shared route table (record A1). One route-config array, consumed
// identically by production (hashHistory, mount.tsx) and tests (memoryHistory,
// test-router.tsx) — no drift between prod and test.
//
// The seven `View` surfaces map 1:1 to routes; the `:channelId` / `:topicId` /
// `:agentId` params carry the selection that today lives only in signals. The
// channel segment nests the `/channel/:channelId/topic/:topicId` deep link —
// the topic message view — as a child route under it. The `*all` catch-all
// redirects an unknown/stale deep-link to the board rather than a blank screen.
//
// Router 2 is config-based: routes are plain objects, and the App shell is the
// router instance's render-prop child (the always-mounted root layout), not a
// `root=` prop. `defineRoutes` preserves the literal path types the typed
// `paths`/hooks read.

import { defineRoutes, useNavigate } from "@solidjs/router";
import type { Component } from "solid-js";
import { onSettled } from "solid-js";
import { AgentView } from "./components/AgentView";
import { BacklogView } from "./components/BacklogView";
import { Bridge } from "./components/Bridge";
import { ChannelView } from "./components/ChannelView";
import { DoneView } from "./components/DoneView";
import { SettingsView } from "./components/SettingsView";
import { TopicView } from "./components/TopicView";

/** Redirect a catch-all match to the board. Router 2 has no `<Navigate>`
 *  component; a matched component navigates imperatively once mounted. */
const RedirectHome: Component = () => {
	const navigate = useNavigate();
	onSettled(() => navigate("/"));
	return null;
};

/** The shared route table — the app's routes. Consumed by whichever history
 *  adapter (hashHistory in prod, memoryHistory in tests) `createRouter` wraps,
 *  with the App shell as the render-prop root layout. */
export const appRoutes = defineRoutes([
	{ path: "/", component: Bridge },
	{ path: "/channel/:channelId", component: ChannelView },
	{ path: "/channel/:channelId/topic/:topicId", component: TopicView },
	{ path: "/agent/:agentId", component: AgentView },
	{ path: "/backlog", component: BacklogView },
	{ path: "/done", component: DoneView },
	{ path: "/settings", component: SettingsView },
	{ path: "*all", component: RedirectHome },
]);
