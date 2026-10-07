// Native client boot connects through the shell, which keeps the bearer on its side.

import {
	BUTTON_STYLE,
	DETAIL_STYLE,
	HEADING_STYLE,
	INPUT_STYLE,
	SCREEN_STYLE,
	URL_STYLE,
} from "./boot-styles";
import {
	type ConnectResult,
	nativeConnectionProvider,
	pickCACert,
	type ServerChoice,
	shellConnect,
} from "./daemon-transport";
import type { ConnectionProvider, ResolvedConnection } from "./live/provider";
import { shellServerUrl } from "./shell-globals";

export type NativeBootDeps = {
	shellConnect: (
		token: string,
		server?: ServerChoice,
	) => Promise<ConnectResult>;
	pickCACert: () => Promise<{ ref: string; name: string }>;
	nativeConnectionProvider: (baseUrl: string) => ConnectionProvider;
};

const defaultNativeBootDeps: NativeBootDeps = {
	shellConnect,
	pickCACert,
	nativeConnectionProvider,
};

function failureCopy(result: ConnectResult): { heading: string; hint: string } {
	switch (result.kind) {
		case "bad-url":
			return {
				heading: "Can't reach the host",
				hint: "Check the server URL below is reachable, then paste your token and connect.",
			};
		case "bad-cert":
			return {
				heading: "Can't verify the server's certificate",
				hint: "Check the server certificate or choose its CA certificate, then try again.",
			};
		case "bad-token":
			return {
				heading: "The server rejected this token",
				hint: "Paste a valid token and connect.",
			};
		case "version-mismatch":
			return {
				heading: `App speaks compass.v1; server speaks ${result.apiVersion}`,
				hint: "The app and the server disagree on the API version — upgrade whichever is behind.",
			};
		case "invalid-url":
		case "invalid-ca":
		case "other":
			return { heading: "Could not connect", hint: result.message };
		case "":
			throw new Error("Successful connect results have no failure copy");
		default: {
			const exhaustive: never = result.kind;
			throw new Error(`Unhandled connect result kind: ${exhaustive}`);
		}
	}
}

/** Start the configured probe or open the first-run connection form. */
export async function bootNativeClient(
	root: HTMLElement,
	deps: NativeBootDeps = defaultNativeBootDeps,
	entry: "configured" | "setup" = "configured",
	signal?: AbortSignal,
): Promise<ResolvedConnection | undefined> {
	if (entry === "setup") return awaitUserSetupConnect(root, deps, signal);
	renderConnecting(root);
	const probe = await deps.shellConnect("");
	if (probe.ok) {
		root.replaceChildren();
		return deps.nativeConnectionProvider(shellServerUrl() ?? "").resolve();
	}
	return awaitUserConnect(root, probe, deps);
}

function renderConnecting(root: HTMLElement): void {
	const screen = document.createElement("div");
	screen.setAttribute("style", SCREEN_STYLE);
	const heading = document.createElement("h1");
	heading.setAttribute("style", HEADING_STYLE);
	heading.textContent = "Connecting…";
	const url = document.createElement("p");
	url.setAttribute("style", URL_STYLE);
	url.textContent = shellServerUrl() ?? "";
	screen.append(heading, url);
	root.replaceChildren(screen);
}

function awaitUserConnect(
	root: HTMLElement,
	initial: ConnectResult,
	deps: NativeBootDeps,
): Promise<ResolvedConnection> {
	return new Promise<ResolvedConnection>((resolve, reject) => {
		const screen = document.createElement("div");
		screen.setAttribute("style", SCREEN_STYLE);
		const heading = document.createElement("h1");
		heading.setAttribute("style", HEADING_STYLE);
		const detail = document.createElement("p");
		detail.setAttribute("style", DETAIL_STYLE);
		const url = document.createElement("p");
		url.setAttribute("style", URL_STYLE);
		url.textContent = `Server: ${shellServerUrl() ?? ""}`;
		const input = document.createElement("input");
		input.setAttribute("style", INPUT_STYLE);
		input.type = "password";
		input.placeholder = "Paste your token";
		input.autocomplete = "off";
		const button = document.createElement("button");
		button.setAttribute("style", BUTTON_STYLE);
		button.type = "button";
		button.textContent = "Connect";
		screen.append(heading, detail, url, input, button);

		const syncDisabled = (): void => {
			button.disabled = input.value.length === 0;
		};
		const paint = (result: ConnectResult): void => {
			const copy = failureCopy(result);
			heading.textContent = copy.heading;
			detail.textContent = copy.hint;
		};
		input.addEventListener("input", syncDisabled);
		paint(initial);
		syncDisabled();
		button.addEventListener("click", () => {
			const token = input.value;
			if (token.length === 0) return;
			button.disabled = true;
			input.value = "";
			void deps
				.shellConnect(token)
				.then(async (result) => {
					if (result.ok) {
						root.replaceChildren();
						try {
							resolve(
								await deps
									.nativeConnectionProvider(shellServerUrl() ?? "")
									.resolve(),
							);
						} catch (reason) {
							reject(reason);
						}
						return;
					}
					paint(result);
					syncDisabled();
				})
				.catch((reason: unknown) => {
					const message =
						reason instanceof Error ? reason.message : String(reason);
					heading.textContent = "Could not connect";
					detail.textContent = `Try again. ${message}`;
					button.disabled = false;
				});
		});
		root.replaceChildren(screen);
	});
}

