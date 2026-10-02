/// <reference types="bun" />
import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { bootBrowser, defaultBrowserBootDeps } from "./boot-browser";
import type { ResolvedConnection } from "./live/provider";

// The browser gate picks "boot", "ask for a token", or "let boot report it" from a
// WhoAmI probe. The probe here is the PRODUCTION one (real clients, real bearer
// interceptor, real ConnectError); only the network is faked, by a gRPC-Web fetch
// that answers WhoAmI for an accepted bearer and Unauthenticated (16, what the
// network door answers) otherwise.

const DOOR = "https://compass.example:8443";
const VALID = "good-token";
const KEY = `compass.token.${DOOR}`;

let presented: Array<string | null>;
let status: "auth" | "down";
let accepted: (auth: string | null) => boolean;

// WhoAmIResponse{account_id: "acc-matt"} as one gRPC-Web data frame plus an OK trailer.
function whoAmIBody(): Uint8Array<ArrayBuffer> {
	const id = new TextEncoder().encode("acc-matt");
	const msg = [0x0a, id.length, ...id];
	const trailer = new TextEncoder().encode("grpc-status: 0\r\n");
	const frame = (flag: number, bytes: ArrayLike<number>) => [
		flag,
		0,
		0,
		0,
		bytes.length,
		...Array.from(bytes),
	];
	return new Uint8Array([...frame(0, msg), ...frame(0x80, trailer)]);
}

const fakeDoor = (async (input: RequestInfo | URL, init?: RequestInit) => {
	const auth = new Headers(init?.headers).get("authorization");
	presented.push(auth);
	const headers = { "content-type": "application/grpc-web+proto" };
	if (status === "down") {
		return new Response(null, {
			status: 200,
			headers: { ...headers, "grpc-status": "14", "grpc-message": "door down" },
		});
	}
	if (!accepted(auth)) {
		return new Response(null, {
			status: 200,
			headers: { ...headers, "grpc-status": "16", "grpc-message": "nope" },
		});
	}
	void input;
	return new Response(whoAmIBody(), { status: 200, headers });
}) as typeof fetch;

const ENV: ResolvedConnection = {
	baseUrl: DOOR,
	token: undefined,
	fetchImpl: fakeDoor,
};

function deps(env: ResolvedConnection = ENV) {
	return {
		...defaultBrowserBootDeps,
		envConnectionProvider: () => ({ resolve: async () => env }),
	};
}

/** Submit `token` through the screen's form, as a user pressing Connect. */
function submit(root: HTMLElement, token: string): void {
	const input = root.querySelector("input");
	const form = root.querySelector("form");
	if (!input || !form) throw new Error("token screen is not up");
	input.value = token;
	input.dispatchEvent(new Event("input"));
	form.dispatchEvent(new Event("submit", { cancelable: true }));
}

/** Resolve once the screen shows `text`, woken by the gate's own DOM writes. */
function screenShows(root: HTMLElement, text: string): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	const check = () => {
		if (root.textContent?.includes(text)) {
			observer.disconnect();
			resolve();
		}
	};
	const observer = new MutationObserver(check);
	observer.observe(root, {
		childList: true,
		subtree: true,
		characterData: true,
	});
	check();
	return promise;
}

beforeEach(() => {
	presented = [];
	status = "auth";
	accepted = (auth) => auth === `Bearer ${VALID}`;
	localStorage.clear();
});
afterEach(() => localStorage.clear());

describe("bootBrowser", () => {
	test("a no-auth door boots with no prompt and no stored token", async () => {
		accepted = (auth) => auth === null;
		const root = document.createElement("div");

		const conn = await bootBrowser(root, deps());

		expect(conn?.token).toBeUndefined();
		expect(root.querySelector("input")).toBeNull();
		expect(localStorage.getItem(KEY)).toBeNull();
	});

	test("an auth door with no token shows the screen; a valid paste boots and is stored", async () => {
		const root = document.createElement("div");

		const booted = bootBrowser(root, deps());
		await screenShows(root, "This server needs a token");
		expect(root.textContent).toContain(DOOR);
		submit(root, `  ${VALID}  `);

		const conn = await booted;
		expect(conn?.token).toBe(VALID);
		expect(conn?.baseUrl).toBe(DOOR);
		expect(localStorage.getItem(KEY)).toBe(VALID);
		expect(presented).toEqual([null, `Bearer ${VALID}`]);
		// render() appends, so a leftover screen would sit above the mounted app.
		expect(root.childNodes.length).toBe(0);
	});

	test("a stored token is presented first and boots without the screen", async () => {
		localStorage.setItem(KEY, VALID);
		const root = document.createElement("div");

		const conn = await bootBrowser(root, deps());

		expect(conn?.token).toBe(VALID);
		expect(presented).toEqual([`Bearer ${VALID}`]);
		expect(root.querySelector("input")).toBeNull();
	});

	test("a rejected stored token is forgotten and the screen says so", async () => {
		localStorage.setItem(KEY, "revoked");
		const root = document.createElement("div");

		void bootBrowser(root, deps());
		await screenShows(root, "The server rejected this token");

		expect(localStorage.getItem(KEY)).toBeNull();
	});

	test("a wrong paste keeps the screen up, stores nothing, and clears the input", async () => {
		const root = document.createElement("div");

		const booted = bootBrowser(root, deps());
		await screenShows(root, "This server needs a token");
		submit(root, "wrong");
		await screenShows(root, "The server rejected this token");

		expect(localStorage.getItem(KEY)).toBeNull();
		expect(root.querySelector("input")?.value).toBe("");
		submit(root, VALID);
		expect((await booted)?.token).toBe(VALID);
	});

	test("a token stored for another door is never presented to this one", async () => {
		localStorage.setItem("compass.token.https://other.example:8443", VALID);
		const root = document.createElement("div");

		void bootBrowser(root, deps());
		await screenShows(root, "This server needs a token");

		expect(presented).toEqual([null]);
	});

	test("a non-auth probe failure hands the connection on without a prompt", async () => {
		status = "down";
		const root = document.createElement("div");

		const conn = await bootBrowser(root, deps());

		expect(conn?.baseUrl).toBe(DOOR);
		expect(root.querySelector("input")).toBeNull();
	});

	test("a build-time bearer is used when nothing is stored", async () => {
		const root = document.createElement("div");

		const conn = await bootBrowser(root, deps({ ...ENV, token: VALID }));

		expect(conn?.token).toBe(VALID);
		expect(presented).toEqual([`Bearer ${VALID}`]);
	});

	test("a stored token wins over the build-time bearer", async () => {
		localStorage.setItem(KEY, VALID);
		const root = document.createElement("div");

		const conn = await bootBrowser(root, deps({ ...ENV, token: "baked" }));

		expect(conn?.token).toBe(VALID);
		expect(presented).toEqual([`Bearer ${VALID}`]);
	});

	test("a missing door paints the env failure and returns undefined", async () => {
		const root = document.createElement("div");

		const conn = await bootBrowser(root, {
			...defaultBrowserBootDeps,
			envConnectionProvider: () => ({
				resolve: async () => {
					throw new Error("VITE_COMPASS_BASE_URL is required");
				},
			}),
		});

		expect(conn).toBeUndefined();
		expect(root.textContent).toContain("VITE_COMPASS_BASE_URL");
	});
});
