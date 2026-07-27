import { createRoot } from "solid-js";
import { render } from "solid-js/web";
import App from "./App";
import { StoreContext } from "./context";
import { createAppStore } from "./store";

const root = document.getElementById("root");
if (!root) {
	throw new Error("missing #root element");
}

// The store is an app-lifetime singleton; createRoot gives its memos a stable
// owner (intentionally never disposed) so Solid doesn't warn about computations
// created before render() establishes a root. One unified store drives every
// surface: the board, the per-agent workspace, and the channel conversation.
const store = createRoot(() => createAppStore());

render(
	() => (
		<StoreContext.Provider value={store}>
			<App />
		</StoreContext.Provider>
	),
	root,
);
