import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// Vite + the SolidJS plugin (JSX -> reactive DOM transform). The UI consumes
// the generated @compass/client; the daemon transport + dev proxy arrive with
// the local-transport work.
export default defineConfig({
	plugins: [solid()],
});
