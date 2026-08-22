// Bun test preload. Two jobs, both required before any test module loads:
//
//  1. A happy-dom global DOM, so component tests can mount SolidJS trees and
//     assert against rendered output (Bun's runner is otherwise headless).
//  2. A SolidJS JSX transform. Solid's JSX is not a pragma swap like React's —
//     it needs babel-preset-solid's compile-time reactive transform (the same
//     one vite-plugin-solid runs for the build). Bun's native transpiler emits
//     broken React-shaped code for Solid JSX (oven-sh/bun#3528), so we register
//     a Bun loader plugin that runs the official preset over .tsx on load.
//     Matching the build transform keeps test and production semantics identical.
//
// Wired via bunfig.toml `preload`.

import { afterEach } from "bun:test";
import { transformAsync } from "@babel/core";
import syntaxJsx from "@babel/plugin-syntax-jsx";
import presetTypeScript from "@babel/preset-typescript";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { cleanup } from "@solidjs/testing-library";
import solid from "babel-preset-solid";
import { plugin } from "bun";

GlobalRegistrator.register();

plugin({
	name: "solid-jsx",
	setup(build) {
		build.onLoad({ filter: /\.tsx$/ }, async (args) => {
			const source = await Bun.file(args.path).text();
			const result = await transformAsync(source, {
				filename: args.path,
				presets: [
					[solid, { generate: "dom", hydratable: false }],
					[presetTypeScript, { onlyRemoveTypeImports: true }],
				],
				plugins: [syntaxJsx],
				sourceMaps: "inline",
			});
			return { contents: result?.code ?? source, loader: "js" };
		});
	},
});

// ── Virtualized-list geometry shim ───────────────────────────────────────────
// The conversation stream (`.conv-stream`) is a @tanstack/solid-virtual chat-mode
// virtualizer: it only renders the thread rows whose measured positions fall in
// the scroll viewport. happy-dom has NO layout, so a scroll element's real rect
// is 0×0 and item offsetHeight is 0 — under which the virtualizer renders an
// EMPTY window and every test asserting on `.conv-stream` content (a `.msg`, a
// `.block-ask`, a `.thread`) would see nothing. This shim gives the layout-less
// DOM just enough geometry that a virtualized list renders its rows:
//
//   - `.conv-stream` reports a very tall viewport (100_000px), so the whole
//     fixture-sized channel is inside the window and renders in full — matching
//     the pre-virtualization behavior every existing suite was written against.
//   - a rendered thread row ([data-index]) reports a nonzero height so
//     measureElement records a real size.
//   - clientHeight / scrollHeight / a no-op scrollTo exist so the chat-mode
//     scrollToEnd() and the offset observer never throw in happy-dom.
//
// The dedicated scroll-contract suite (ChannelView.scroll.test.tsx) OVERRIDES
// this with a small viewport + real scroll emulation to exercise genuine
// windowing; this global default just keeps every other suite rendering content.
//
// Scoping: these getters are defined on HTMLDivElement.prototype, so they shadow
// the property div-prototype-wide for the whole test process. happy-dom defines
// the real getters on ANCESTOR prototypes (offsetHeight/clientHeight on
// HTMLElement.prototype, scrollHeight on Element.prototype), never own on
// HTMLDivElement.prototype — so we cannot conditionally skip installing. Instead
// each getter, for a NON-conv element, DELEGATES to the inherited getter
// (captured below before we override), so an ordinary div keeps happy-dom's real
// value and only `.conv-stream` / `[data-index]` rows get the synthetic geometry.
const CONV_STREAM_VIEWPORT = 100_000;
const CONV_ROW_HEIGHT = 64;
const divProto = Object.getPrototypeOf(document.createElement("div"));
const isConvStream = (el: HTMLElement): boolean =>
	el.classList?.contains("conv-stream") ?? false;
const hasIndex = (el: HTMLElement): boolean =>
	el.hasAttribute?.("data-index") ?? false;

/** The inherited getter for `prop` (from an ancestor prototype — happy-dom's
 *  real implementation), so a non-conv element delegates to it instead of being
 *  forced to 0. Walks the prototype chain above divProto; undefined if none. */
const inheritedGetter = (prop: string): (() => number) | undefined => {
	let proto = Object.getPrototypeOf(divProto);
	while (proto) {
		const desc = Object.getOwnPropertyDescriptor(proto, prop);
		if (desc?.get) return desc.get as () => number;
		proto = Object.getPrototypeOf(proto);
	}
	return undefined;
};

