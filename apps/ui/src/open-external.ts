import { Browser } from "@wailsio/runtime";

function isNativeShell(): boolean {
	if (typeof window === "undefined" || !("_wails" in window)) return false;
	const wails = window._wails;
	return typeof wails === "object" && wails !== null && "environment" in wails;
}

export function openExternal(href: string): void {
	if (isNativeShell()) {
		void Browser.OpenURL(href).catch(() => {});
		return;
	}
	window.open(href, "_blank", "noreferrer,noopener");
}
