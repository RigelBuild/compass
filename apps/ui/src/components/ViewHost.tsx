import { Dynamic } from "@solidjs/web";
import { type Component, createMemo } from "solid-js";
import { appRoutes } from "../routes";
import { parseRoute, type RouteMatch, routePath } from "../view-route";
import { ViewContext, type ViewScope } from "../view-scope";

// Each parsed view renders through its route-table entry, so appRoutes stays
// the one component map for the router, the tests and every view.
const ROUTE_PATTERN: Record<RouteMatch["view"], string> = {
	bridge: "/",
	channel: "/channel/:channelId",
	topic: "/channel/:channelId/topic/:topicId",
	agent: "/agent/:agentId",
	agents: "/agents",
	backlog: "/backlog",
	done: "/done",
	settings: "/settings/:section",
};
const CATCH_ALL = "*all";

function routeComponent(pattern: string): Component {
	const route = appRoutes.find((entry) => entry.path === pattern);
	if (!route) throw new Error(`appRoutes has no route for ${pattern}`);
	return route.component;
}

// A path that only parses by falling back (unknown, or missing a param) is not
// canonical; it renders the catch-all so the view is redirected home.
function isCanonical(path: string): boolean {
	const trimmed = path.replace(/\/+$/, "") || "/";
	return routePath(parseRoute(trimmed)) === trimmed;
}

/** Render one view instance: its scope is the subtree's ViewContext, and its
 *  path picks the component from the shared route table. */
export const ViewHost: Component<{ scope: ViewScope }> = (props) => {
	const component = createMemo(() => {
		const path = props.scope.path();
		return isCanonical(path)
			? routeComponent(ROUTE_PATTERN[props.scope.route().view])
			: routeComponent(CATCH_ALL);
	});
	return (
		<ViewContext value={props.scope}>
			<Dynamic component={component()} />
		</ViewContext>
	);
};
