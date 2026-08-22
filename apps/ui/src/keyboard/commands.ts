/**
 * Command registry contract — the spine of the keyboard-first product.
 *
 * Frozen by design record D5 (Command palette + global keymap). CONTRACTS
 * ONLY: interfaces, type unions, and branded ids. No runtime behavior, no
 * registry implementation — compass-ui implements against these stubs.
 */

import type { FocusZone } from "./zones";

/**
 * The scopes a command can bind to (D5:421-424).
 *
 * A command is either `'global'` or bound to one of the four interactive focus
 * zones. Scoped commands (e.g. "Split pane right") rank above global ones when
 * their scope is active.
 */
export type CommandScope = FocusZone | "global";

/**
 * Branded command id (D5:439-443).
 *
 * A nominal string type so a raw string can't be passed where a registered
 * command id is required. Mint one with a cast at the registration boundary;
 * everywhere else, a `CommandId` can only come from a registered command.
 */
export type CommandId = string & { readonly __brand: "CommandId" };

/**
 * A command-registry entry (D5:421-422, 433-435).
 *
 * Every primary action in the product registers one of these. `keywords`
 * broadens fuzzy matching in the palette; `shortcut` is the display chip (the
 * authoritative chord lives in the keymap table, see keymap.ts).
 */
export interface Command {
	readonly id: CommandId;
	readonly title: string;
	readonly keywords: string[];
	readonly scope: CommandScope;
	/** Display chip for the palette/tooltip; authoritative chord is in keymap.ts. */
	readonly shortcut?: string;
	run(): void;
}

/**
 * The kinds of destination a navigation-mode result can be (D5:425-429).
 */
export type DestinationKind =
	| "agent"
	| "channel"
	| "topic"
	| "issue"
	| "pr"
	| "view";

/**
 * A navigation-mode destination (D5:425-429).
 *
 * Selecting a destination navigates. `score` is the provider's ranking signal
 * (higher = more relevant); see `DestinationProvider` for the ranking inputs.
 */
export interface Destination {
	readonly id: string;
	readonly title: string;
	readonly kind: DestinationKind;
	navigate(): void;
	/** Optional provider-assigned ranking score; higher ranks first. */
	readonly score?: number;
}

/**
 * An async, ranked destination provider (D5:425-429).
 *
 * Providers back navigation mode: agents (tree), channels/topics, issues
 * (`SEA-…` keys), PRs, views. The palette is prefix-free — bare typing matches
 * both commands and destinations.
 *
 * Ranking inputs (D5:429): results are ordered by **recency** and **fuzzy
 * score**. The exact weighting of these two signals is an NLB deferral and is
 * intentionally NOT specified here — this contract fixes only the inputs, not
 * their weights.
 */
export interface DestinationProvider {
	readonly id: string;
	query(input: string): Promise<Destination[]>;
}

/**
 * Enforcement rule (D5:439-443): the primitive `Button`/`Menu` take a
 * `command` id — NOT a raw handler — for any primary action, so an unregistered
 * primary action can't be wired. This is the type that expresses that rule:
 * a primary action references a registered `CommandId`, and the registry
 * (below) is the single source of truth menus, buttons, and the palette all
 * resolve through.
 */
export interface PrimaryAction {
	readonly command: CommandId;
}

/**
 * The command registry shape (D5:433-439).
 *
 * Interface ONLY — no implementation. The registry is the single source of
 * truth: menus, buttons, and the palette all resolve commands through it, so
 * palette coverage can't drift from the UI.
 */
export interface CommandRegistry {
	register(cmd: Command): void;
	get(id: CommandId): Command | undefined;
	all(): Command[];
	/** Remove a command by id; an unknown id is a no-op. Additive to the frozen
	 *  D5 contract so a surface can retract the commands it registered when it
	 *  unmounts (the shared registry is app-lifetime — see keyboard/spine.ts). */
	unregister(id: CommandId): void;
}
