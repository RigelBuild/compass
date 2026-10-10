// The shared route table (record A1). One route-config array, consumed
// identically by production (hashHistory, mount.tsx) and tests (memoryHistory,
// test-router.tsx) — no drift between prod and test.
//
// The eight `View` surfaces map 1:1 to routes; the `:channelId` / `:topicId` /
// `:agentId` params carry the selection that today lives only in signals. The
// channel segment nests the `/channel/:channelId/topic/:topicId` deep link —
// the topic message view — as a child route under it. The `*all` catch-all
// redirects an unknown/stale deep-link to the board rather than a blank screen.
//
// Router 2 is config-based: routes are plain objects, and the App shell is the
// router instance's render-prop child (the always-mounted root layout), not a
// `root=` prop. `defineRoutes` preserves the literal path types the typed
// `paths`/hooks read.

import { defineRoutes } from "@solidjs/router";
import type { Component } from "solid-js";
import { onSettled } from "solid-js";
import { AgentsView } from "./components/AgentsView";
import { AgentView } from "./components/AgentView";
import { Bridge } from "./components/Bridge";
import { ChannelView } from "./components/ChannelView";
import { SettingsView } from "./components/settings/SettingsView";
import { TopicView } from "./components/TopicView";
import { routePath } from "./view-route";
import { useView } from "./view-scope";

/** Redirect a stale path to the canonical route for its parsed view. It moves
 *  its own view and replaces the entry, so Back cannot loop onto the stale path. */
const RedirectCanonical: Component = () => {
	const view = useView();
	onSettled(() => {
		view.navigate(routePath(view.route()), { replace: true });
	});
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
	{ path: "/agents", component: AgentsView },
	// Backlog and Done are Bridge segments; one component keeps the Bridge mounted.
	{ path: "/backlog", component: Bridge },
	{ path: "/done", component: Bridge },
	{ path: "/settings/:section", component: SettingsView },
	{ path: "*all", component: RedirectCanonical },
]);
