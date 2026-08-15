import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import type { Account, Message } from "../comms-stub";
import { MessageRow } from "./ChannelView";

// The message row's author-style contract: a row's `.msg` element carries the
// author's `kind` (`user`/`agent`/`system`) as a modifier class, so each sender
// reads distinctly. The system arm is the reserved `@compass` platform sender
// (SEA-1820 T5) — without its own class it would render styled as a plain user.
// MessageRow is rendered directly (no virtualizer/store) so the assertion pins
// the class mapping alone.

function acc(id: string, kind: Account["kind"]): Account {
	return { id, handle: id, displayName: id, kind };
}

function msg(authorAccountId: string): Message {
	return {
		id: "m1",
		topicId: "top-x",
		authorAccountId,
		atUnixMs: 1_000,
		blocks: [{ kind: "text", text: "hello" }],
	};
}

function rowClass(author: Account): DOMTokenList {
	const byId = new Map<string, Account>([[author.id, author]]);
	const byHandle = new Map<string, Account>([[author.handle, author]]);
	const { container } = render(() => (
		<MessageRow msg={msg(author.id)} byId={byId} byHandle={byHandle} />
	));
	const el = container.querySelector(".msg");
	if (!el) throw new Error("message row did not render");
	return el.classList;
}

describe("MessageRow author style", () => {
	test("a system author renders the .msg.system modifier, not user/agent", () => {
		const cls = rowClass(acc("acc-sys-compass", "system"));
		expect(cls.contains("system")).toBe(true);
		expect(cls.contains("user")).toBe(false);
		expect(cls.contains("agent")).toBe(false);
	});

	// The contrast that proves the modifier keys on the author's kind, not a
	// constant: a user and an agent get their own classes, distinct from system.
	test("user and agent authors keep their own distinct modifiers", () => {
		expect(rowClass(acc("acc-matt", "user")).contains("user")).toBe(true);
		expect(rowClass(acc("acc-cook", "agent")).contains("agent")).toBe(true);
	});
});
