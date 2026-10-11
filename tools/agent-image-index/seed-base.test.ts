import { describe, expect, test } from "bun:test";
import { classifyManifestStatus, findPublishedBase } from "./seed-base.ts";

const A = "a".repeat(40);
const B = "b".repeat(40);
const C = "c".repeat(40);

describe("findPublishedBase", () => {
	test("returns the newest commit whose tag is published, skipping unpublished ones", async () => {
		const published = new Set([B.slice(0, 12), C.slice(0, 12)]);
		const probed: string[] = [];
		const base = await findPublishedBase([A, B, C], async (sha12) => {
			probed.push(sha12);
			return published.has(sha12);
		});
		expect(base).toBe(B);
		expect(probed).toEqual([A.slice(0, 12), B.slice(0, 12)]);
	});

	test("returns empty when nothing in reach is published", async () => {
		expect(await findPublishedBase([A, B], async () => false)).toBe("");
	});

	test("an ambiguous probe stops the walk instead of skipping past it", async () => {
		const walk = findPublishedBase([A, B], async (sha12) =>
			classifyManifestStatus(sha12 === A.slice(0, 12) ? 503 : 200, sha12),
		);
		await expect(walk).rejects.toThrow(/ambiguous registry answer 503/);
	});
});

describe("classifyManifestStatus", () => {
	test("200 is present and 404 is absent", () => {
		expect(classifyManifestStatus(200, "x")).toBe(true);
		expect(classifyManifestStatus(404, "x")).toBe(false);
	});

	test("auth and server errors are not read as absent", () => {
		for (const status of [401, 403, 429, 500]) {
			expect(() => classifyManifestStatus(status, "x")).toThrow(/ambiguous/);
		}
	});
});
