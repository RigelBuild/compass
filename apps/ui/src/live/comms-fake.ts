// A hand-written CommsClient double for driving the store's live comms path
// without a server — the read side (a scripted SubscribeComms stream plus the
// four snapshot read RPCs) and the write side (PostMessage / RespondToAsk
// recorded verbatim, so a test asserts the exact wire request the UI issued).
//
// The write side also enforces the two server invariants a permissive double
// would hide: PostMessage dedups on clientRequestId, and an ask is answerable
// exactly ONCE (a second RespondToAsk throws, as the server's ErrConflict does).
//
// It lives beside the live layer rather than inline in one suite because the
// store's live behavior is asserted from several angles (the store's own
// reduction, the channel composer, the thread composer) and all three must
// agree on ONE definition of "what the server looks like". stream.test.ts keeps
// its own protocol-level fake: that suite drives cursors and reconnects, which
// this double deliberately does not model.
//
// Dev/test-only, like comms-stub.ts — nothing in the shipped app imports it.

import {
	AccountSchema,
	AskOptionSchema,
	AskQuestionSchema,
	AskSchema,
	ChannelKind,
	ChannelSchema,
	type CommsClient,
	create,
	MessageBlockSchema,
	MessageSchema,
	UserAccountSchema,
	type Account as WireAccount,
	type Channel as WireChannel,
	type Message as WireMessage,
} from "@compass/client";

/** One recorded PostMessage — the fields the UI is contractually required to
 *  set. `parentMessageId` is "" for a root post (the wire's unset convention). */
export interface RecordedPost {
	readonly channelId: string;
	readonly text: string;
	readonly parentMessageId: string;
	readonly clientRequestId: string;
}

/** One recorded RespondToAsk: the ask plus every question's answer, in the wire
 *  order the UI sent them (the RPC is atomic — one answer per question). */
export interface RecordedAskResponse {
	readonly askId: string;
	readonly answers: ReadonlyArray<{
		readonly questionId: string;
		readonly chosenOptionIds: readonly string[];
	}>;
}

export interface FakeComms {
	readonly client: CommsClient;
	/** Every PostMessage the UI issued, in order. */
	readonly posts: RecordedPost[];
	/** Every RespondToAsk the UI issued, in order. */
	readonly askResponses: RecordedAskResponse[];
	/** Whether a SubscribeComms stream is currently live. The observable for the
	 *  store's teardown contract: after the owner is disposed this goes false,
	 *  because the driver's abort ends the generator. */
	isStreaming: () => boolean;
	/** Push one more SubscribeComms response to the driver's tail, resolving once
	 *  the driver has consumed it. The value is the response's payload oneof. */
	emit: (payload: unknown, seq: bigint) => Promise<void>;
	/** Close the scripted stream so `runCommsStream` stops tailing it. */
	close: () => void;
	/** Reject the next PostMessage with `error` (one-shot) — the composer's
	 *  keep-the-user's-text path. */
	failNextPost: (error: Error) => void;
	/** Reject the next RespondToAsk with `error` (one-shot) — the ask's
	 *  server-refused path. Thrown BEFORE the answered-once bookkeeping, so a
	 *  refused respond does not burn the ask: the server only flips
	 *  `Ask.Answered` on a respond it accepted, and a UI that rolls its local
	 *  answer back must be able to retry against a still-unanswered ask. */
	failNextAskResponse: (error: Error) => void;
	/** Hold the next RespondToAsk IN FLIGHT until the returned gate is settled —
	 *  the same refusal as `failNextAskResponse`, with the timing under the
	 *  test's control. That is what lets a test interleave a SubscribeComms push
	 *  between the click and the refusal, which is the only way to observe
	 *  whether a rollback clobbers a newer server value. `reject` refuses the
	 *  respond (before the answered-once bookkeeping, exactly as
	 *  `failNextAskResponse` does); `accept` lets it through to the normal path. */
	holdNextAskResponse: () => AskResponseGate;
}

