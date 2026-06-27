import { CompassService } from "@compass/client";
import type { Component } from "solid-js";

// The walking-skeleton view: it renders the contract's fully-qualified service
// name straight from the generated @compass/client descriptor, proving the
// schema -> generated client -> Solid UI stack compiles and renders. The real
// Bridge (swimlane board, agent panes, the xterm.js terminal) lands with the
// daemon transport and the M3 UI work.
const App: Component = () => (
	<main>
		<h1>Compass</h1>
		<p>compass.v1 contract: {CompassService.typeName}</p>
	</main>
);

export default App;
