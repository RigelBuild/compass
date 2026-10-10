export type ReduceMotion = "system" | "on";

export const REDUCE_MOTION_KEY = "compass.settings.reduceMotion";

export function loadReduceMotion(storage: Storage | undefined): ReduceMotion {
	if (!storage) return "system";
	try {
		return storage.getItem(REDUCE_MOTION_KEY) === "on" ? "on" : "system";
	} catch {
		return "system";
	}
}

export function saveReduceMotion(
	storage: Storage | undefined,
	value: ReduceMotion,
): void {
	if (!storage) return;
	try {
		storage.setItem(REDUCE_MOTION_KEY, value);
	} catch {
		// Persistence is best-effort in privacy-locked contexts.
	}
}

export function applyReduceMotion(
	root: HTMLElement,
	value: ReduceMotion,
): void {
	if (value === "on") {
		root.setAttribute("data-reduce", "on");
	} else {
		root.removeAttribute("data-reduce");
	}
}
