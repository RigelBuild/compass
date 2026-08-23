import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import type { SpawnSpec } from "../spawn";
import { StartAgentDialog } from "./StartAgentDialog";

// The start-agent dialog's START contract (design compass-spawn-control T3,
// DL-164). The caller has already fixed the agent + card via props.spec,
// so the only field is the initial prompt (a textarea). An empty prompt is
// valid ("start idle"). Pure callback component — no store.

const SPEC: Omit<SpawnSpec, "initialPrompt"> = {
	agentAccountId: "acc-cook",
	issueId: "iss-42",
};

const promptArea = (c: HTMLElement) =>
	c.querySelector<HTMLTextAreaElement>("textarea.field-prompt");
const submitBtn = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>("button.submit");
const cancelBtn = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>("button.cancel");

describe("StartAgentDialog", () => {
	test("an EMPTY prompt submits (start idle) and carries the fixed spec + initialPrompt ''", () => {
		let captured: SpawnSpec | undefined;
		const { container } = render(() => (
			<StartAgentDialog
				spec={SPEC}
				onSubmit={(s) => {
					captured = s;
				}}
				onCancel={() => {}}
			/>
		));
		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		expect(submit.disabled).toBe(false);
		fireEvent.click(submit);
		expect(captured).toEqual({
			agentAccountId: "acc-cook",
			issueId: "iss-42",
			initialPrompt: "",
		});
	});

	test("a non-empty prompt is carried verbatim", () => {
		let captured: SpawnSpec | undefined;
		const { container } = render(() => (
			<StartAgentDialog
				spec={SPEC}
				onSubmit={(s) => {
					captured = s;
				}}
				onCancel={() => {}}
			/>
		));
		const area = promptArea(container);
		if (!area) throw new Error("no prompt");
		fireEvent.input(area, { target: { value: "pick up SEA-1729" } });
		flush();
		const submit = submitBtn(container);
		if (!submit) throw new Error("no submit");
		fireEvent.click(submit);
		expect(captured).toEqual({
			agentAccountId: "acc-cook",
			issueId: "iss-42",
			initialPrompt: "pick up SEA-1729",
		});
	});

	test("Cancel fires onCancel, not onSubmit", () => {
		let submitted = false;
		let cancelled = false;
		const { container } = render(() => (
			<StartAgentDialog
				spec={SPEC}
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

	test("Escape fires onCancel, not onSubmit", () => {
		let submitted = false;
		let cancelled = false;
		const { container } = render(() => (
			<StartAgentDialog
				spec={SPEC}
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

	test("focuses its own container on mount (Escape reachable from open)", () => {
		const { container } = render(() => (
			<StartAgentDialog spec={SPEC} onSubmit={() => {}} onCancel={() => {}} />
		));
		expect(document.activeElement).toBe(
			container.querySelector("[role='dialog']"),
		);
	});
});
