import type { Accessor, Context } from "solid-js";
import {
	createContext,
	createEffect,
	createMemo,
	createSignal,
	useContext,
} from "solid-js";
import type { Channel, Topic } from "./comms-stub";
import { type AppStore, firstChannelId } from "./store";
import type { Agent } from "./stub-data";
import type { RouteMatch } from "./view-route";
import { parseRoute } from "./view-route";

export interface ViewScope {
	id: string;
	path: Accessor<string>;
	route: Accessor<RouteMatch>;
	channel: Accessor<Channel | undefined>;
	topic: Accessor<Topic | undefined>;
	agent: Accessor<Agent | undefined>;
	navigate: (path: string) => void;
}

export const ViewContext: Context<ViewScope | undefined> = createContext<
	ViewScope | undefined
>(undefined);

export function useView(): ViewScope {
	const view = useContext(ViewContext);
	if (!view)
		throw new Error("useView must be used within a ViewContext provider");
	return view;
}

export function createViewScope(
	store: AppStore,
	id: string,
	initialPath: string,
): ViewScope {
	const [path, setPath] = createSignal(initialPath);
	const route = createMemo(() => parseRoute(path()));
	const channel = createMemo(() => {
		const match = route();
		if (match.view !== "channel" && match.view !== "topic") return undefined;
		return store.channels().find((item) => item.id === match.channelId);
	});
	const topic = createMemo(() => {
		const match = route();
		if (match.view !== "topic") return undefined;
		return store.topics().find((item) => item.id === match.topicId);
	});
	const agent = createMemo(() => {
		const match = route();
		if (match.view !== "agent") return undefined;
		return store.agentById(match.agentId);
	});
	const navigate = (nextPath: string): void => {
		setPath(nextPath);
	};

	// Pending-aware: an unknown id is held until the first snapshot, then this
	// view (only) falls back, mirroring the store's applyChannelRoute.
	createEffect(
		() => {
			const match = route();
			if (match.view !== "channel" && match.view !== "topic") return null;
			const channels = store.channels();
			return {
				match,
				channelKnown: channels.some((item) => item.id === match.channelId),
				topicKnown:
					match.view === "topic" &&
					store.topics().some((item) => item.id === match.topicId),
				firstSnapshotArrived: store.firstSnapshotArrived(),
				fallbackChannelId: firstChannelId(channels),
			};
		},
		(resolution) => {
			if (!resolution?.firstSnapshotArrived) return;
			if (!resolution.channelKnown) {
				navigate(
					resolution.fallbackChannelId
						? `/channel/${resolution.fallbackChannelId}`
						: "/",
				);
				return;
			}
			if (resolution.match.view === "topic" && !resolution.topicKnown) {
				navigate(`/channel/${resolution.match.channelId}`);
			}
		},
	);

	return { id, path, route, channel, topic, agent, navigate };
}