/** The handle on one held-in-flight RespondToAsk. Settle it exactly once. */
export interface AskResponseGate {
	/** Refuse the held respond with `error`. */
	readonly reject: (error: Error) => void;
	/** Release the held respond into the fake's normal accept path. */
	readonly accept: () => void;
}

/** What the fake serves from its snapshot read RPCs. Wire-typed for accounts /
 *  groups / channels (reduceSnapshot adapts them) and messages (the injected
 *  mapper adapts them), exactly as the real client returns. */
export interface FakeCommsSnapshot {
	readonly accounts?: readonly unknown[];
	readonly channelGroups?: readonly unknown[];
	readonly channels?: readonly unknown[];
	/** Per channel id, newest-first — what ListMessages pages over. */
	readonly messagesByChannel?: Readonly<Record<string, readonly unknown[]>>;
}

/** Build the fake. The subscribe stream is an async generator fed by `emit`:
 *  it yields the boundary response first (so the driver takes the snapshot),
 *  then whatever the test pushes, and ends on `close` — or when the caller's
 *  AbortSignal fires, mirroring the real transport (a gRPC-Web call aborts its
 *  response stream). Honoring the signal is what makes the store's
 *  teardown-aborts-the-stream contract observable here. */
export function createFakeComms(snapshot: FakeCommsSnapshot = {}): FakeComms {
	const posts: RecordedPost[] = [];
	const askResponses: RecordedAskResponse[] = [];
	let postFailure: Error | undefined;
	let askFailure: Error | undefined;
	// A held-in-flight RespondToAsk: the promise the RPC parks on until the
	// test's gate settles it. One at a time — the store issues at most one
	// respond per ask, and a test gates exactly the one it is interleaving on.
	let askHold: Promise<void> | undefined;
	// The two server-side write invariants the double models: PostMessage's
	// idempotency key → its stored response, and the set of asks already
	// answered (an ask is answerable exactly once).
	const deduped = new Map<string, { message: WireMessage | undefined }>();
	const answered = new Set<string>();

	// The scripted tail: a queue of responses plus the promise the generator
	// parks on when it runs dry. `emit` enqueues and hands back a promise that
	// settles once the driver has pulled the response through, so a test can
	// await a push and then assert on the store synchronously.
	const queue: Array<{ resp: unknown; delivered: () => void }> = [];
	let wake: (() => void) | undefined;
	let closed = false;
	// Whether a subscribe generator is currently live. False before the first
	// subscribe and again once the stream ends — the observable the teardown
	// test reads (a store whose owner was disposed must leave no live stream).
	let streaming = false;

	async function* subscribe(signal?: AbortSignal): AsyncGenerator<unknown> {
		streaming = true;
		try {
			// The first response is the boundary: it carries the opaque snapshotSeq
			// the driver reads the snapshot at and no entity payload.
			yield {
				seq: 0n,
				atUnixMs: 0n,
				instanceEpoch: 1n,
				snapshotSeq: 1n,
				payload: { case: undefined },
			};
			while (!closed && !signal?.aborted) {
				const next = queue.shift();
				if (!next) {
					// Park until something arrives — or until the caller aborts, which
					// a real transport surfaces by ending the stream.
					await new Promise<void>((resolve) => {
						wake = resolve;
						signal?.addEventListener("abort", () => resolve(), {
							once: true,
						});
					});
					continue;
				}
				yield next.resp;
				next.delivered();
			}
		} finally {
			streaming = false;
		}
	}

	const client = {
		listAccounts: async () => ({ accounts: snapshot.accounts ?? [] }),
		listChannelGroups: async () => ({ groups: snapshot.channelGroups ?? [] }),
		listChannels: async () => ({ channels: snapshot.channels ?? [] }),
		listMessages: async (req: {
			container: { case: "channelId"; value: string };
			beforeMessageId: string;
		}) => {
			const all = snapshot.messagesByChannel?.[req.container.value] ?? [];
			// Paged backward by beforeMessageId (exclusive), newest-first — one
			// page is enough here, so a second call past the end returns empty and
			// terminates the driver's paging loop.
			if (req.beforeMessageId) return { messages: [] };
			return { messages: all };
		},
		subscribeComms: (
			_req: unknown,
			opts?: { signal?: AbortSignal },
		): AsyncGenerator<unknown> => subscribe(opts?.signal),
		postMessage: async (req: {
			container: { case: "channelId"; value: string };
			blocks: Array<{ block: { case?: string; value?: unknown } }>;
			parentMessageId: string;
			clientRequestId: string;
		}) => {
			if (postFailure) {
				const err = postFailure;
				postFailure = undefined;
				throw err;
			}
			// Idempotency, as the server implements it: a retry carrying an
			// already-seen clientRequestId returns the STORED message and records
			// nothing new (the `(author_account_id, client_request_id)` partial
			// unique index — go/internal/store/messages.go:79-84, migrations/
			// 0001_init.sql:133-138). Modelled so the contract is asserted by
			// consequence — one recorded post — rather than only by the keys being
			// distinct. An empty key is NOT deduped, matching the partial index.
			const stored = req.clientRequestId
				? deduped.get(req.clientRequestId)
				: undefined;
			if (stored) return stored;
			const first = req.blocks[0]?.block;
			posts.push({
				channelId: req.container.value,
				text: first?.case === "text" ? String(first.value) : "",
				parentMessageId: req.parentMessageId,
				clientRequestId: req.clientRequestId,
			});
			const response = { message: undefined as WireMessage | undefined };
			if (req.clientRequestId) deduped.set(req.clientRequestId, response);
			return response;
		},
		respondToAsk: async (req: {
			askId: string;
			answers: Array<{ questionId: string; chosenOptionIds: string[] }>;
		}) => {
			// A gated respond parks here — still IN FLIGHT from the store's point
			// of view — until the test settles it, so the test can push a stream
			// event through in between. Resolving with an error is how `reject`
			// refuses: the throw lands here, ahead of the answered-once
			// bookkeeping, exactly where `failNextAskResponse`'s does.
			if (askHold) {
				const held = askHold;
				askHold = undefined;
				await held;
			}
			// An ask is answered exactly ONCE. The server flips Ask.Answered on the
			// first AnswerAsk and rejects every later one with ErrConflict →
			// connect CodeAlreadyExists (go/internal/store/messages.go:400-406 and
			// :437; internal/comms/context.go:50-51) — a re-answer would silently
			// destroy the recorded audit value. A UI that fires one RespondToAsk
			// per click on a multi-question ask therefore gets its SECOND click
			// rejected by a real server; modelling it here is what makes that
			// visible to a test instead of passing against a permissive double.
			//
			// A plain Error, like failNextPost's: ConnectError is not re-exported
			// from @compass/client and the biome fence forbids importing
			// @connectrpc/connect here, so the double carries the status in the
			// message. The store surfaces `e.message` either way.
			if (askFailure) {
				const err = askFailure;
				askFailure = undefined;
				throw err;
			}
			if (answered.has(req.askId)) {
				throw new Error(
					`[already_exists] store: conflict: ask "${req.askId}" is already answered`,
				);
			}
			answered.add(req.askId);
			askResponses.push({
				askId: req.askId,
				answers: req.answers.map((a) => ({
					questionId: a.questionId,
					chosenOptionIds: [...a.chosenOptionIds],
				})),
			});
			return {};
		},
	};

	return {
		// The double implements only the driven subset, so a structural check
		// would (rightly) reject it; the unknown-cast is the one sanctioned seam,
		// mirroring stream.test.ts's fake.
		client: client as unknown as CommsClient,
		posts,
		askResponses,
		isStreaming: () => streaming,
		emit: (payload, seq) =>
			new Promise<void>((resolve) => {
				queue.push({
					resp: {
						seq,
						atUnixMs: 0n,
						instanceEpoch: 1n,
						snapshotSeq: 0n,
						payload,
					},
					delivered: resolve,
				});
				wake?.();
				wake = undefined;
			}),
		close: () => {
			closed = true;
			wake?.();
			wake = undefined;
		},
		failNextPost: (error) => {
			postFailure = error;
		},
		failNextAskResponse: (error) => {
			askFailure = error;
		},
		holdNextAskResponse: () => {
			let settle!: (error?: Error) => void;
			askHold = new Promise<void>((resolve, reject) => {
				settle = (error) => (error ? reject(error) : resolve());
			});
			// The store attaches its own catch the moment it issues the respond;
			// this one keeps a gate the test rejects BEFORE the RPC fires from
			// tripping an unhandled-rejection abort.
			askHold.catch(() => {});
			return {
				reject: (error: Error) => settle(error),
				accept: () => settle(),
			};
		},
	};
}

