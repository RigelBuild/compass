import type { JSX } from "@solidjs/web";
import type { SettingsSection } from "../../view-route";
import { AppearanceSection } from "./AppearanceSection";
import { ForgeMirrorSection } from "./ForgeMirrorSection";
import { GeneralSection } from "./GeneralSection";
import { ModelsSection } from "./ModelsSection";

export const SECTION_VIEW: Record<SettingsSection, () => JSX.Element> = {
	general: () => <GeneralSection />,
	appearance: () => <AppearanceSection />,
	models: () => <ModelsSection />,
	"issue-mirroring": () => <ForgeMirrorSection />,
};
