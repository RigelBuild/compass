import { existsSync } from "node:fs";
import { basename, join } from "node:path";

/** The staged entrypoint symlink's name; the Dockerfile ENTRYPOINT must name it. */
export const ENTRYPOINT_LINK = ".compass-server";

/** The nonroot uid:gid the image must run as. */
export const IMAGE_USER = "65532:65532";

/** Return contract violations between a Dockerfile's text and the staged layout. */
export function dockerfileViolations(dockerfile: string): string[] {
	const lines = dockerfile.split("\n").map((line) => line.trim());
	// A later directive overrides an earlier one, so only the last of each counts.
	const last = (directive: string) =>
		lines.filter((line) => line.startsWith(`${directive} `)).at(-1);
	const violations: string[] = [];
	const entrypoint = `["/nix/store/${ENTRYPOINT_LINK}"]`;
	if (last("ENTRYPOINT") !== `ENTRYPOINT ${entrypoint}`) {
		violations.push(`last ENTRYPOINT must be ${entrypoint}`);
	}
	if (last("USER") !== `USER ${IMAGE_USER}`) {
		violations.push(`last USER must be ${IMAGE_USER}`);
	}
	return violations;
}

/** Resolve the server binary's relative path inside the staged store tree. */
export function entrypointTarget(server: string, storeDir: string): string {
	const storeName = basename(server);
	const target = join(storeName, "bin", "compass-server");
	if (!existsSync(join(storeDir, target))) {
		throw new Error(`server closure lacks bin/compass-server: ${server}`);
	}
	return target;
}
