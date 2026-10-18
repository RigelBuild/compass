import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const releaseWorkflow = readFileSync(
	join(import.meta.dir, "../../.github/workflows/release.yml"),
	"utf8",
);
const agentPublisher = readFileSync(
	join(import.meta.dir, "../../agent-image/publish.sh"),
	"utf8",
);

function joinContinuations(source: string): string {
	return source.replace(/\\\n[\t ]*/g, " ");
}

describe("registry auth sources", () => {
	test("release-image inspects anonymously and copies with destination-only auth", () => {
		const start = releaseWorkflow.indexOf("  release-image:");
		const end = releaseWorkflow.indexOf("  publish-runner-image:");
		const releaseJob = joinContinuations(releaseWorkflow.slice(start, end));
		const runBlocks =
			releaseJob.match(
				/\n {8}run: \|([\s\S]*?)(?=\n {6}- |\n {2}[a-z0-9-]+:|$)/g,
			) ?? [];
		const shell = joinContinuations(runBlocks.join("\n"));
		const calls = shell
			.split("\n")
			.map((line) => line.trim())
			.filter((line) =>
				/(?:skopeo inspect|skopeo copy|skopeo login|login ghcr.io)/.test(line),
			);
		const inspects = calls.filter((line) => line.includes("skopeo inspect"));
		const copies = calls.filter((line) => line.includes("skopeo copy"));
		const logins = calls.filter((line) => line.includes("login ghcr.io"));

		expect(inspects).toHaveLength(3);
		for (const line of inspects) {
			expect(line).toContain("--no-creds");
			expect(line).not.toContain("--authfile");
		}
		expect(copies).toHaveLength(1);
		expect(copies[0]).toContain("--src-no-creds");
		expect(copies[0]).toContain('--dest-authfile "$REGISTRY_AUTH_FILE"');
		expect(copies[0]).not.toContain("--authfile ");
		expect(logins).toHaveLength(1);
		expect(logins[0]).toContain('--authfile "$REGISTRY_AUTH_FILE"');
	});

	test("agent publish registry reads and copies use the specified auth boundary", () => {
		const source = joinContinuations(agentPublisher);
		const outsideGuard = source.replace(
			/guard_immutable\(\) \{[\s\S]*?\n\}/,
			"",
		);
		const inspectCalls = outsideGuard
			.split("\n")
			.filter(
				(line) =>
					/(?:inspect|inspect\s)/.test(line) && /\$dest|docker:\/\//.test(line),
			);
		for (const line of inspectCalls) expect(line).toContain("--no-creds");
		const copies = outsideGuard
			.split("\n")
			.filter((line) => /\bcopy\b/.test(line) && /\$dest/.test(line));
		expect(copies).toHaveLength(1);
		expect(copies[0]).toContain("--src-no-creds");
		expect(copies[0]).toContain('--dest-authfile "$REGISTRY_AUTH_FILE"');
		expect(copies[0]).not.toContain("--authfile ");
		const guard =
			source.match(/guard_immutable\(\) \{([\s\S]*?)\n\}/)?.[1] ?? "";
		const guardInspects = guard
			.split("\n")
			.filter((line) => /inspect/.test(line) && /\$dest/.test(line));
		expect(guardInspects).toHaveLength(1);
		expect(guardInspects[0]).toContain(
			'inspect --raw --authfile "$REGISTRY_AUTH_FILE"',
		);
		expect(guardInspects[0]).not.toContain("--no-creds");
	});

	test("release-notes image template reads anonymously", () => {
		const releaseNotes = readFileSync(
			join(import.meta.dir, "../release-notes/index.ts"),
			"utf8",
		);
		const gather =
			releaseNotes.match(/async function gatherImage[\s\S]*?\n\}/)?.[0] ?? "";
		expect(gather.includes("--no-creds --raw")).toBe(true);
		expect(gather.includes("docker://" + "$" + "{ref}")).toBe(true);
	});
});
