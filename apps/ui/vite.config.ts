import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// Vite + the SolidJS plugin (JSX -> reactive DOM transform). The UI consumes
// the generated @compass/client; the daemon transport + dev proxy arrive with
// the local-transport work.
export default defineConfig({
	plugins: [solid()],
	// Pin the dev-server port so the URL is copy-paste stable across restarts;
	// strictPort fails loudly rather than silently drifting to 5174 if taken.
	server: { port: 5173, strictPort: true },
});