// ── Wire builders ────────────────────────────────────────────────────────────
// The generated-schema constructors every live-path suite feeds the fake with.
// Shared so the store suite, the thread suite, and the component suites all
// describe the SAME server; each takes only the fields whose value the tests
// assert on, defaulting the rest.

/** A user account on the wire. */
export function wireAccount(id: string): WireAccount {
	return create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});
}

/** A plain channel the caller is a member AND subscriber of — so the derived
 *  domain membership is "subscribed" and the composer renders enabled. */
export function wireChannel(id: string, callerId: string): WireChannel {
	return create(ChannelSchema, {
		id,
		name: id,
		kind: ChannelKind.CHANNEL,
		memberAccountIds: [callerId],
		subscriberAccountIds: [callerId],
	});
}

/** A single-text-block message. `parentMessageId` "" is the wire's unset
 *  convention for a thread root. */
export function wireTextMessage(opts: {
	id: string;
	channelId: string;
	authorAccountId: string;
	atUnixMs: number;
	text: string;
	parentMessageId?: string;
}): WireMessage {
	return create(MessageSchema, {
		id: opts.id,
		container: { case: "channelId", value: opts.channelId },
		authorAccountId: opts.authorAccountId,
		atUnixMs: BigInt(opts.atUnixMs),
		parentMessageId: opts.parentMessageId ?? "",
		blocks: [
			create(MessageBlockSchema, {
				block: { case: "text", value: opts.text },
			}),
		],
	});
}

