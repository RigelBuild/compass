import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// Vite + the SolidJS plugin (JSX -> reactive DOM transform). The UI consumes the
// generated @compass/client and dials the Compass server's loopback gRPC-Web dev door
// directly (URL from VITE_COMPASS_BASE_URL at boot); the door serves wildcard CORS, so
// no dev proxy is needed here.
export default defineConfig({
	plugins: [solid()],
	// Pin the dev-server port so the URL is copy-paste stable across restarts;
	// strictPort fails loudly rather than silently drifting to 5174 if taken.
	server: { port: 5173, strictPort: true },
	// Prebundle the CJS-only leaves of the markdown chain, or the dev server serves a
	// blank page (each served raw over `/@fs` with no ESM `default`). The nested `a > b > c`
	// form is required — a bare name resolves from the project root — and an unresolvable
	// middle segment is skipped silently, so if these paths rot there is no warning.
	optimizeDeps: {
		include: [
			"@rigelbuild/solid-markdown > remark-parse > mdast-util-from-markdown > micromark > debug",
			"@rigelbuild/solid-markdown > remark-parse > unified > extend",
			"@rigelbuild/solid-markdown > hast-util-to-jsx-runtime > style-to-js",
		],
	},
});
