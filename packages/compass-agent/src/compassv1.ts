// The single import point for every compass.v1 type + schema this package uses. A barrel:
// where the generated types live is a build-seam decision owned by the compass service.
// Frozen §T5: generated into ./gen via a second `out:` on buf.gen.yaml; if it fans out
// differently, only this file's imports change. The agent emits protojson (no @connectrpc).

// Codec: protobuf-es v2 runtime (the gen files import from the same package).
export {
	create,
	fromJson,
	type JsonValue,
	type MessageInitShape,
	toJson,
} from "@bufbuild/protobuf";
export {
	// The agent-initiated board call envelopes (internal-only AgentGateway gen). Request
	// carries the toolCallId as `call_id` plus a oneof; Result mirrors it with an in-band
	// `error` case (not a transport teardown). Reused verbatim as the RelayBoardCall payloads.
	type BoardCallError,
	BoardCallErrorSchema,
	type BoardCallRequest,
	BoardCallRequestSchema,
	type BoardCallResult,
	BoardCallResultSchema,
	// The agent-initiated forge call envelopes (internal-only AgentGateway gen). Request
	// carries `call_id`, a oneof over the twelve forge arms, an optional `ForgeRef`, and a
	// `client_request_id` (create arms); Result retypes to canonical Issue/PullRequest/etc
	// plus an in-band `error` arm carrying `retry_after_ms`. Reused as RelayForgeCall payloads.
	CommentOnIssueRequestSchema,
	CommentOnPullRequestRequestSchema,
	// The agent-initiated comms call envelopes (internal-only AgentGateway gen). Request
	// carries `call_id` plus a oneof; Result mirrors it with an in-band `error` case (not a
	// transport teardown). Reused verbatim as the RelayCommsCall payloads.
	type CommsCallError,
	CommsCallErrorSchema,
	type CommsCallRequest,
	CommsCallRequestSchema,
	type CommsCallResult,
	CommsCallResultSchema,
	CreateIssueRequestSchema,
	CreatePullRequestRequestSchema,
	// The agent-initiated lifecycle call envelopes (internal-only AgentGateway gen). Request
	// carries `call_id` plus a oneof over spawn/despawn; Result mirrors it with an in-band
	// `error` case (not a transport teardown). Reused verbatim as RelayLifecycleCall payloads.
	type DespawnPeerRequest,
	DespawnPeerRequestSchema,
	type DespawnPeerResponse,
	DespawnPeerResponseSchema,
	type ForgeCallError,
	ForgeCallErrorSchema,
	type ForgeCallRequest,
	ForgeCallRequestSchema,
	type ForgeCallResult,
	ForgeCallResultSchema,
	GetIssueRequestSchema,
	GetPullRequestRequestSchema,
	type LifecycleCallError,
	LifecycleCallErrorSchema,
	type LifecycleCallRequest,
	LifecycleCallRequestSchema,
	type LifecycleCallResult,
	LifecycleCallResultSchema,
	type ListIssuesRequest,
	ListIssuesRequestSchema,
	type ListIssuesResponse,
	ListIssuesResponseSchema,
	type ReviewCommentInput,
	ReviewCommentInputSchema,
	// The agent's activity-status upsert (internal-only AgentGateway gen): the
	// `SetAgentStatusRequest` carries the human-readable activity string; the
	// empty `SetAgentStatusResponse` is the non-error ack (a durable
	// `agent_activity` upsert then a best-effort presence publish, both T2).
	type SetAgentStatusRequest,
	SetAgentStatusRequestSchema,
	type SetAgentStatusResponse,
	SetAgentStatusResponseSchema,
	// The one board-call arm today (internal-only AgentGateway gen): the
	// `SetIssueStateRequest` names a Compass-local issue id + target `IssueState`;
	// the `SetIssueStateResponse` carries the post-transition `Issue` truth.
	type SetIssueStateRequest,
	SetIssueStateRequestSchema,
	type SetIssueStateResponse,
	SetIssueStateResponseSchema,
	type SpawnPeerRequest,
	SpawnPeerRequestSchema,
	type SpawnPeerResponse,
	SpawnPeerResponseSchema,
	SubmitReviewRequestSchema,
	SubscribeForgeRequestSchema,
	type SubscribeForgeResponse,
	SubscribeForgeResponseSchema,
	// The two forge state-transition arms (RIG-3331): both requests are
	// all-scalar and reuse `ForgeCallResult.issue` / `.pull_request`, so no new
	// forge domain type rides the wire.
	TransitionIssueStateRequestSchema,
	TransitionPullRequestStateRequestSchema,
	UnsubscribeForgeRequestSchema,
	type UnsubscribeForgeResponse,
	UnsubscribeForgeResponseSchema,
} from "./gen/compass/v1/agent_gateway_pb";
export {
	// The inbound control envelope (internal-only §T5): a oneof over the control ops plus a
	// Runner-assigned `controlSeq` (retention cursor). The source decodes one AgentControl per
	// message; payload fields for steer/deliver/replay/config stay empty shells (RIG-1310 parked).
	type AgentControl,
	AgentControlSchema,
	// The stdout envelope (internal-only §T5): a oneof over the payload messages.
	// The sink builds it and protojson-serializes it; the reader classifies each
	// line by the set oneof field.
	type AgentFrame,
	AgentFrameSchema,
	type ConfigControl,
	ConfigControlSchema,
	// Agent -> Runner control-plane ack frames (AgentFrame oneof variants),
	// riding the Publish spine: replay-barrier ack + selective apply-ack.
	type ControlAck,
	ControlAckSchema,
	type DeliverControl,
	DeliverControlSchema,
	// The agent's per-message delivery receipt (RIG-1569), an AgentFrame oneof
	// variant riding the Publish spine — correlates to the delivered message by
	// id. Emitted by the CompassAgent at injection time (RIG-1310 §8 deliver arm).
	type DeliveryAck,
	DeliveryAckSchema,
	// The agent's per-notification forge delivery receipt (RIG-2732 W3), an
	// AgentFrame oneof variant riding the Publish spine beside DeliveryAck.
	// Correlates a forge notification by subscription_id and carries the notified
	// revision. Emitted by the CompassAgent at turn-end flush (T6 forge arm).
	type ForgeNotificationAck,
	ForgeNotificationAckSchema,
	type PromptControl,
	PromptControlSchema,
	type ReplayComplete,
	type ReplayCompleteAck,
	ReplayCompleteAckSchema,
	ReplayCompleteSchema,
	// The `session` variant's payload: a typed OMP-native trace event
	// (typed_event) plus the board lifecycle state (state). Retyped from the
	// former opaque `bytes event` under design: architecture-lineage
	// — the trace is a typed SessionEvent now, never opaque bytes.
	type SessionFrame,
	SessionFrameSchema,
	type SteerControl,
	SteerControlSchema,
	// The `transcript_entry` variant's payload: one committed SDK session entry (entry_json +
	// checkpoint + entry_seq) the tee backend commits locally and tees upstream (RIG-1570).
	// Constructed with `create(...)` so the branded message satisfies the AgentFrame oneof.
	type TranscriptEntry,
	TranscriptEntrySchema,
	type TranscriptReplay,
	TranscriptReplaySchema,
} from "./gen/compass/v1/agent_pb";
export {
	// The presence enum a RosterEntry carries (field 5) — session-derived
	// server-side, rendered as a fixed label so it needs no render guard.
	AgentPresence,
	type Ask,
	// The answered-ask snapshot a delivered `ask_answer` message carries on the
	// deliver lane (RIG-2257): the answered `Ask` plus the denormalized asker
	// account id. The agent renders it via `formatAskAnswerForPrompt`.
	type AskAnswerBlock,
	AskAnswerBlockSchema,
	type AskOption,
	AskOptionSchema,
	type AskQuestion,
	// The answer to one AskQuestion (question_id + chosen option ids + free
	// text). A repeated AskQuestionAnswer keys one answer per question — the
	// same shape RespondToAskRequest uses.
	type AskQuestionAnswer,
	AskQuestionAnswerSchema,
	AskQuestionSchema,
	AskSchema,
	// The full channel message an `OpenDMResponse` wraps — the DM tools render
	// its `name` (dm--<lo>--<hi>) and tests build fixtures from it.
	type Channel,
	ChannelSchema,
	// The roster read payloads the agent's `compass_roster` tool constructs: the
	// request names a `scope` (RosterScope) and, for an agent caller, omits the
	// session-resolved `agentAccountId`; the response carries the RosterEntry
	// rows.
	type GetRosterRequest,
	GetRosterRequestSchema,
	type GetRosterResponse,
	GetRosterResponseSchema,
	// The comms call payloads the agent tools construct: the post/list request
	// pair (each with a `container` oneof whose unset case means "the agent's
	// home channel", resolved server-side) and their responses.
	type ListMessagesRequest,
	ListMessagesRequestSchema,
	type ListMessagesResponse,
	ListMessagesResponseSchema,
	// Conversation payloads (comms surface). The AgentFrame reuses MessagePosted/MessageUpdated
	// (each wraps a Message carrying MessageBlocks) as its conversation variants. The MessageBlock
	// oneof carries the durable variants (text + ask); trace variants ride the typed SessionEvent.
	type Message,
	type MessageBlock,
	MessageBlockSchema,
	type MessagePosted,
	MessagePostedSchema,
	MessageSchema,
	type MessageUpdated,
	MessageUpdatedSchema,
	// The peer-DM open call payload pair (peer-DM record): OpenDMRequest names a
	// peer by handle; OpenDMResponse carries the resolved DM `Channel` (above)
	// plus a `created` flag distinguishing a mint from a resume.
	type OpenDMRequest,
	OpenDMRequestSchema,
	type OpenDMResponse,
	OpenDMResponseSchema,
	type PostMessageRequest,
	PostMessageRequestSchema,
	type PostMessageResponse,
	PostMessageResponseSchema,
	// One roster row (id, tree position, presence, activity) plus the scope enum
	// mapping the tool's string param onto the request.
	type RosterEntry,
	RosterEntrySchema,
	RosterScope,
} from "./gen/compass/v1/comms_pb";
export {
	// Forge canonical result types (DL-069/DL-092: the forge domain arms retype to these) plus
	// the multi-forge selector. Issue/PullRequest are read + create-ack payloads; Review/etc are
	// nested read-render sub-messages; ForgeRef/ForgeProvider are the selector every forge tool spreads.
	type AgentAttribution,
	// The plan entry the typed session plan reuses (content + status) and its
	// status enum — reused rather than minting parallel enums
	// (compass.proto:272-277, 297-300).
	type AgentPlanEntry,
	AgentPlanEntrySchema,
	AgentPlanEntryStatus,
	// The board lifecycle state carried by SessionFrame.state.
	AgentSessionState,
	AgentToolCallStatus,
	type ChecksSummary,
	ChecksSummarySchema,
	type Comment,
	ForgeProvider,
	type ForgeRef,
	ForgeRefSchema,
	type Issue,
	IssueSchema,
	// The canonical issue lifecycle state (Issue.state, SetIssueStateRequest.state)
	// — the eight-value board enum the board tool targets and renders.
	IssueState,
	IssueStateSchema,
	type PullRequest,
	PullRequestSchema,
	type Review,
	type ReviewThread,
	// The typed observation-trace event carried by SessionFrame.typed_event: a oneof over
	// assistant-text / thinking chunks, a tool call + updates (with diffs), a plan, or a notice.
	// The emitter builds one per trace event; the Session* sub-messages are the oneof payloads.
	type SessionAssistantText,
	SessionAssistantTextSchema,
	type SessionError,
	SessionErrorKind,
	SessionErrorSchema,
	type SessionEvent,
	SessionEventSchema,
	type SessionFileDiff,
	SessionFileDiffSchema,
	type SessionInjection,
	SessionInjectionKind,
	SessionInjectionSchema,
	type SessionNotice,
	SessionNoticeSchema,
	type SessionPlan,
	SessionPlanSchema,
	type SessionThinking,
	SessionThinkingSchema,
	type SessionToolCall,
	SessionToolCallSchema,
	type SessionToolCallUpdate,
	SessionToolCallUpdateSchema,
} from "./gen/compass/v1/compass_pb";
export {
	// The forge write-ack references (internal-only forge gen). `CommentRef` is the ack for both
	// comment arms; `ReviewRef` is the submit_review ack (url + review_id + verdict);
	// `ForgeArtifactKind` is the subscribe arm's kind selector.
	type CommentRef,
	CommentRefSchema,
	ForgeArtifactKind,
	// The forge change pushed to the agent (RIG-2732 DL-053/DL-054): the
	// per-artifact notification the control-source decodes off
	// AgentControl.forge_notification and the agent renders + acks at turn-end
	// flush. `ForgeNotificationKind` discriminates the per-kind render.
	type ForgeNotification,
	ForgeNotificationKind,
	ForgeNotificationSchema,
	type ReviewRef,
	ReviewRefSchema,
} from "./gen/compass/v1/forge_pb";