/** A message carrying one ask of `questionIds.length` single-select questions,
 *  each with two options `<questionId>-a` / `<questionId>-b`. More than one
 *  question is what lets a test tell "sent every question" (the atomic wire
 *  contract) from "sent only the clicked one".
 *
 *  `chosen` seeds a question's already-recorded answer — what the server sends
 *  when ANOTHER participant answered the ask (the state a stream push carries),
 *  which a test needs to tell an authoritative server value apart from local
 *  state. */
export function wireAskMessage(opts: {
	id: string;
	channelId: string;
	authorAccountId: string;
	askId: string;
	questionIds: readonly string[];
	chosen?: Readonly<Record<string, readonly string[]>>;
}): WireMessage {
	return create(MessageSchema, {
		id: opts.id,
		container: { case: "channelId", value: opts.channelId },
		authorAccountId: opts.authorAccountId,
		atUnixMs: 1000n,
		blocks: [
			create(MessageBlockSchema, {
				block: {
					case: "ask",
					value: create(AskSchema, {
						askId: opts.askId,
						questions: opts.questionIds.map((questionId) =>
							create(AskQuestionSchema, {
								questionId,
								question: `${questionId}?`,
								allowMultiple: false,
								chosenOptionIds: [...(opts.chosen?.[questionId] ?? [])],
								options: [
									create(AskOptionSchema, {
										id: `${questionId}-a`,
										label: "A",
									}),
									create(AskOptionSchema, {
										id: `${questionId}-b`,
										label: "B",
									}),
								],
							}),
						),
					}),
				},
			}),
		],
	});
}
