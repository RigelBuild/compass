import { readFile } from "node:fs/promises";
import {
	parseServerDeployValues,
	renderServerManifests,
} from "./render-core.ts";

const valuesPath = Bun.argv[2];
if (valuesPath === undefined) {
	throw new Error("usage: bun run render.ts <values.json>");
}
const raw: unknown = JSON.parse(await readFile(valuesPath, "utf8"));
const values = parseServerDeployValues(raw);
// A JSON round-trip un-shares label objects before Bun serializes the YAML.
const documents = renderServerManifests(values).map((manifest) =>
	Bun.YAML.stringify(JSON.parse(JSON.stringify(manifest)), null, 2),
);
process.stdout.write(`${documents.join("\n---\n")}\n`);
