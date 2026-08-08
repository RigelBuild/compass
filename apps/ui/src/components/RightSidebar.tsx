import {
	type Component,
	createSignal,
	For,
	Match,
	Show,
	Switch,
} from "solid-js";
import {
	authorLabel,
	checkPip,
	isMultiForge,
	issueKey,
	prBadge,
	primaryPr,
} from "../board-render";
import type { Channel } from "../comms-stub";
import type { ActivityBarItem } from "../constants";
import { useStore } from "../context";
import {
	type Agent,
	type Check,
	type FileNode,
	type Issue,
	type IssueState,
	type PullRequest,
	STUB_FILES,
} from "../stub-data";
import { ChannelView } from "./ChannelView";
import { StateDot } from "./StateDot";

const FILE_ICON: Record<string, string> = { dir: "▸", file: "·" };
const STATUS_MARK: Record<string, string> = {
	modified: "M",
	added: "A",
	deleted: "D",
	untracked: "?",
};

/** A recursive file/dir row in the explorer. */
const FileRow: Component<{ node: FileNode; depth: number }> = (props) => (
	<>
		<div
			class="file-row"
			style={{ "padding-left": `${props.depth * 12 + 6}px` }}
		>
			<span class="f-icon">{FILE_ICON[props.node.kind]}</span>
			<span class="f-name">{props.node.name}</span>
			<Show when={props.node.status}>
				{(s) => (
					<span class="f-status" data-s={s()}>
						{STATUS_MARK[s()]}
					</span>
				)}
			</Show>
		</div>
		<Show when={props.node.children}>
			{(kids) => (
				<For each={kids()}>
					{(child) => <FileRow node={child} depth={props.depth + 1} />}
				</For>
			)}
		</Show>
	</>
);

/** Filter a file tree to nodes matching `query` (case-insensitive substring on
 *  the name). A directory whose OWN name matches is kept whole — it is itself
 *  the match, so its entire subtree stays as context. A directory that doesn't
 *  match is kept only when some descendant does, carrying just those matching
 *  descendants. An empty query returns the tree unchanged. */
export function filterFileTree(nodes: FileNode[], query: string): FileNode[] {
	const q = query.trim().toLowerCase();
	if (!q) return nodes;
	const walk = (node: FileNode): FileNode | null => {
		const selfMatch = node.name.toLowerCase().includes(q);
		const kids =
			node.children?.map(walk).filter((n): n is FileNode => n !== null) ?? [];
		if (selfMatch) return node;
		if (kids.length > 0) return { ...node, children: kids };
		return null;
	};
	return nodes.map(walk).filter((n): n is FileNode => n !== null);
}

/** The Files pane: a search box over the worktree file tree (design D5: "search
 *  is a top search box in the files view"), then the filtered tree. */
const FilesPane: Component<{ files: FileNode[] }> = (props) => {
	const [query, setQuery] = createSignal("");
	const shown = () => filterFileTree(props.files, query());
	return (
		<div class="r-files-pane">
			<input
				type="search"
				class="r-files-search"
				placeholder="Search files…"
				value={query()}
				onInput={(e) => setQuery(e.currentTarget.value)}
			/>
			<Show
				when={shown().length > 0}
				fallback={
					<p class="term-empty">
						{query().trim() ? "No files match." : "No file list in the stub."}
					</p>
				}
			>
				<div class="r-files">
					<For each={shown()}>
						{(node) => <FileRow node={node} depth={0} />}
					</For>
				</div>
			</Show>
		</div>
	);
};

/** The VCS pane body: branch, issue, the primary PR's changed-files count, the
 *  latest activity summary, and — at the bottom — the branch's recent commit
 *  history (design D5: "history is a commits menu at the bottom of the VCS
 *  pane"). The diff is a PR fact (DL-071 correction #4): the Changes row
 *  re-sources from the primary PR's `changed`, showing a dash when there is no
 *  PR diffstat. */
