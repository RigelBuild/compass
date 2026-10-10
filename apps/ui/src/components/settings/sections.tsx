import type { JSX } from "@solidjs/web";
import type { SettingsSection } from "../../view-route";
import { GeneralSection } from "./GeneralSection";
import { ModelsSection } from "./ModelsSection";
import { TrackerSection } from "./TrackerSection";

export const SECTION_VIEW: Record<SettingsSection, () => JSX.Element> = {
	general: () => <GeneralSection />,
	tracker: () => <TrackerSection />,
	models: () => <ModelsSection />,
};
