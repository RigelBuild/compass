import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import type { WorkstreamSpec } from "../spawn";
import type { Agent } from "../stub-data";
import { NewWorkstreamDialog } from "./NewWorkstreamDialog";

// The new-workstream dialog's ADD-A-CARD contract (design compass-spawn-control
// T3, DL-164). It is a pure callback component: it assembles a WorkstreamSpec
// and hands it to onSubmit, or cancels — no store, no lifecycle effect, no
// initial-prompt field (that is the START operation, a separate dialog). These
// tests mount it over plain props and assert the user-visible contract.

const account = (id: string, handle: string) => ({
	id,
	handle,
	displayName: handle,
	kind: "agent" as const,
});

const AGENTS: Agent[] = [
	{
		account: account("acc-cook", "cook"),
		role: "worker",
		model: "m",
		cwd: "/",
		terminals: [],
	},
	{
		account: account("acc-scout", "scout"),
		role: "worker",
		model: "m",
		cwd: "/",
		terminals: [],
	},
];

const agentSelect = (c: HTMLElement) =>
	c.querySelector<HTMLSelectElement>("select.field-agent");
const titleInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>("input.field-title");
const prioritySelect = (c: HTMLElement) =>
	c.querySelector<HTMLSelectElement>("select.field-priority");
const handleInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>("input.field-handle");
const submitBtn = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>("button.submit");
const cancelBtn = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>("button.cancel");

describe("NewWorkstreamDialog", () => {
	test("renders every field and NO prompt field", () => {
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={() => {}}
				onCancel={() => {}}
			/>
		));
		expect(agentSelect(container)).not.toBeNull();
		expect(titleInput(container)).not.toBeNull();
		expect(prioritySelect(container)).not.toBeNull();
		// This is the add-a-card operation, not start: there is no prompt input.
		expect(container.querySelector("textarea")).toBeNull();
		expect(container.querySelector("[data-field='initial-prompt']")).toBeNull();
	});

	test("the '＋ new agent' option reveals the handle field (hidden until selected)", () => {
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={() => {}}
				onCancel={() => {}}
			/>
		));
		expect(handleInput(container)).toBeNull();
		const select = agentSelect(container);
		if (!select) throw new Error("no agent select");
		fireEvent.input(select, { target: { value: "__new__" } });
		expect(handleInput(container)).not.toBeNull();
	});

	test("submit is disabled until agent + title are set", () => {
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={() => {}}
				onCancel={() => {}}
			/>
		));
		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		expect(submit.disabled).toBe(true);

		const select = agentSelect(container);
		if (!select) throw new Error("no agent select");
		fireEvent.input(select, { target: { value: "acc-cook" } });
		// agent set, title still empty → still disabled
		expect(submit.disabled).toBe(true);

		const title = titleInput(container);
		if (!title) throw new Error("no title");
		fireEvent.input(title, { target: { value: "Fix the flake" } });
		expect(submit.disabled).toBe(false);
	});

	test("an existing-agent submit carries { kind: 'existing', agentAccountId }", () => {
		let captured: WorkstreamSpec | undefined;
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={(s) => {
					captured = s;
				}}
				onCancel={() => {}}
			/>
		));
		const select = agentSelect(container);
		const title = titleInput(container);
		const priority = prioritySelect(container);
		if (!select || !title || !priority) throw new Error("fields missing");
		fireEvent.input(select, { target: { value: "acc-scout" } });
		fireEvent.input(title, { target: { value: "Investigate CI" } });
		fireEvent.input(priority, { target: { value: "high" } });
		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		fireEvent.click(submit);

		expect(captured).toEqual({
			agent: { kind: "existing", agentAccountId: "acc-scout" },
			title: "Investigate CI",
			priority: "high",
		});
		// A WorkstreamSpec carries no lifecycle / prompt fields.
		expect("initialPrompt" in (captured as object)).toBe(false);
	});

	test("trims title + handle and rejects a whitespace-only title", () => {
		let captured: WorkstreamSpec | undefined;
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={(s) => {
					captured = s;
				}}
				onCancel={() => {}}
			/>
		));
		const select = agentSelect(container);
		if (!select) throw new Error("no agent select");
		fireEvent.input(select, { target: { value: "__new__" } });
		const handle = handleInput(container);
		const title = titleInput(container);
		if (!handle || !title) throw new Error("fields missing");
		// A whitespace-only title does not enable submit (agent is ready).
		fireEvent.input(handle, { target: { value: "  newbie  " } });
		fireEvent.input(title, { target: { value: "   " } });
		expect(submitBtn(container)?.disabled).toBe(true);
		// A real title enables it, and both values are trimmed in the spec.
		fireEvent.input(title, { target: { value: "  Fix the flake  " } });
		expect(submitBtn(container)?.disabled).toBe(false);
		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		fireEvent.click(submit);
		expect(captured).toEqual({
			agent: { kind: "new", handle: "newbie" },
			title: "Fix the flake",
			priority: "medium",
		});
	});

	test("a new-agent submit carries { kind: 'new', handle }", () => {
		let captured: WorkstreamSpec | undefined;
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={(s) => {
					captured = s;
				}}
				onCancel={() => {}}
			/>
		));
		const select = agentSelect(container);
		if (!select) throw new Error("no agent select");
		fireEvent.input(select, { target: { value: "__new__" } });
		const handle = handleInput(container);
		const title = titleInput(container);
		if (!handle || !title) throw new Error("fields missing");
		// title set but handle empty → submit still disabled
		fireEvent.input(title, { target: { value: "New lane" } });
		expect(submitBtn(container)?.disabled).toBe(true);
		fireEvent.input(handle, { target: { value: "newbie" } });
		expect(submitBtn(container)?.disabled).toBe(false);

		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		fireEvent.click(submit);
		expect(captured).toEqual({
			agent: { kind: "new", handle: "newbie" },
			title: "New lane",
			priority: "medium",
		});
	});

	test("Cancel fires onCancel and NOT onSubmit", () => {
		let submitted = false;
		let cancelled = false;
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={() => {
					submitted = true;
				}}
				onCancel={() => {
					cancelled = true;
				}}
			/>
		));
		const cancel = cancelBtn(container);
		if (!cancel) throw new Error("no cancel");
		fireEvent.click(cancel);
		expect(cancelled).toBe(true);
		expect(submitted).toBe(false);
	});

	test("Escape fires onCancel and NOT onSubmit", () => {
		let submitted = false;
		let cancelled = false;
		const { container } = render(() => (
			<NewWorkstreamDialog
				agents={AGENTS}
				onSubmit={() => {
					submitted = true;
				}}
				onCancel={() => {
					cancelled = true;
				}}
			/>
		));
		const dialog = container.querySelector("[role='dialog']");
		if (!dialog) throw new Error("no dialog");
		fireEvent.keyDown(dialog, { key: "Escape" });
		expect(cancelled).toBe(true);
		expect(submitted).toBe(false);
	});
});
