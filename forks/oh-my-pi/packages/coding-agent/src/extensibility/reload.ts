/**
 * In-session roster reload — re-scan skills + rules from disk and swap the
 * process-global snapshots without tearing down the session.
 *
 * Everything the harness reads once at session-start is otherwise frozen for
 * that session's life. A mid-wave change to a skill or rule (a nix sync of a
 * new skill, an edited rulebook entry) is invisible to already-running sessions
 * until a full restart. This primitive re-runs the exact discovery pipeline
 * `createAgentSession` runs at init (`sdk.ts` `discoverSkills` +
 * `loadCapability(ruleCapability)` + `bucketRules`) and re-publishes the
 * `activeSkills`/`activeRules` globals with a pointer swap.
 *
 * The caller (`AgentSession.refresh`) is responsible for the parts a global
 * swap does NOT reach: the per-session `#skills` snapshot that `skill://`
 * actually binds (see `AgentSession.applyReloadedSkills`) and the system-prompt
 * rebuild that re-renders the advertised roster.
 */
import { getProjectDir } from "@oh-my-pi/pi-utils";
import { type Rule, ruleCapability, setActiveRules } from "../capability/rule";
import { bucketRules } from "../capability/rule-buckets";
import type { SkillsSettings } from "../config/settings";
import { loadCapability } from "../discovery";
import type { TtsrManager } from "../export/ttsr";
import { loadSkills, type Skill, setActiveSkills } from "./skills";

/**
 * Config surface(s) an in-session refresh re-reads from disk. Single-sourced:
 * the union type, the tool's arktype schema, and the slash-command validator
 * all derive from this one tuple, so adding a scope can't leave a runtime guard
 * silently stale.
 */
export const REFRESH_SCOPES = ["skills", "rules", "settings", "mcp", "all"] as const;
export type RefreshScope = (typeof REFRESH_SCOPES)[number];

/**
 * Outcome of an in-session refresh. Each field is populated only for the
 * surfaces the requested scope touched; an untouched surface stays `undefined`.
 */
export interface RefreshResult {
	/** Number of skills active after a roster reload. */
	skills?: number;
	/** Number of rules addressable via `rule://` after a roster reload. */
	rules?: number;
	/** Whether the merged settings view changed on a settings reload. */
	settingsChanged?: boolean;
	/** Whether the active default model was swapped on a settings reload. */
	modelSwapped?: boolean;
	/** Whether MCP servers were rediscovered and their tools rebound. `true` when the reconnect ran; `undefined` when no MCP manager existed. */
	mcp?: true;
	/**
	 * Set when the refresh was refused because a cooperative restart is in
	 * progress (`AgentSession.requestRestart()` has latched). Distinguishes a
	 * refusal from a successful no-op so `summarizeRefresh` can report it rather
	 * than collapsing it into "nothing to reload".
	 */
	refused?: "restarting";
}

/** Inputs for a roster reload, sourced from the live session/settings. */
export interface ReloadSkillsAndRulesOptions {
	/** Working directory for project-local skills/rules. Default: `getProjectDir()`. */
	cwd?: string;
	/** Skills settings group (`settings.getGroup("skills")`), as at session init. */
	skillsSettings?: SkillsSettings;
	/** Disabled extension ids (`settings.get("disabledExtensions")`). */
	disabledExtensions?: string[];
	/**
	 * The live session's TTSR manager. Reused (not replaced) so a rule reload
	 * preserves in-flight injected/trigger state. New TTSR rules register via
	 * `addRule`; an EDITED condition on an already-registered rule is not
	 * re-read (`addRule` is name-idempotent) — that sub-case still needs a
	 * restart. Rulebook/always-apply changes and brand-new rules are picked up.
	 */
	ttsrManager: TtsrManager;
	/** TTSR gating from `settings.getGroup("ttsr")` — mirrors `bucketRules` at init. */
	ttsrSettings?: { builtinRules?: boolean; disabledRules?: readonly string[] };
	/** Fresh skills, if the caller already discovered them (skips re-scan). */
	skills?: readonly Skill[];
}

/** Fresh roster produced by a reload — counts plus the swapped skills/rule buckets. */
export interface ReloadSkillsAndRulesResult {
	/** Number of skills now active. */
	skills: number;
	/** Number of rules addressable via `rule://` (rulebook + always + TTSR). */
	rules: number;
	/** The fresh skills, so the caller can fan them into per-session snapshots. */
	activeSkills: readonly Skill[];
	/** Fresh rulebook (described) rules, for threading into the prompt rebuild. */
	rulebookRules: Rule[];
	/** Fresh always-apply rules, for threading into the prompt rebuild. */
	alwaysApplyRules: Rule[];
}

/**
 * Re-scan skills + rules from disk and swap the `activeSkills`/`activeRules`
 * process globals. Pure re-READ: no config file is written.
 *
 * `rule://` resolution reads `getActiveRules()` directly, so the `setActiveRules`
 * swap self-heals a rule miss with no further work. `skill://` binds a
 * per-session snapshot instead, so the returned `activeSkills` must be threaded
 * into the live sessions by the caller (`AgentSession.applyReloadedSkills`).
 */
export async function reloadSkillsAndRules(options: ReloadSkillsAndRulesOptions): Promise<ReloadSkillsAndRulesResult> {
	const cwd = options.cwd ?? getProjectDir();

	// Skills: re-run the same discovery `sdk.ts` runs at init (`discoverSkills`
	// is a thin wrapper over `loadSkills`; called directly here to avoid a cycle
	// back through the sdk entry point). Only cwd + the skills settings matter.
	const skills =
		options.skills ??
		(
			await loadSkills({
				...options.skillsSettings,
				disabledExtensions: options.disabledExtensions,
				cwd,
			})
		).skills;
	setActiveSkills(skills);

	// Rules: re-scan the rules capability and re-bucket through the LIVE ttsr
	// manager (preserving injected state), exactly as `sdk.ts` does at init.
	const rulesResult = await loadCapability<Rule>(ruleCapability.id, { cwd });
	const { rulebookRules, alwaysApplyRules } = bucketRules(rulesResult.items, options.ttsrManager, {
		builtinRules: options.ttsrSettings?.builtinRules,
		disabledRules: options.ttsrSettings?.disabledRules,
	});
	const activeRules = [...rulebookRules, ...alwaysApplyRules, ...options.ttsrManager.getRules()];
	setActiveRules(activeRules);

	return {
		skills: skills.length,
		rules: activeRules.length,
		activeSkills: skills,
		rulebookRules,
		alwaysApplyRules,
	};
}