function awaitUserSetupConnect(
	root: HTMLElement,
	deps: NativeBootDeps,
	signal?: AbortSignal,
): Promise<ResolvedConnection | undefined> {
	if (signal?.aborted) {
		root.replaceChildren();
		return Promise.resolve(undefined);
	}
	return new Promise<ResolvedConnection | undefined>((resolve, reject) => {
		const screen = document.createElement("div");
		screen.setAttribute("style", SCREEN_STYLE);
		const heading = document.createElement("h1");
		heading.setAttribute("style", HEADING_STYLE);
		heading.textContent = "Connect to a server";
		const detail = document.createElement("p");
		detail.setAttribute("style", DETAIL_STYLE);
		const url = document.createElement("input");
		url.setAttribute("style", INPUT_STYLE);
		url.type = "url";
		url.placeholder = "https://your-server.example";
		url.autocomplete = "off";
		const chooseCA = document.createElement("button");
		chooseCA.setAttribute("style", BUTTON_STYLE);
		chooseCA.type = "button";
		chooseCA.textContent = "Choose CA certificate…";
		const caRow = document.createElement("p");
		caRow.setAttribute("style", URL_STYLE);
		caRow.textContent = "System trust";
		const systemTrust = document.createElement("button");
		systemTrust.setAttribute("style", BUTTON_STYLE);
		systemTrust.type = "button";
		systemTrust.textContent = "Use system trust";
		const token = document.createElement("input");
		token.setAttribute("style", INPUT_STYLE);
		token.type = "password";
		token.placeholder = "Paste your token";
		token.autocomplete = "off";
		const submit = document.createElement("button");
		submit.setAttribute("style", BUTTON_STYLE);
		submit.type = "button";
		submit.textContent = "Connect";
		screen.append(
			heading,
			detail,
			url,
			chooseCA,
			caRow,
			systemTrust,
			token,
			submit,
		);

		let caRef = "";
		let shellCallInFlight = false;
		let finished = false;
		const finish = (connection: ResolvedConnection | undefined): void => {
			if (finished) return;
			finished = true;
			signal?.removeEventListener("abort", onAbort);
			resolve(connection);
		};
		const fail = (reason: unknown): void => {
			if (finished) return;
			finished = true;
			signal?.removeEventListener("abort", onAbort);
			reject(reason);
		};
		const onAbort = (): void => {
			if (signal?.aborted && !shellCallInFlight) {
				root.replaceChildren();
				finish(undefined);
			}
		};
		const syncDisabled = (): void => {
			submit.disabled =
				url.value.length === 0 || token.value.length === 0 || shellCallInFlight;
		};
		const paintResult = (result: ConnectResult): void => {
			const copy = failureCopy(result);
			heading.textContent = copy.heading;
			detail.textContent =
				result.kind === "bad-cert"
					? "Use Choose CA certificate… above to select the server's CA, then try again."
					: copy.hint;
		};
		const finishWithConnection = async (serverUrl: string): Promise<void> => {
			signal?.removeEventListener("abort", onAbort);
			root.replaceChildren();
			try {
				finish(await deps.nativeConnectionProvider(serverUrl).resolve());
			} catch (reason) {
				fail(reason);
			}
		};
		const handleConnectResult = async (
			result: ConnectResult,
		): Promise<void> => {
			shellCallInFlight = false;
			if (result.ok && result.serverUrl) {
				await finishWithConnection(result.serverUrl);
				return;
			}
			if (signal?.aborted) {
				root.replaceChildren();
				finish(undefined);
				return;
			}
			if (result.ok) {
				heading.textContent = "Could not connect";
				detail.textContent =
					"The server did not return its normalized server URL. Try again.";
			} else {
				paintResult(result);
			}
			syncDisabled();
		};
		const handleConnectRejection = (reason: unknown): void => {
			shellCallInFlight = false;
			if (signal?.aborted) {
				root.replaceChildren();
				finish(undefined);
				return;
			}
			const message = reason instanceof Error ? reason.message : String(reason);
			heading.textContent = "Could not connect";
			detail.textContent = `Try again. ${message}`;
			syncDisabled();
		};
		url.addEventListener("input", syncDisabled);
		token.addEventListener("input", syncDisabled);
		signal?.addEventListener("abort", onAbort, { once: true });
		chooseCA.addEventListener("click", () => {
			void deps.pickCACert().then(
				(picked) => {
					if (finished || signal?.aborted) return;
					if (picked.ref.length === 0) return;
					caRef = picked.ref;
					caRow.textContent = picked.name;
				},
				(reason: unknown) => {
					if (finished || signal?.aborted) return;
					detail.textContent =
						reason instanceof Error ? reason.message : String(reason);
				},
			);
		});
		systemTrust.addEventListener("click", () => {
			caRef = "";
			caRow.textContent = "System trust";
		});
		submit.addEventListener("click", () => {
			if (
				finished ||
				shellCallInFlight ||
				url.value.length === 0 ||
				token.value.length === 0
			)
				return;
			const server: ServerChoice = { url: url.value, caRef };
			const secret = token.value;
			token.value = "";
			shellCallInFlight = true;
			syncDisabled();
			void deps.shellConnect(secret, server).then(
				(result) => handleConnectResult(result),
				(reason: unknown) => handleConnectRejection(reason),
			);
		});
		root.replaceChildren(screen);
		syncDisabled();
		onAbort();
	});
}