const VcsPane: Component<{ issue: Issue }> = (props) => {
	const store = useStore();
	const changed = () => primaryPr(props.issue)?.changed;
	return (
		<div class="r-pane-body">
			<div class="vcs-row">
				<span class="k">Branch</span>
				<span class="v">{props.issue.branch}</span>
			</div>
			<div class="vcs-row">
				<span class="k">Issue</span>
				<span class="v">
					{issueKey(props.issue, isMultiForge(store.issues()))}
				</span>
			</div>
			<div class="vcs-row">
				<span class="k">Changes</span>
				<span class="v">
					<Show when={changed()} fallback={<span class="sub">—</span>} keyed>
						{(c) => (
							<>
								{c.files} files <span class="add">+{c.additions}</span>{" "}
								<span class="del">−{c.deletions}</span>
							</>
						)}
					</Show>
				</span>
			</div>
			<div class="vcs-row">
				<span class="k">Summary</span>
				<span class="v" style={{ "white-space": "normal" }}>
					{props.issue.summary}
				</span>
			</div>
			<div class="vcs-commits">
				<div class="vcs-commits-head">History</div>
				<Show
					when={props.issue.commits && props.issue.commits.length > 0}
					fallback={<p class="term-empty">No commits in the stub.</p>}
				>
					<For each={props.issue.commits}>
						{(c) => (
							<div class="commit-row">
								<span class="commit-sha">{c.sha}</span>
								<span class="commit-subject">{c.subject}</span>
								<span class="commit-at">{c.at}</span>
							</div>
						)}
					</For>
				</Show>
			</div>
		</div>
	);
};

/** The PR check-run list — one pip + name + verdict per run. Shared by the
 *  Checks pane and the PR pane so the two can't drift in styling or markup.
 *  Each 6-valued forge `Check.state` maps to a 3-valued pip class via the shared
 *  `checkPip` map (DL-071). */
const CheckRuns: Component<{ checks: Check[] }> = (props) => (
	<div class="pr-checks">
		<For each={props.checks}>
			{(c) => {
				const pip = checkPip(c.state);
				return (
					<div class="pr-check">
						<span class="check-pip" data-status={pip} />
						<span class="c-name">{c.name}</span>
						<span class="c-verdict" data-status={pip}>
							{pip}
						</span>
					</div>
				);
			}}
		</For>
	</div>
);

/** The latest verdict per review author — `reviews` is submission-ordered, so a
 *  reviewer's current verdict is its last entry (DL-069). */
function latestVerdicts(
	reviews: readonly PullRequest["reviews"][number][],
): PullRequest["reviews"][number][] {
	const byAuthor = new Map<string, PullRequest["reviews"][number]>();
	for (const r of reviews) byAuthor.set(r.author, r);
	return [...byAuthor.values()];
}

/** The canonical (forge) verdict vocabulary → the existing chip key (DL-069):
 *  "changes_requested" → "changes", like the checks 6→3 map. */
const VERDICT_CHIP: Record<PullRequest["reviews"][number]["verdict"], string> =
	{
		approved: "approved",
		changes_requested: "changes",
		commented: "commented",
	};

/** The PR pane body: state badge, checks, bot reviews, thread progress. */
const PrPane: Component<{ pr: PullRequest }> = (props) => {
	const total = () => props.pr.threads.length;
	const resolved = () => props.pr.threads.filter((t) => t.resolved).length;
	const pct = () =>
		total() === 0 ? 100 : Math.round((resolved() / total()) * 100);
	const botReviews = () =>
		latestVerdicts(props.pr.reviews.filter((r) => r.isBot));
	return (
		<div class="pr-card">
			<div class="pr-title-row">
				<span class="pr-num">#{props.pr.number}</span>
				<span class="pr-state" data-state={prBadge(props.pr)}>
					{prBadge(props.pr)}
				</span>
			</div>
			<div class="pr-title">{props.pr.title}</div>
			<Show when={props.pr.agent}>
				{(agent) => <div class="pr-agent">{authorLabel(agent())}</div>}
			</Show>
			<Show when={props.pr.checks}>
				{(checks) => <CheckRuns checks={checks().checks} />}
			</Show>
			<Show when={botReviews().length > 0}>
				<div class="pr-reviews">
					<For each={botReviews()}>
						{(r) => (
							<span class="review-chip">
								{r.author}
								<span class="rv" data-v={VERDICT_CHIP[r.verdict]}>
									{r.verdict === "approved"
										? "✓"
										: r.verdict === "changes_requested"
											? "✗"
											: "•"}
								</span>
							</span>
						)}
					</For>
				</div>
			</Show>
			<div class="pr-threads">
				{resolved()}/{total()} threads resolved
				<div class="bar">
					<div class="bar-fill" style={{ width: `${pct()}%` }} />
				</div>
			</div>
		</div>
	);
};

/** The prompt the pane shows when no issue is selected. */
const SelectPrompt: Component = () => (
	<p class="muted">Select an issue to see its files, VCS, and PR.</p>
);

/** The issue detail header: the selected issue key + title, and the primary
 *  "Open agent" action that jumps into the assigned agent's view (design D10 —
 *  the button is the primary gesture; a card double-click is the accelerator).
 *  Hidden when the issue has no assignee (no jump target). */
