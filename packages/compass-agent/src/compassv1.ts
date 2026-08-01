// The single import point for every compass.v1 type + schema this package uses.
//
// WHY a barrel: the agent needs the generated compass.v1 message types to
// construct the payloads it emits, but where those types live is a build-seam
// decision owned by the compass service (buf codegen pipeline + drift gate).
// Frozen §T5 (design compass-0.6): the agent's compass.v1 types are generated
// into ./gen via a second `out:` on buf.gen.yaml — its own drift-gated tree
// (option A). Today ./gen holds a byte-identical second buf output of the one
// owned schema; if that ever fans out differently, only this file's import
// paths change. Every other module imports compass.v1 through here.
//
// The agent emits protojson (newline-framed) — it never reaches the daemon over
// gRPC, so it imports the message *types* + the @bufbuild/protobuf codec, not
// the @connectrpc transport the biome fence restricts. No fence override needed.

// Codec: protobuf-es v2 runtime (the gen files import from the same package).
export { create, fromJson, type JsonValue, toJson } from "@bufbuild/protobuf";
export {
	// The agent-initiated comms call envelopes (internal-only AgentGateway gen).
	// One `CommsCallRequest` carries the SDK toolCallId as `call_id` plus a oneof
	// over the comms operations; the `CommsCallResult` mirrors it with a third
	// `error` case — an in-band domain failure, NOT a transport teardown. The same
	// two messages are reused verbatim as the RelayCommsCall payloads on the
	// Runner->Server leg, so this is the one wire shape for both hops.
	type CommsCallError,
	CommsCallErrorSchema,
	type CommsCallRequest,
	CommsCallRequestSchema,
	type CommsCallResult,
	CommsCallResultSchema,
} from "./gen/compass/v1/agent_gateway_pb";
export {
	// The inbound control envelope (internal-only §T5): a oneof over the control
	// ops plus a Runner-assigned `controlSeq` envelope field (retention cursor).
	// The control source decodes one AgentControl per Control-stream message;
	// the agent classifies each by the set oneof case. Payload fields for
	// steer/deliver/replay/config stay empty shells (SEA-1310 parked).
	type AgentControl,
	AgentControlSchema,
	// The stdout envelope (internal-only §T5): a oneof over the payload messages.
	// The sink builds it and protojson-serializes it; the reader classifies each
	// line by the set oneof field.
	type AgentFrame,
	AgentFrameSchema,
	type AskAnswerControl,
	AskAnswerControlSchema,
	type ConfigControl,
	ConfigControlSchema,
	// Agent -> Runner control-plane ack frames (AgentFrame oneof variants),
	// riding the Publish spine: replay-barrier ack + selective apply-ack.
	type ControlAck,
	ControlAckSchema,
	type DeliverControl,
	DeliverControlSchema,
	type PromptControl,
	PromptControlSchema,
	type ReplayComplete,
	type ReplayCompleteAck,
	ReplayCompleteAckSchema,
	ReplayCompleteSchema,
	// The `session` variant's payload: a typed OMP-native trace event
	// (typed_event) plus the board lifecycle state (state). Retyped from the
	// former opaque `bytes event` under compass-0.8 §"First-party typed session
	// renderer" — the trace is a typed SessionEvent now, never opaque bytes.
	type SessionFrame,
	SessionFrameSchema,
	type SteerControl,
	SteerControlSchema,
	// The `transcript_entry` variant's payload: one committed SDK session entry
	// (entry_json + checkpoint + entry_seq) the tee backend commits locally and
	// tees upstream as a durable frame (SEA-1570). Constructed with
	// `create(TranscriptEntrySchema, …)` so the branded message satisfies the
	// AgentFrame oneof.
	type TranscriptEntry,
	TranscriptEntrySchema,
	type TranscriptReplay,
	TranscriptReplaySchema,
} from "./gen/compass/v1/agent_pb";
export {
	type Ask,
	type AskOption,
	AskOptionSchema,
	// The answer to one AskQuestion (question_id + chosen option ids + free
	// text). AskAnswerControl carries a repeated AskQuestionAnswer so a
	// multi-question ask survives the control wire keyed per question — the
	// same shape RespondToAskRequest uses.
	type AskQuestionAnswer,
	AskQuestionAnswerSchema,
	AskQuestionSchema,
	AskSchema,
	// The comms call payloads the agent tools construct: the post/list request
	// pair (each with a `container` oneof whose unset case means "the agent's
	// home channel", resolved server-side) and their responses.
	type ListMessagesRequest,
	ListMessagesRequestSchema,
	type ListMessagesResponse,
	ListMessagesResponseSchema,
	// Conversation payloads (comms surface). The AgentFrame reuses
	// MessagePosted/MessageUpdated (each wraps a Message carrying MessageBlocks)
	// as its conversation variants — no bare-block variant. The MessageBlock
	// oneof carries the surviving durable-conversation variants (text + ask); the
	// trace variants (thought/tool_call/plan/diff) ride the typed SessionEvent on
	// the session surface, not comms blocks (design compass-0.6, spine-inversion).
	type Message,
	type MessageBlock,
	MessageBlockSchema,
	type MessagePosted,
	MessagePostedSchema,
	MessageSchema,
	type MessageUpdated,
	MessageUpdatedSchema,
	type PostMessageRequest,
	PostMessageRequestSchema,
	type PostMessageResponse,
	PostMessageResponseSchema,
} from "./gen/compass/v1/comms_pb";
export {
	// The plan entry the typed session plan reuses (content + status) and its
	// status enum — reused rather than minting parallel enums
	// (compass.proto:272-277, 297-300).
	type AgentPlanEntry,
	AgentPlanEntrySchema,
	AgentPlanEntryStatus,
	// The board lifecycle state carried by SessionFrame.state.
	AgentSessionState,
	AgentToolCallStatus,
	// The typed observation-trace event (compass-0.8 §"First-party typed session
	// renderer") carried by SessionFrame.typed_event: a oneof over assistant-text
	// / thinking chunks, a tool call + its updates (with file diffs), a plan, or a
	// notice. The emitter builds one per trace event it maps; the Session* sub-
	// message schemas are the oneof payloads it constructs with `create`.
	type SessionAssistantText,
	SessionAssistantTextSchema,
	type SessionEvent,
	SessionEventSchema,
	type SessionFileDiff,
	SessionFileDiffSchema,
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
