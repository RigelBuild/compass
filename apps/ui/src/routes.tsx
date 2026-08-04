// The shared route table (record A1). One `<Route>` declaration site, rendered
// identically by production (HashRouter, index.tsx) and tests (MemoryRouter,
// test-router.tsx) — no drift between prod and test.
//
// The seven `View` surfaces map 1:1 to routes; the `:channelId` / `:topicId` /
// `:agentId` params carry the selection that today lives only in signals. The
// channel segment nests the SEA-1655 T5 `/channel/:channelId/topic/:topicId`
// deep link — the topic message view — as a child `<Route>` under it. The `*`
// catch-all redirects an unknown/stale deep-link to the board rather than a
// blank screen.

import { Navigate, Route } from "@solidjs/router";
import type { Component } from "solid-js";
import { AgentView } from "./components/AgentView";
import { BacklogView } from "./components/BacklogView";
import { Bridge } from "./components/Bridge";
import { ChannelView } from "./components/ChannelView";
import { DoneView } from "./components/DoneView";
import { SettingsView } from "./components/SettingsView";
import { TopicView } from "./components/TopicView";

/** Redirect a catch-all match to the board. */
const RedirectHome: Component = () => <Navigate href="/" />;

/** The shared `<Route>` fragment — the app's route table. Rendered as the
 *  children of whichever router (HashRouter in prod, MemoryRouter in tests)
 *  wraps it with `root={App}`. */
export const AppRoutes: Component = () => (
	<>
		<Route path="/" component={Bridge} />
		<Route path="/channel/:channelId" component={ChannelView} />
		<Route path="/channel/:channelId/topic/:topicId" component={TopicView} />
		<Route path="/agent/:agentId" component={AgentView} />
		<Route path="/backlog" component={BacklogView} />
		<Route path="/done" component={DoneView} />
		<Route path="/settings" component={SettingsView} />
		<Route path="*" component={RedirectHome} />
	</>
);
