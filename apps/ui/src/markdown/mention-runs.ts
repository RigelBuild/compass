import { parseMentions } from "../comms";
import type { Account } from "../comms-stub";

/** One rendered run of a text string: a plain run (`mention` absent) or a
 *  `@`-mention chip. `known` = the handle resolves to an account; `reserved` =
 *  a broadcast target (@everyone/@agents/@users). Plain runs render as text and
 *  mentions as `mention-chip` spans carrying the `reserved`/`unknown` modifiers
 *  styled by the `.mention-chip` rules in app.css, so every surface chips
 *  identically. */
export interface MentionRun {
	text: string;
	mention?: { handle: string; known: boolean; reserved: boolean };
}

/** Split `text` into alternating plain runs and `@`-mention runs, resolving each
 *  mention against `byHandle` (keyed lowercase). Pure over its inputs — a text
 *  node is parsed independently, so offsets are trivially correct (no cross-node
 *  math). A string with no mentions yields a single plain run (or none, for the
 *  empty string). */
export function mentionRuns(
	text: string,
	byHandle: Map<string, Account>,
): MentionRun[] {
	const mentions = parseMentions(text);
	const out: MentionRun[] = [];
	let cursor = 0;
	for (const men of mentions) {
		if (men.start > cursor) out.push({ text: text.slice(cursor, men.start) });
		out.push({
			text: text.slice(men.start, men.end),
			mention: {
				handle: men.handle,
				known: byHandle.has(men.handle.toLowerCase()),
				reserved: men.reserved,
			},
		});
		cursor = men.end;
	}
	if (cursor < text.length) out.push({ text: text.slice(cursor) });
	return out;
}
