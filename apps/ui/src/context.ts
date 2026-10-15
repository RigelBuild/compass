// The AppStore context — how components reach the shared store without prop
// drilling. The root provides a single createAppStore() instance; every surface
// reads it through useStore().

import { type Accessor, createContext, useContext } from "solid-js";
import type { RefLinkRule } from "./refs/grammar";
import type { AppStore } from "./store";

const StoreContext = createContext<AppStore>();

export { StoreContext };

/** Read the app store from context. Throws if used outside the provider. */
export function useStore(): AppStore {
	const store = useContext(StoreContext);
	if (!store) {
		throw new Error("useStore must be used within a StoreContext provider");
	}
	return store;
}

const NO_RULES: Accessor<readonly RefLinkRule[]> = () => [];
// Defaulted so MarkdownText still renders with no provider (bare component tests).
export const RefLinkRulesContext =
	createContext<Accessor<readonly RefLinkRule[]>>(NO_RULES);

export function useRefLinkRules(): Accessor<readonly RefLinkRule[]> {
	return useContext(RefLinkRulesContext);
}
