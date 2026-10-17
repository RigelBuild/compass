import type { ImageLane } from "./retag-core.ts";

export type ImageLaneConfig = {
	lane: ImageLane;
	imageDir: `${ImageLane}-image`;
	projectName: `compass-${ImageLane}-image`;
	closureEnv: "RUNNER_IMAGE_CLOSURE_PATHS" | "SERVER_IMAGE_CLOSURE_PATHS";
};

const CONFIG: Record<ImageLane, ImageLaneConfig> = {
	runner: {
		lane: "runner",
		imageDir: "runner-image",
		projectName: "compass-runner-image",
		closureEnv: "RUNNER_IMAGE_CLOSURE_PATHS",
	},
	server: {
		lane: "server",
		imageDir: "server-image",
		projectName: "compass-server-image",
		closureEnv: "SERVER_IMAGE_CLOSURE_PATHS",
	},
};

/** Return lane-specific image paths and closure key, or undefined for invalid input. */
export function imageLaneConfig(
	lane: string | undefined,
): ImageLaneConfig | undefined {
	return lane === "runner" || lane === "server" ? CONFIG[lane] : undefined;
}
