import type { JSX } from "@solidjs/web";
import type { SettingsSection } from "../../view-route";
import { AppearanceSection } from "./AppearanceSection";
import { GeneralSection } from "./GeneralSection";
import { ModelsSection } from "./ModelsSection";
import { TrackerSection } from "./TrackerSection";

export const SECTION_VIEW: Record<SettingsSection, () => JSX.Element> = {
	general: () => <GeneralSection />,
	appearance: () => <AppearanceSection />,
	tracker: () => <TrackerSection />,
	models: () => <ModelsSection />,
};
