import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import { createAppStore } from "../store";
import { STUB_ISSUES } from "../stub-data";
import { testQueryClient } from "../test-support";
import { IssueCard } from "./IssueCard";

// The advancing-card dormant hook (RIG-2111 T5, D2). The chase-light CSS is
// exercised end-to-end in e2e/advancing-hook.spec.ts; this unit test defends the
// PROP SEAM that a future store accessor will drive: `advancing` toggles
// `data-advancing="1"` on the card (presence attribute, like `data-selected`),
// and is absent by default so the hook ships dormant. An inverted or dropped
// toggle would otherwise ship green — no caller passes the prop today.
function mountCard(advancing?: boolean): HTMLElement {
	const issue = STUB_ISSUES[0];
	if (!issue) throw new Error("no fixture issue");
	const { container } = render(() => {
		const store = createAppStore({ queryClient: testQueryClient() });
		return (
			<StoreContext.Provider value={store}>
				<IssueCard issue={issue} advancing={advancing} />
			</StoreContext.Provider>
		);
	});
	const card = container.querySelector(".cx-card");
	if (!card) throw new Error("card did not render");
	return card as HTMLElement;
}

describe("IssueCard advancing seam (T5, D2)", () => {
	test('advancing renders data-advancing="1"', () => {
		const card = mountCard(true);
		expect(card.getAttribute("data-advancing")).toBe("1");
	});

	test("is dormant by default: no data-advancing without the prop", () => {
		const card = mountCard();
		expect(card.hasAttribute("data-advancing")).toBe(false);
	});

	test("advancing={false} does not render the attribute", () => {
		const card = mountCard(false);
		expect(card.hasAttribute("data-advancing")).toBe(false);
	});
});
