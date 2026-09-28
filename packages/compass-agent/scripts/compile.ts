import * as path from "node:path";
import { createLegacyPiModulesPlugin } from "./legacy-pi-modules-plugin";

const packageDirectory = path.resolve(import.meta.dir, "..");
const outfile = Bun.argv[2];
if (!outfile) throw new Error("Usage: bun scripts/compile.ts <outfile>");

const result = await Bun.build({
	entrypoints: [path.join(packageDirectory, "src/cli.ts")],
	root: packageDirectory,
	external: ["fastembed", "onnxruntime-node"],
	define: { "process.env.PI_COMPILED": JSON.stringify("true") },
	minify: { identifiers: false, keepNames: true },
	plugins: [await createLegacyPiModulesPlugin()],
	compile: {
		outfile,
		autoloadBunfig: false,
		autoloadDotenv: false,
		autoloadTsconfig: false,
		autoloadPackageJson: false,
	},
	throw: false,
});
if (!result.success) {
	throw new Error(
		`Compass agent binary bundle failed:\n${result.logs.map((log) => log.message).join("\n")}`,
	);
}
