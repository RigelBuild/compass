/**
 * Command registry (RIG-2130 T1) — the runtime backing of the established
 * `CommandRegistry` contract (commands.ts). Map-backed, last-write-wins: a
 * re-`register` under an existing id replaces the prior command (so a user
 * remap or a later default override is a plain overwrite), and in a dev build a
 * duplicate id logs a warning so an accidental collision is visible without
 * failing the app.
 */

import type { Command, CommandId, CommandRegistry } from "./commands";

/**
 * Create an empty command registry. `register` is last-write-wins; `get`
 * resolves by branded id; `all` snapshots the registered commands as an array
 * in insertion order (Map iteration order; a re-registered id keeps its
 * original slot).
 */
export function createCommandRegistry(): CommandRegistry {
	const commands = new Map<CommandId, Command>();

	return {
		register(cmd: Command): void {
			if (import.meta.env?.DEV && commands.has(cmd.id)) {
				console.warn(
					`compass: duplicate command id "${cmd.id}" — last write wins`,
				);
			}
			commands.set(cmd.id, cmd);
		},
		get(id: CommandId): Command | undefined {
			return commands.get(id);
		},
		all(): Command[] {
			return [...commands.values()];
		},
		unregister(id: CommandId): void {
			commands.delete(id);
		},
	};
}