const IssueDetailHead: Component<{ issue: Issue }> = (props) => {
	const store = useStore();
	return (
		<div class="r-detail-head">
			<div class="r-detail-meta">
				<span class="r-detail-issue">
					{issueKey(props.issue, isMultiForge(store.issues()))}
				</span>
				<span class="r-detail-title" title={props.issue.title}>
					{props.issue.title}
				</span>
			</div>
			<Show when={props.issue.assignee}>
				{(agentId) => (
					<button
						type="button"
						class="r-open-agent"
						title="Open the assigned agent's view"
						onClick={() => store.openAgent(agentId())}
					>
						Open agent
					</button>
				)}
			</Show>
		</div>
	);
};

/** The repo + branch dropdown (design D5's sole Compass-specific addition). A
 *  Compass agent's container may hold several clones, so this spans the agent's
 *  repos (`agentRepos`) and the branches within the active one. A single clone
 *  collapses the repo picker to a label; the branch dropdown always renders. */
const RepoBranchDropdown: Component = () => {
	const store = useStore();
	const [repoOpen, setRepoOpen] = createSignal(false);
	const [branchOpen, setBranchOpen] = createSignal(false);
	return (
		<Show when={store.activeRepo()}>
			{(repo) => (
				<div class="repo-branch">
					<Show
						when={store.agentRepos().length > 1}
						fallback={
							<div class="rb-repo-label" title={repo().name}>
								<span class="rb-icon" aria-hidden="true">
									🗀
								</span>
								<span class="rb-name">{repo().name}</span>
							</div>
						}
					>
						<div class="rb-dd">
							<button
								type="button"
								class="rb-dd-btn"
								title="Select repository"
								aria-label="Select repository"
								onClick={() => {
									setRepoOpen((v) => !v);
									setBranchOpen(false);
								}}
							>
								<span class="rb-icon" aria-hidden="true">
									🗀
								</span>
								<span class="rb-name">{repo().name}</span>
								<span class="caret" aria-hidden="true">
									▾
								</span>
							</button>
							<Show when={repoOpen()}>
								<div class="rb-menu">
									<For each={store.agentRepos()}>
										{(r) => (
											<button
												type="button"
												class="rb-item"
												classList={{ active: r.id === store.activeRepoId() }}
												onClick={() => {
													store.setActiveRepo(r.id);
													setRepoOpen(false);
												}}
											>
												<span class="rb-name">{r.name}</span>
											</button>
										)}
									</For>
								</div>
							</Show>
						</div>
					</Show>

					<div class="rb-dd">
						<button
							type="button"
							class="rb-dd-btn"
							title="Switch branch"
							aria-label="Switch branch"
							onClick={() => {
								setBranchOpen((v) => !v);
								setRepoOpen(false);
							}}
						>
							<span class="rb-icon" aria-hidden="true">
								⎇
							</span>
							<span class="rb-name">{repo().currentBranch}</span>
							<span class="caret" aria-hidden="true">
								▾
							</span>
						</button>
						<Show when={branchOpen() && repo().branches.length > 1}>
							<div class="rb-menu">
								<For each={repo().branches}>
									{(branch) => (
										<button
											type="button"
											class="rb-item"
											classList={{ active: branch === repo().currentBranch }}
											onClick={() => {
												store.setActiveBranch(branch);
												setBranchOpen(false);
											}}
										>
											<span class="rb-name">{branch}</span>
										</button>
									)}
								</For>
							</div>
						</Show>
					</div>
				</div>
			)}
		</Show>
	);
};

/** A fleet tab's pane (design compass-0.7). Renders the agent's home-DM
 *  conversation inline (asks answerable in place — first-responder-wins, per
 *  the ask-in-channel record), above a compact header control that opens the
 *  agent's full workspace via store.openAgent. Only rendered for a RESOLVABLE
 *  pin (SEA-1645 P2): the pane arm resolves reachability before choosing this
 *  vs the unreachable block, so there is no unresolved-agentId fallback here. */
const FleetPane: Component<{ item: ActivityBarItem }> = (props) => {
	const store = useStore();
	const agent = (): Agent | undefined =>
		props.item.agentId ? store.agentById(props.item.agentId) : undefined;
	return (
		<Show when={agent()}>
			{(a) => {
				const homeDm = (): Channel | undefined =>
					store.channels().find((c) => c.id === a().account.homeChannelId);
				return (
					<div class="fleet-pane">
						<div class="fleet-head">
							<button
								type="button"
								class="r-open-agent"
								title="Open this agent's workspace"
								onClick={() => store.openAgent(a().account.id)}
							>
								Open {a().account.handle}'s workspace
							</button>
						</div>
						<ChannelView channel={homeDm()} />
					</div>
				);
			}}
		</Show>
	);
};

