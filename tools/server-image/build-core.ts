import { existsSync } from "node:fs";
import { basename, join } from "node:path";

/** Resolve the server binary's relative path inside the staged store tree. */
export function entrypointTarget(server: string, storeDir: string): string {
	const storeName = basename(server);
	const target = join(storeName, "bin", "compass-server");
	if (!existsSync(join(storeDir, target))) {
		throw new Error(`server closure lacks bin/compass-server: ${server}`);
	}
	return target;
}
