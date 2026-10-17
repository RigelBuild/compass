// Runs the package scan in a child so configs.test.ts can hand it a scrubbed env.
import { gitGrep, loadRefGateConfig } from "@rigelbuild/ref-gate";

const [root, configPath] = Bun.argv.slice(2);
if (!root || !configPath)
	throw new Error("usage: scan.fixture.ts <root> <config>");
console.log(
	JSON.stringify(gitGrep(root, loadRefGateConfig(configPath).prefilter)),
);
