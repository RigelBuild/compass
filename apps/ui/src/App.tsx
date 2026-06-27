import { CompassService } from "@compass/client";
import type { Component } from "solid-js";

// Walking-skeleton view: renders the contract's service name from the generated
// @compass/client descriptor, proving the schema -> client -> Solid UI stack
// compiles and renders.
const App: Component = () => (
	<main>
		<h1>Compass</h1>
		<p>compass.v1 contract: {CompassService.typeName}</p>
	</main>
);

export default App;