/** An unreachable pin's pane (SEA-1645 P2): the pinned agent no longer resolves
 *  (dead / despawned / filtered out). Shows an empty-state message styled like
 *  the other `term-empty` panes and a WORKING unpin control — the only unpin
 *  affordance for an unreachable pin, whose left-tree row is gone (the tree
 *  renders the VISIBLE set). Unpinning routes through `store.unpinAgent`, which
 *  drops the pin and falls the active tab back to `status`. */
const AgentUnreachable: Component<{ item: ActivityBarItem }> = (props) => {
	const store = useStore();
	return (
		<div class="fleet-pane fleet-unreachable">
			<p class="term-empty">
				This pinned agent is unreachable — dead, despawned, or filtered out.
			</p>
			<button
				type="button"
				class="r-unpin-agent"
				// Both item builders (fleetItemForAgent, unreachableFleetItem) always set
				// agentId, and AgentUnreachable only renders for a pinned item read out of
				// rightTabGroups(), so it is never undefined here. Asserting (rather than
				// `?? ""`) surfaces a genuinely-empty agentId as a bug instead of silently
				// no-op-ing through unpinAgent("").
				// biome-ignore lint/style/noNonNullAssertion: guaranteed by both builders (see above)
				onClick={() => store.unpinAgent(props.item.agentId!)}
			>
				Unpin {props.item.title}
			</button>
		</div>
	);
};

/** The four fleet issue counts shown on the Status pane (design
 *  dock-in-sidebar T2). `active` sums both live states (`in_progress` +
 *  `in_review`); the others are single-state counts. States outside these
 *  buckets (`backlog`, `done`) are intentionally not surfaced here. */
export interface FleetMetrics {
	active: number;
	queued: number;
	todo: number;
	blocked: number;
}

/** Bucket an issue list into the Status pane's counts (design T2). Pure and
 *  exported for tests — the salvaged `countState` logic from the old dock
 *  facet, where "active" is the two in-flight states combined. */
export function fleetMetrics(issues: readonly Issue[]): FleetMetrics {
	const count = (...states: IssueState[]): number =>
		issues.filter((w) => states.includes(w.state)).length;
	return {
		active: count("in_progress", "in_review"),
		queued: count("queued"),
		todo: count("todo"),
		blocked: count("blocked"),
	};
}

/** The Status pane (design dock-in-sidebar D3/T2): the fleet metrics strip over
 *  every issue. Salvaged from the dock's Supervisor facet, full-width — no
 *  agent conversation, so this is the one fleet tab with no `agentId`. Works
 *  with no issue selected. */
const StatusPane: Component = () => {
	const store = useStore();
	const metrics = (): FleetMetrics => fleetMetrics(store.issues());
	return (
		<div class="r-status">
			<div class="r-status-metrics">
				<div class="r-status-metric">
					<span class="m-val">{metrics().active}</span>
					<span class="m-label">active</span>
				</div>
				<div class="r-status-metric">
					<span class="m-val">{metrics().queued}</span>
					<span class="m-label">queued</span>
				</div>
				<div class="r-status-metric">
					<span class="m-val">{metrics().todo}</span>
					<span class="m-label">todo</span>
				</div>
				<div class="r-status-metric">
					<span class="m-val" classList={{ del: metrics().blocked > 0 }}>
						{metrics().blocked}
					</span>
					<span class="m-label">blocked</span>
				</div>
			</div>
		</div>
	);
};

/** The right sidebar (design D5, dock-in-sidebar D2/D3/D5): an icon-per-tab
 *  activity bar mirroring Orca, grouped fleet-over-issue with a divider.
 *  Fleet tabs (Supervisor) render the agent's home-DM conversation
 *  inline (read-only asks) above an open-workspace control;
 *  issue tabs render the repo/branch dropdown + detail head above the pane
 *  for the active tab — Files (with a search box), VCS (changed files + commit
 *  history), or PR (with its checks inside). The card-scoped chrome is hidden
 *  while a fleet tab is active. */
