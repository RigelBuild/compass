import { bootNativeClient } from "./boot-native";
import {
	BUTTON_STYLE,
	DETAIL_STYLE,
	HEADING_STYLE,
	SCREEN_STYLE,
} from "./boot-styles";
import {
	chooseEmbedded,
	onSetupDecided,
	quitApp,
	type SetupResult,
	shellState,
} from "./daemon-transport";
import type { ResolvedConnection } from "./live/provider";
import type { ShellMode } from "./shell-globals";

const ALREADY_SET_UP =
	"Compass is already set up. Quit and reopen it to change this.";

export type SetupBootDeps = {
	chooseEmbedded: () => Promise<SetupResult>;
	shellState: () => Promise<{ mode: ShellMode; serverUrl: string }>;
	onSetupDecided: (fn: () => void) => () => void;
	quitApp: () => Promise<void>;
	bootNativeClient: (
		root: HTMLElement,
		entry: "setup" | "configured",
		signal?: AbortSignal,
	) => Promise<ResolvedConnection | undefined>;
};

const defaultSetupBootDeps: SetupBootDeps = {
	chooseEmbedded,
	shellState,
	onSetupDecided,
	quitApp,
	bootNativeClient: (root, entry, signal) =>
		bootNativeClient(root, undefined, entry, signal),
};

/** Show first-run choices and follow decisions made by other windows. */
export async function bootSetup(
	root: HTMLElement,
	deps: SetupBootDeps = defaultSetupBootDeps,
): Promise<ResolvedConnection | undefined> {
	let terminal = false;
	let chooseInFlight = false;
	let stateReadInFlight = false;
	let decisionQueued = false;
	let connectController: AbortController | undefined;
	let resolveBoot:
		| ((value: ResolvedConnection | undefined) => void)
		| undefined;
	let unsubscribe: () => void = () => {};

	const booted = new Promise<ResolvedConnection | undefined>((resolve) => {
		resolveBoot = resolve;
	});

	const closeListener = (): void => {
		unsubscribe();
	};

	const renderReopen = (): void => {
		terminal = true;
		closeListener();
		renderTerminal(root, ALREADY_SET_UP, deps.quitApp);
		resolveBoot?.(undefined);
	};

	const renderEmbeddedReady = (message: string): void => {
		terminal = true;
		closeListener();
		renderTerminal(root, message, deps.quitApp);
		resolveBoot?.(undefined);
	};

	let choose: () => Promise<void>;
	let connect: () => void;

	const readShellState = async (): Promise<void> => {
		if (terminal || stateReadInFlight || chooseInFlight) {
			decisionQueued = true;
			return;
		}
		stateReadInFlight = true;
		try {
			const current = await deps.shellState();
			if (terminal) return;
			switch (current.mode) {
				case "client": {
					terminal = true;
					closeListener();
					connectController?.abort();
					const connection = await deps.bootNativeClient(root, "configured");
					resolveBoot?.(connection);
					return;
				}
				case "reopen":
					renderReopen();
					return;
				case "setup":
				case "embedded":
					renderChoices(root, choose, connect);
					return;
				default: {
					const exhaustive: never = current.mode;
					throw new Error(`Unhandled shell mode: ${exhaustive}`);
				}
			}
		} finally {
			stateReadInFlight = false;
			if (decisionQueued && !terminal && !chooseInFlight) {
				decisionQueued = false;
				void readShellState();
			}
		}
	};

	const onDecision = (): void => {
		if (terminal) return;
		if (chooseInFlight || stateReadInFlight) {
			decisionQueued = true;
			return;
		}
		if (connectController) {
			connectController.abort();
			return;
		}
		void readShellState();
	};

	connect = (): void => {
		if (terminal || connectController) return;
		connectController = new AbortController();
		const setupConnection = deps.bootNativeClient(
			root,
			"setup",
			connectController.signal,
		);
		void setupConnection.then(async (connection) => {
			if (terminal) return;
			if (connection) {
				terminal = true;
				closeListener();
				resolveBoot?.(connection);
				return;
			}
			connectController = undefined;
			await readShellState();
		});
	};

	choose = async (): Promise<void> => {
		if (terminal || chooseInFlight) return;
		chooseInFlight = true;
		decisionQueued = false;
		renderChecking(root);
		let result: SetupResult;
		try {
			result = await deps.chooseEmbedded();
		} finally {
			chooseInFlight = false;
		}
		if (terminal) return;
		if (result.ok) {
			renderEmbeddedReady(result.message);
			return;
		}
		if (decisionQueued) {
			decisionQueued = false;
			await readShellState();
			return;
		}
		if (result.message === ALREADY_SET_UP) {
			renderReopen();
			return;
		}
		renderChoices(root, choose, connect, result.message);
	};

	unsubscribe = deps.onSetupDecided(onDecision);
	await readShellState();
	return await booted;
}

function renderChoices(
	root: HTMLElement,
	choose: () => Promise<void>,
	connect: () => void,
	message = "",
): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const heading = document.createElement("h1");
	heading.setAttribute("style", HEADING_STYLE);
	heading.textContent = "Set up Compass";
	const detail = document.createElement("p");
	detail.setAttribute("style", DETAIL_STYLE);
	detail.textContent = message;
	const embedded = document.createElement("button");
	embedded.setAttribute("style", BUTTON_STYLE);
	embedded.type = "button";
	embedded.textContent = "Run Compass on this computer";
	embedded.addEventListener("click", () => void choose());
	const connectButton = document.createElement("button");
	connectButton.setAttribute("style", BUTTON_STYLE);
	connectButton.type = "button";
	connectButton.textContent = "Connect to a server";
	connectButton.addEventListener("click", connect);
	screen.append(heading, detail, embedded, connectButton);
	root.replaceChildren(screen);
}

function renderChecking(root: HTMLElement): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const detail = document.createElement("p");
	detail.textContent =
		"Checking this computer… The first check on a Mac can take several minutes.";
	screen.append(detail);
	root.replaceChildren(screen);
}

function renderTerminal(
	root: HTMLElement,
	message: string,
	quit: () => Promise<void>,
): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const detail = document.createElement("p");
	detail.textContent = message;
	const button = document.createElement("button");
	button.setAttribute("style", BUTTON_STYLE);
	button.type = "button";
	button.textContent = "Quit";
	button.addEventListener("click", () => void quit());
	screen.append(detail, button);
	root.replaceChildren(screen);
}
