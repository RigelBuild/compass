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
	// Prebundle two CJS-only leaves of the markdown chain, or the dev server
	// serves a blank page: both are served raw over `/@fs`, so the browser's ESM
	// loader finds no `default` binding and the whole module graph dies before
	// `render()` runs. Naming them here routes them through the prebundler,
	// which wraps CJS into ESM.
	//
	// One symptom, two unrelated causes — worth keeping straight, because only
	// the first has anything to do with dev builds:
	//
	//   - `micromark` exposes a `development` export condition (its package.json
	//     maps it to `./dev/index.js`), and `vite-plugin-solid` prepends that
	//     condition while serving (`dist/esm/index.mjs:152`). So the dev tree
	//     loads, and `dev/lib/create-tokenizer.js:40` does
	//     `import createDebug from "debug"`. The non-dev tree never mentions it.
	//   - `unified` has NO dev tree — a single `"exports": "./index.js"` — and
	//     its `lib/index.js:350` `import extend from "extend"` is unconditional.
	//     That one breaks under every condition set.
	//
	// Which is why dropping the `development` condition is not the smaller fix
	// it looks like: it does not work. Vite re-supplies the condition itself
	// (`DEV_PROD_CONDITION` in its default client conditions, expanded to
	// `development` outside a production build), and even with both sources
	// filtered, `extend` still breaks. It also costs solid-refresh, which
	// degrades HMR to a full reload and says so in a console warning.
	//
	// The nested `a > b > c` form is required — a bare `"debug"` resolves from
	// the project root rather than from the dependency's own directory. Its one
	// hazard: Vite silently skips an unresolvable INTERMEDIATE segment (it keeps
	// the previous basedir), so if these paths ever rot there is no warning,
	// just the blank page again.
	optimizeDeps: {
		include: [
			"solid-markdown > remark-parse > mdast-util-from-markdown > micromark > debug",
			"solid-markdown > remark-parse > unified > extend",
		],
	},
});
