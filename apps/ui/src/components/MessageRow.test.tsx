import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import type { Account, Message } from "../comms-stub";
import { MessageRow } from "./ChannelView";

// The author kind distinguishes system senders from ordinary users.

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

function rowKind(author: Account): string | undefined {
	const byId = new Map<string, Account>([[author.id, author]]);
	const byHandle = new Map<string, Account>([[author.handle, author]]);
	const { container } = render(() => (
		<MessageRow msg={msg(author.id)} byId={byId} byHandle={byHandle} />
	));
	const el = container.querySelector<HTMLElement>(".msg");
	if (!el) throw new Error("message row did not render");
	return el.dataset.kind;
}

describe("MessageRow author kind", () => {
	test("a system author renders data-kind system, not user/agent", () => {
		expect(rowKind(acc("acc-sys-compass", "system"))).toBe("system");
	});

	// The contrast that proves the attribute keys on the author's kind, not a
	// constant: a user and an agent get their own values, distinct from system.
	test("user and agent authors keep their own distinct kinds", () => {
		expect(rowKind(acc("acc-matt", "user"))).toBe("user");
		expect(rowKind(acc("acc-cook", "agent"))).toBe("agent");
	});
});
