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
	type ShellState,
	shellState,
} from "./daemon-transport";
import type { ResolvedConnection } from "./live/provider";
import { type ShellMode, setShellServerUrl } from "./shell-globals";

export const REOPEN_MESSAGE =
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
	let queuedStateMessages: string[] = [];
	let connectController: AbortController | undefined;
	let resolveBoot:
		| ((value: ResolvedConnection | undefined) => void)
		| undefined;
	let rejectBoot: ((reason: unknown) => void) | undefined;
	let unsubscribe: () => void = () => {};
	const booted = new Promise<ResolvedConnection | undefined>(
		(resolve, reject) => {
			resolveBoot = resolve;
			rejectBoot = reject;
		},
	);
	const closeListener = (): void => unsubscribe();
	const showReopen = (): void => {
		terminal = true;
		connectController?.abort();
		closeListener();
		renderReopenScreen(root, deps.quitApp);
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
	// A queued decision always cancels an idle connect form; the form's own
	// abort handling lets an in-flight call finish first.
	const queueDecision = (message = ""): void => {
		decisionQueued = true;
		connectController?.abort();
		if (message.length > 0 && !queuedStateMessages.includes(message))
			queuedStateMessages.push(message);
	};
	const takeQueuedStateMessage = (fallback = ""): string => {
		if (fallback.length > 0 && !queuedStateMessages.includes(fallback))
			queuedStateMessages.push(fallback);
		const message = queuedStateMessages.join("\n");
		queuedStateMessages = [];
		return message;
	};
	const deferStateRead = (): boolean => {
		if (!stateReadInFlight && !chooseInFlight) return false;
		queueDecision();
		return true;
	};
	const applyShellState = async (
		current: ShellState,
		message: string,
	): Promise<void> => {
		if (terminal) return;
		if (chooseInFlight || connectController) {
			queueDecision(message);
			if (
				connectController &&
				(current.mode === "client" || current.mode === "reopen")
			)
				connectController.abort();
			return;
		}
		switch (current.mode) {
			case "client": {
				terminal = true;
				closeListener();
				setShellServerUrl(current.serverUrl);
				try {
					const connection = await deps.bootNativeClient(root, "configured");
					resolveBoot?.(connection);
				} catch (reason) {
					rejectBoot?.(reason);
				}
				return;
			}
			case "reopen":
				showReopen();
				return;
			case "setup":
			case "embedded":
				renderChoices(root, choose, connect, message);
				return;
			default: {
				const exhaustive: never = current.mode;
				throw new Error(`Unhandled shell mode: ${exhaustive}`);
			}
		}
	};
	const handleShellStateError = (reason: unknown): void => {
		if (terminal) {
			rejectBoot?.(reason);
			return;
		}
		if (chooseInFlight || connectController) {
			queueDecision(errorMessage(reason));
			return;
		}
		const message = takeQueuedStateMessage(errorMessage(reason));
		renderChoices(root, choose, connect, message);
		// A decision that arrived during the failed read still needs a read;
		// keep the error visible by carrying it into the retry.
		if (decisionQueued) queueDecision(message);
	};
	const finishShellStateRead = (): void => {
		stateReadInFlight = false;
		if (!decisionQueued || terminal || chooseInFlight || connectController)
			return;
		decisionQueued = false;
		const message = takeQueuedStateMessage();
		void readShellState(message);
	};
	const deferActiveShellState = (
		current: ShellState,
		message: string,
	): boolean => {
		if (!chooseInFlight && !connectController) return false;
		queueDecision(message);
		if (
			connectController &&
			(current.mode === "client" || current.mode === "reopen")
		)
			connectController.abort();
		return true;
	};
	const readShellState = async (message = ""): Promise<void> => {
		if (terminal) return;
		if (deferStateRead()) {
			queueDecision(message);
			return;
		}
		stateReadInFlight = true;
		try {
			const current = await deps.shellState();
			stateReadInFlight = false;
			if (deferActiveShellState(current, message)) return;
			const displayMessage = takeQueuedStateMessage(message);
			if (current.mode === "reopen") {
				showReopen();
				return;
			}
			await applyShellState(current, displayMessage);
		} catch (reason) {
			stateReadInFlight = false;
			handleShellStateError(reason);
		} finally {
			finishShellStateRead();
		}
	};
	const onDecision = (): void => {
		if (terminal) return;
		if (deferStateRead()) return;
		if (connectController) {
			decisionQueued = true;
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
		void setupConnection.then(
			(connection) => {
				if (terminal) return;
				if (connection) {
					connectController = undefined;
					terminal = true;
					closeListener();
					resolveBoot?.(connection);
				} else {
					connectController = undefined;
					if (decisionQueued) {
						decisionQueued = false;
						const message = takeQueuedStateMessage();
						void readShellState(message);
					}
				}
			},
			(reason: unknown) => {
				if (terminal) return;
				connectController = undefined;
				renderChoices(root, choose, connect, errorMessage(reason));
				if (decisionQueued) {
					decisionQueued = false;
					const message = takeQueuedStateMessage(errorMessage(reason));
					void readShellState(message);
				}
			},
		);
	};
	const handleEmbeddedChoice = async (): Promise<void> => {
		if (terminal || chooseInFlight) return;
		chooseInFlight = true;
		renderChecking(root);
		let result: SetupResult;
		try {
			result = await deps.chooseEmbedded();
		} catch (reason) {
			chooseInFlight = false;
			if (terminal) return;
			if (decisionQueued) {
				decisionQueued = false;
				const message = takeQueuedStateMessage(errorMessage(reason));
				await readShellState(message);
				return;
			}
			renderChoices(root, choose, connect, errorMessage(reason));
			return;
		} finally {
			chooseInFlight = false;
		}
		await applyEmbeddedResult(result);
	};
	choose = handleEmbeddedChoice;
	const applyEmbeddedResult = async (result: SetupResult): Promise<void> => {
		if (terminal) return;
		if (result.ok) {
			renderEmbeddedReady(result.message);
			return;
		}
		if (decisionQueued) {
			decisionQueued = false;
			const message = takeQueuedStateMessage(result.message);
			await readShellState(message);
			return;
		}
		if (result.message === REOPEN_MESSAGE) {
			showReopen();
			return;
		}
		renderChoices(root, choose, connect, result.message);
	};
	unsubscribe = deps.onSetupDecided(onDecision);
	await readShellState();
	return await booted;
}

function errorMessage(reason: unknown): string {
	return reason instanceof Error ? reason.message : String(reason);
}

export function renderReopenScreen(
	root: HTMLElement,
	quit: () => Promise<void>,
): void {
	renderTerminal(root, REOPEN_MESSAGE, quit);
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
