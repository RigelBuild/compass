import * as path from "node:path";
import { createLegacyPiModulesPlugin } from "./legacy-pi-modules-plugin";

export interface CompileAgentOptions {
	readonly entrypoint?: string;
	readonly outfile: string;
}

export async function compileAgentBinary(
	options: CompileAgentOptions,
): Promise<void> {
	const packageDirectory = path.resolve(import.meta.dir, "..");
	const workspaceRoot = path.resolve(packageDirectory, "../..");
	const entrypoint =
		options.entrypoint ?? path.join(packageDirectory, "src/cli.ts");
	const result = await Bun.build({
		entrypoints: [entrypoint],
		root: workspaceRoot,
		external: ["fastembed", "onnxruntime-node"],
		define: { "process.env.PI_COMPILED": JSON.stringify("true") },
		minify: { identifiers: false, keepNames: true },
		plugins: [await createLegacyPiModulesPlugin()],
		compile: {
			outfile: options.outfile,
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
}

if (import.meta.main) {
	const outfile = Bun.env.COMPASS_AGENT_OUTFILE;
	if (!outfile)
		throw new Error(
			"COMPASS_AGENT_OUTFILE must name the compiled binary output path",
		);
	await compileAgentBinary({ outfile });
}
