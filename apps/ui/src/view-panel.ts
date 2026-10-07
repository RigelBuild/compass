export const viewPanelId = (viewId: string): string => `view-panel-${viewId}`;
export const viewTabId = (tabId: string): string => `view-tab-${tabId}`;

export function focusViewPanel(
	viewId: string,
	preferred?: HTMLElement,
): HTMLElement | undefined {
	const panel = document.getElementById(viewPanelId(viewId));
	if (!(panel instanceof HTMLElement) || panel.closest("[hidden], [inert]")) {
		return undefined;
	}
	const usable = (el: HTMLElement): boolean =>
		el.isConnected &&
		panel.contains(el) &&
		!el.matches(":disabled") &&
		el.closest("[hidden], [inert]") === null;
	const candidates = panel.querySelectorAll<HTMLElement>(
		'a[href], button, input, textarea, select, [tabindex]:not([tabindex="-1"])',
	);
	for (const target of [preferred, ...candidates]) {
		if (!target || !usable(target)) continue;
		target.focus();
		if (document.activeElement === target) return target;
	}
	panel.focus();
	return document.activeElement === panel ? panel : undefined;
}
