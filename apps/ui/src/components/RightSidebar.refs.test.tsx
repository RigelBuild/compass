import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { type Accessor, flush } from "solid-js";
import { STUB_COMMS_STATE } from "../comms-stub";
import { RefLinkRulesContext, StoreContext } from "../context";
import type { RefLinkRule } from "../refs/grammar";
import { type AppStore, createAppStore } from "../store";
import { STUB_ISSUES } from "../stub-data";
import { testQueryClient } from "../test-support";
import { RightSidebar } from "./RightSidebar";

const LINEAR_RIG: Accessor<readonly RefLinkRule[]> = () => [
	{
		kind: "linear",
		prefix: "RIG",
		urlTemplate: "https://linear.app/rigel/issue/{key}-{number}",
	},
];

// The detail head sits outside any button, so its refs must be real anchors.
describe("RightSidebar issue title refs", () => {
	test("the selected issue's title refs are focusable anchors", () => {
		const issue = STUB_ISSUES[0];
		if (!issue) throw new Error("no fixture issue");
		let store!: AppStore;
		const { container } = render(() => {
			store = createAppStore({
				initialComms: STUB_COMMS_STATE,
				queryClient: testQueryClient(),
				initialIssues: [{ ...issue, title: "Follow up RIG-12" }],
			});
			return (
				<StoreContext value={store}>
					<RefLinkRulesContext value={LINEAR_RIG}>
						<RightSidebar />
					</RefLinkRulesContext>
				</StoreContext>
			);
		});
		store.selectIssue(issue.id);
		store.setActiveRightTab("pr");
		flush();
		const link =
			container.querySelector<HTMLAnchorElement>(".r-detail-title a");
		expect(link?.getAttribute("href")).toBe(
			"https://linear.app/rigel/issue/RIG-12",
		);
		expect(link?.tabIndex).toBe(0);
	});
});