export const RightSidebar: Component = () => {
	const store = useStore();
	const ws = store.selectedIssue;
	const files = (): FileNode[] => {
		const id = store.selectedIssueId();
		return id ? (STUB_FILES[id] ?? []) : [];
	};
	// D5: the card-scoped chrome (detail head + repo/branch dropdown) is
	// meaningless above a fleet conversation, so it hides when a fleet tab is
	// active. The active tab is fleet iff it's `status` or an `agent:`-prefixed
	// pin (the RIGHT_SIDEBAR_TAB_BY_ID index can't key the open `agent:` arm, so
	// this reads the shape, matching the rightTabGroups() partition).
	const fleetActive = (): boolean => {
		const active = store.activeRightTab();
		return active === "status" || active.startsWith("agent:");
	};
	// The active tab's fleet pane item, for any `agent:`-prefixed pin (SEA-1645
	// P2). The P1 `rightTabGroups()` memo already emits an item for EVERY pin
	// (marked unreachable or not) with the cached-handle title; read it out rather
	// than resolving/rebuilding a second time — this is the SINGLE
	// item-construction site. Never undefined for a pinned `agent:` tab (that was
	// the blank-pane gap); undefined only for a non-`agent:` tab or an `agent:`
	// tab with no matching pin (which falls through to `status`).
	const activeFleetItem = (): ActivityBarItem | undefined => {
		const active = store.activeRightTab();
		if (!active.startsWith("agent:")) return undefined;
		return store
			.rightTabGroups()
			.flatMap((g) => g.items)
			.find((i) => i.id === active);
	};

	return (
		<aside class="right" aria-label="Issue detail">
			<div class="r-shell">
				<div class="r-main">
					<Show when={!fleetActive()}>
						<Show when={ws()}>{(w) => <IssueDetailHead issue={w()} />}</Show>
						<RepoBranchDropdown />
					</Show>
					<div class="r-pane" classList={{ fleet: fleetActive() }}>
						<Switch>
							<Match when={activeFleetItem()}>
								{(item) => (
									<Show
										when={!item().unreachable}
										fallback={<AgentUnreachable item={item()} />}
									>
										<FleetPane item={item()} />
									</Show>
								)}
							</Match>

							{/* Defensive close (SEA-1645 P2): an `agent:` tab that matches no
							    pin in rightTabGroups() lands on status rather than a blank
							    pane. Unreachable via the current UI — setActiveRightTab is
							    only ever called with a pinned bar-item id, "status", or an
							    issue tab — but a future caller (or the live-agents migration)
							    that sets an unpinned agent tab can't strand the Switch. */}
							<Match when={store.activeRightTab().startsWith("agent:")}>
								<StatusPane />
							</Match>

							<Match when={store.activeRightTab() === "status"}>
								<StatusPane />
							</Match>

							<Match when={store.activeRightTab() === "files"}>
								<Show when={ws()} fallback={<SelectPrompt />}>
									<FilesPane files={files()} />
								</Show>
							</Match>

							<Match when={store.activeRightTab() === "vcs"}>
								<Show when={ws()} fallback={<SelectPrompt />}>
									{(w) => <VcsPane issue={w()} />}
								</Show>
							</Match>

							<Match when={store.activeRightTab() === "pr"}>
								<Show when={ws()} fallback={<SelectPrompt />}>
									{(w) => (
										<Show
											when={primaryPr(w())}
											fallback={<p class="term-empty">No PR opened yet.</p>}
										>
											{(pr) => (
												<div class="r-pane-body">
													<PrPane pr={pr()} />
												</div>
											)}
										</Show>
									)}
								</Show>
							</Match>
						</Switch>
					</div>
				</div>

				<nav class="r-activity" aria-label="Right sidebar tabs">
					<For each={store.rightTabGroups()}>
						{(group, groupIndex) => (
							<>
								<Show when={groupIndex() > 0}>
									<div class="r-activity-divider" aria-hidden="true" />
								</Show>
								<For each={group.items}>
									{(tab) => {
										const agent = (): Agent | undefined =>
											tab.agentId ? store.agentById(tab.agentId) : undefined;
										return (
											<button
												type="button"
												class="r-tab"
												classList={{
													active: store.activeRightTab() === tab.id,
													unreachable: tab.unreachable === true,
												}}
												title={
													tab.unreachable === true
														? `${tab.title} (unreachable)`
														: tab.title
												}
												aria-label={
													tab.unreachable === true
														? `${tab.title} (unreachable)`
														: tab.title
												}
												aria-pressed={store.activeRightTab() === tab.id}
												onClick={() => store.setActiveRightTab(tab.id)}
											>
												<span class="r-tab-icon" aria-hidden="true">
													{tab.icon}
												</span>
												<Show when={agent()}>
													{(a) => <StateDot state={a().lifecycle ?? "idle"} />}
												</Show>
											</button>
										);
									}}
								</For>
							</>
						)}
					</For>
				</nav>
			</div>
		</aside>
	);
};
