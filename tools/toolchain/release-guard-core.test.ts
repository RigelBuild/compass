// This test is CAPABLE OF FAILING: an absent or unparseable job block must fail
// rather than turn an uncheckable release guard into a pass.

import { describe, expect, test } from "bun:test";

const releasePrBlock = (workflow: string): string => {
	const matches = workflow.match(
		/^ {2}release-pr:\n([\s\S]*?)(?=^ {2}\S|(?![\s\S]))/m,
	);
	if (matches === null)
		throw new Error(
			"release-pr block could not be located; the privileged contents-write tag-minting job is unchecked",
		);
	expect(
		matches,
		"release-pr must be present so the privileged contents-write tag-minting job stays guarded",
	).toHaveLength(2);
	const [, block] = matches;
	if (block === undefined)
		throw new Error(
			"release-pr block could not be extracted; the privileged contents-write tag-minting job is unchecked",
		);
	expect(
		block.length,
		"release-pr must contain a substantial job block so the privileged contents-write tag-minting job is actually checked",
	).toBeGreaterThan(100);
	return block;
};

const readReleasePrBlock = async (): Promise<string> => {
	const root = new URL("../../", import.meta.url).pathname;
	return releasePrBlock(
		await Bun.file(`${root}.github/workflows/release.yml`).text(),
	);
};

describe("the release-pr main-ref guard", () => {
	test("keeps the privileged tag-minting job restricted to main", async () => {
		const block = await readReleasePrBlock();
		expect(
			block,
			"release-pr must require main because it holds contents: write and mints release tags",
		).toMatch(/^ {4}if: github\.ref == 'refs\/heads\/main'$/m);
	});

	test("pins release-please to the main target branch", async () => {
		const block = await readReleasePrBlock();
		expect(
			block,
			"release-pr must pass target-branch: main because it holds contents: write and mints release tags",
		).toMatch(/^ {10}target-branch: main$/m);
	});
});
