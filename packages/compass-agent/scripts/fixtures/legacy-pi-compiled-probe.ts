// biome-ignore-all lint/correctness/noUndeclaredDependencies: the compiled probe imports transitive SDK modules directly.
import { EditTool } from "@oh-my-pi/pi-coding-agent/edit";
import { loadExtensions } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/loader";

const extensionPath = process.env.LEGACY_PI_EXTENSION_PATH;
if (!extensionPath) throw new Error("LEGACY_PI_EXTENSION_PATH is required");

const session = {
	cwd: process.cwd(),
	enableLsp: false,
	settings: {
		get(key: string) {
			return key === "edit.fuzzyThreshold" ? 0.5 : false;
		},
	},
};
const hashlineGrammar = new EditTool(session, "hashline").customFormat
	?.definition;
if (!hashlineGrammar) throw new Error("hashline grammar was not loaded");

const result = await loadExtensions([extensionPath], process.cwd());
console.log(
	JSON.stringify({
		loaded: result.extensions.length,
		errors: result.errors,
		hashlineGrammar,
	}),
);
