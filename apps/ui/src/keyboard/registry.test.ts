import { afterEach, describe, expect, test } from "bun:test";
import type { Command, CommandId } from "./commands";
import { createCommandRegistry } from "./registry";

// The runtime registry behind the frozen CommandRegistry contract. These defend
// the two behaviors the contract does not spell out: last-write-wins on a
// duplicate id, and a dev-only warning when that collision happens (silent in a
// non-dev build so offline tests and production stay quiet).

type MutableEnv = Record<string, unknown>;

const id = (s: string): CommandId => s as CommandId;

const makeCommand = (rawId: string, title: string): Command => ({
	id: id(rawId),
	title,
	keywords: [],
	scope: "global",
	run() {},
});

describe("createCommandRegistry", () => {
	test("get resolves a registered command; all snapshots them in order", () => {
		const registry = createCommandRegistry();
		const a = makeCommand("view.bridge", "Bridge");
		const b = makeCommand("palette.open", "Palette");
		registry.register(a);
		registry.register(b);

		expect(registry.get(id("view.bridge"))).toBe(a);
		expect(registry.get(id("palette.open"))).toBe(b);
		expect(registry.all()).toEqual([a, b]);
	});

	test("get returns undefined for an unregistered id", () => {
		const registry = createCommandRegistry();
		expect(registry.get(id("nope.missing"))).toBeUndefined();
	});

	test("last-write-wins: re-registering an id replaces the command", () => {
		const registry = createCommandRegistry();
		const first = makeCommand("view.bridge", "First");
		const second = makeCommand("view.bridge", "Second");
		registry.register(first);
		registry.register(second);

		expect(registry.get(id("view.bridge"))).toBe(second);
		// The replacement collapses to one entry (no duplicate slot).
		expect(registry.all()).toEqual([second]);
	});

	describe("dev-mode duplicate warning", () => {
		// `import.meta.env` is the process-wide Vite env, read directly by
		// registry.ts's dev gate. Under Bun ≥1.4 an ASSIGNMENT coerces the value
		// to a string — `env.DEV = false` stores the truthy string "false" — so
		// the falsy ("not a dev build") case MUST be simulated by DELETE, not by
		// assigning `false`. This mirrors provider.test.ts's env handling. Restore
		// deletes when DEV was originally absent so nothing leaks into a sibling.
		const env = import.meta.env as MutableEnv;
		const realWarn = console.warn;
		let hadDev = false;
		let priorDev: unknown;

		afterEach(() => {
			if (hadDev) env.DEV = priorDev;
			else delete env.DEV;
			console.warn = realWarn;
		});

		test("warns on a duplicate id in a dev build", () => {
			hadDev = "DEV" in env;
			priorDev = env.DEV;
			env.DEV = true;
			const calls: unknown[][] = [];
			console.warn = (...args: unknown[]) => {
				calls.push(args);
			};

			const registry = createCommandRegistry();
			registry.register(makeCommand("view.bridge", "First"));
			expect(calls.length).toBe(0);
			registry.register(makeCommand("view.bridge", "Second"));

			expect(calls.length).toBe(1);
			expect(String(calls[0]?.[0])).toContain("view.bridge");
		});

		test("stays silent outside a dev build", () => {
			hadDev = "DEV" in env;
			priorDev = env.DEV;
			// DELETE, not `= false`: under Bun ≥1.4 an assigned `false` becomes the
			// truthy string "false" and would leave the dev gate enabled.
			delete env.DEV;
			let warned = false;
			console.warn = () => {
				warned = true;
			};

			const registry = createCommandRegistry();
			registry.register(makeCommand("view.bridge", "First"));
			registry.register(makeCommand("view.bridge", "Second"));

			expect(warned).toBe(false);
		});
	});
});
