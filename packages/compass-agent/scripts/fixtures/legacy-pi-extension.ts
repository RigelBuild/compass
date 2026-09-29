// biome-ignore-all lint/correctness/noUndeclaredDependencies: legacy extension peer imports are remapped by the compiled host registry.

import { Type as EarendilType } from "@earendil-works/pi-ai";
import { Type as MarioType } from "@mariozechner/pi-ai";
import { Agent } from "@oh-my-pi/pi-agent-core";

export default () => {
	if (!MarioType || !EarendilType || typeof Agent !== "function") {
		throw new Error("legacy Pi package imports did not resolve");
	}
};
