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
	// Prebundle the CJS-only leaves of the markdown chain, or the dev server
	// serves a blank page.
	//
	// `vite-plugin-solid` prepends the `development` export condition while
	// serving (it needs it for solid-refresh HMR), which resolves dependencies
	// to their development entry points. `solid-markdown` then reaches
	// `micromark/dev` and `unified`, which `import debug from "debug"` and
	// `import extend from "extend"` — both CJS with no ESM default export. Vite
	// serves them raw over `/@fs`, so the browser's ESM loader finds no `default`
	// binding and the whole module graph dies before `render()` runs.
	//
	// Naming them here routes them through the prebundler, which wraps CJS into
	// ESM. The nested `a > b > c` form is required: a bare `"debug"` does not
	// match the dev tree's own resolution of it. Listing the two leaves rather
	// than dropping the `development` condition keeps solid-refresh working.
	optimizeDeps: {
		include: [
			"solid-markdown > remark-parse > unified > extend",
			"solid-markdown > remark-parse > mdast-util-from-markdown > micromark > debug",
		],
	},
});
