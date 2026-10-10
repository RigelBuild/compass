import { readFile } from "node:fs/promises";
import {
	parseRunnerDeployValues,
	renderRunnerManifests,
} from "./render-core.ts";

const valuesPath = Bun.argv[2];
if (valuesPath === undefined) {
	throw new Error("usage: bun run render.ts <values.json>");
}
const raw: unknown = JSON.parse(await readFile(valuesPath, "utf8"));
const values = parseRunnerDeployValues(raw);
// A JSON round-trip un-shares the label objects, which would otherwise render as YAML anchors.
const documents = renderRunnerManifests(values).map((manifest) =>
	Bun.YAML.stringify(JSON.parse(JSON.stringify(manifest)), null, 2),
);
process.stdout.write(`${documents.join("\n---\n")}\n`);
