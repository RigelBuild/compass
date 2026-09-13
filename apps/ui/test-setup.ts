// Bun test preload (wired via bunfig.toml `preload`). Two jobs before any test module loads:
// (1) a happy-dom global DOM so component tests can mount SolidJS trees; (2) a SolidJS JSX
// transform — Solid's JSX needs babel-preset-solid's compile-time reactive transform (Bun's
// native transpiler emits broken React-shaped code, oven-sh/bun#3528), matching the build.

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

// The conversation stream is a @tanstack/solid-virtual chat-mode virtualizer that renders
// only rows in the scroll viewport. happy-dom has NO layout (rect 0×0, offsetHeight 0), so
// it would render an EMPTY window and every `.conv-stream` content assertion would see
// nothing. This shim gives the layout-less DOM just enough geometry to render its rows.

// `.conv-stream` reports a very tall viewport (100_000px) so the whole fixture channel is
// in-window; a rendered `[data-index]` row reports a nonzero height so measureElement records
// a real size; clientHeight/scrollHeight/a no-op scrollTo exist so scroll code never throws.
// The dedicated scroll-contract suite OVERRIDES this with a small viewport + real emulation.

// Scoping: these getters live on HTMLDivElement.prototype, shadowing the property process-wide.
// happy-dom defines the real getters on ANCESTOR prototypes, never own on HTMLDivElement.proto,
// so we can't conditionally skip installing — each getter DELEGATES to the inherited one for a
// non-conv element (captured below), so only `.conv-stream` / `[data-index]` get synthetic geometry.
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
// scrollTo: the virtualizer calls it via scrollToEnd(). happy-dom may leave it absent or
// non-callable; install a no-op unless a real function is present so the chat-mode path never
// throws. The ONLY scroll affordance the global default provides — no scroll-behavior fidelity;
// all scroll-contract coverage lives in ChannelView.scroll.test.tsx with its own emulation.
if (typeof (divProto as { scrollTo?: unknown }).scrollTo !== "function") {
	Object.defineProperty(divProto, "scrollTo", {
		configurable: true,
		value() {},
	});
}

// ── happy-dom `textContent` numeric-coercion shim ────────────────────────────

// happy-dom's `textContent` setter guards node creation with `if (textContent)`, so a FALSY
// value — notably numeric `0` — sets NO child text node, where the spec ToString-coerces it.
// Solid 2's `insertExpression` relies on the spec: a reactive `0` leaves no firstChild, then a
// later non-zero value takes the `firstChild.data` fast-path and throws, HALTING the scheduler.

// Wrap the inherited setter so a non-null/undefined value is String-coerced before delegating
// (matching the spec), while `null`/`undefined` still clear. Scoped to the one owning descriptor.
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

// Dispose every root `render()` mounts after each test, across ALL files. Bun scopes a
// module-side-effect `afterEach` to the file that first imports it, so testing-library's own
// auto-cleanup leaves other files WITHOUT disposal — undisposed roots leak `onCleanup` work
// onto the shared window. Registering from the preload runs it globally, order-independent.
afterEach(cleanup);