for (const [prop, convGet] of [
	[
		"offsetHeight",
		function (this: HTMLElement): number {
			if (isConvStream(this)) return CONV_STREAM_VIEWPORT;
			if (hasIndex(this)) return CONV_ROW_HEIGHT;
			return 0;
		},
	],
	[
		"clientHeight",
		function (this: HTMLElement): number {
			return isConvStream(this) ? CONV_STREAM_VIEWPORT : 0;
		},
	],
	[
		"scrollHeight",
		function (this: HTMLElement): number {
			if (!isConvStream(this)) return 0;
			const sizer = this.querySelector<HTMLElement>(".conv-sizer");
			return Number.parseInt(sizer?.style.height ?? "", 10) || 0;
		},
	],
] as const) {
	const inherited = inheritedGetter(prop);
	// A conv element gets the synthetic geometry; any other div delegates to the
	// inherited getter so this shim never shadows happy-dom's real value globally.
	const get = function (this: HTMLElement): number {
		if (isConvStream(this) || hasIndex(this)) return convGet.call(this);
		return inherited ? inherited.call(this) : 0;
	};
	Object.defineProperty(divProto, prop, { configurable: true, get });
}
// scrollTo: the virtualizer calls it via scrollToEnd(). happy-dom may leave it
// absent or non-callable on elements; install a no-op unless a real function is
// already present, so the chat-mode path never throws. This is the ONLY scroll
// affordance the global default provides — it deliberately gives NO
// scroll-behavior fidelity (the 100_000px viewport renders the whole list, so
// nothing windows); ALL scroll-contract coverage lives in
// ChannelView.scroll.test.tsx, which installs its own real scroll emulation.
if (typeof (divProto as { scrollTo?: unknown }).scrollTo !== "function") {
	Object.defineProperty(divProto, "scrollTo", {
		configurable: true,
		value() {},
	});
}

// ── happy-dom `textContent` numeric-coercion shim ────────────────────────────
// happy-dom's `Element.prototype.textContent` setter guards node creation with
// `if (textContent)`, so a FALSY non-string value — notably numeric `0` — sets
// NO child text node (happy-dom@20.11.1 nodes/element/Element.js). The DOM spec
// instead ToString-coerces the value (`0` → `"0"`, a real text node); every
// browser, and the Wails webview, does this.
//
// Solid 2's `insertExpression` relies on the spec behaviour: a reactive text
// interpolation whose value is `0` at first render (e.g. a live count that
// starts empty) writes `parent.textContent = 0`, leaving happy-dom's span with
// NO firstChild; when the value later becomes non-zero Solid takes the
// `parent.firstChild.data = value` fast-path and throws
// `TypeError: null is not an object`, which HALTS Solid 2's global scheduler and
// cascades into every later test. (First tripped by routing.test.tsx's
// `/backlog` deep-link: BacklogView's async `assignedIssues().length` renders as
// `0`, then resolves to a non-zero count one tick later.)
//
// Wrap the inherited setter so a non-null/undefined value is String-coerced
// before delegating — matching the spec — while `null`/`undefined` still clear
// as happy-dom intends. Scoped to the one owning descriptor, so every other
// textContent write keeps happy-dom's native behaviour.
const textContentDesc = (() => {
	let proto: object | null = Object.getPrototypeOf(
		document.createElement("span"),
	);
	while (proto) {
		const desc = Object.getOwnPropertyDescriptor(proto, "textContent");
		if (desc?.set) return { proto, desc };
		proto = Object.getPrototypeOf(proto);
	}
	return undefined;
})();
if (textContentDesc) {
	const nativeSet = textContentDesc.desc.set as (
		this: Node,
		v: unknown,
	) => void;
	Object.defineProperty(textContentDesc.proto, "textContent", {
		configurable: true,
		enumerable: textContentDesc.desc.enumerable,
		get: textContentDesc.desc.get,
		set(this: Node, value: unknown): void {
			nativeSet.call(this, value == null ? value : String(value));
		},
	});
}

// ── Global per-test DOM cleanup ──────────────────────────────────────────────
// Dispose every root `render()` mounts after each test, across ALL files. Bun
// scopes an `afterEach` registered as a module side-effect to the file that
// first imports the module, so `@solidjs/testing-library`'s own auto-cleanup
// binds to whichever test file loads it first and leaves every other file
// WITHOUT per-test disposal. Undisposed roots leak their `onCleanup` work —
// notably the Bridge's `installKeymap` window `keydown` listener — onto the
// shared happy-dom `window`, so a stale keymap from one file's mount fires
// during another file's test (a cross-file order-dependent failure). Registering
// `cleanup` from the preload runs it in the global scope, so every file disposes
// its roots after each test regardless of import order.
afterEach(cleanup);
