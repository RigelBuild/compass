//go:build unix

package runner

// StartAgent's relay tail: the agent's newline-delimited protojson stdout is
// read, each frame relayed up PublishEvents Runner-sequenced (seq 1,2,3…), an
// undecodable line is skipped (not fatal), and stderr is drained. Driven over a
// pipe-backed StreamingExec (the test writes framed lines into IO.Stdout) and a
// REAL PublishEvents wire (a capturing handler observes the sequenced frames) —
// the relay's client-stream has no exported fake constructor, so it terminates a
// live server. Every assertion pins a contract a plausible bug would break:
// out-of-order or duplicated seq, a fatal decode error, or a stderr stall.

import (
	"context"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"

	"google.golang.org/protobuf/encoding/protojson"
)

// writeFrame marshals an AgentFrame to protojson and writes it as one newline-
// delimited line into the agent-stdout pipe.
func writeFrame(t *testing.T, w interface {
	Write(p []byte) (n int, err error)
}, frame *compassv1internal.AgentFrame) {
	t.Helper()
	b, err := protojson.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// sessionFrame builds an AgentFrame carrying a session trace event.
func sessionFrame(event string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{
				TypedEvent: &compassv1.SessionEvent{
					Event: &compassv1.SessionEvent_AssistantText{
						AssistantText: &compassv1.SessionAssistantText{Text: event},
					},
				},
			},
		},
	}
}

// Each relayed frame is published Runner-sequenced: seq 1, 2, 3 in order, all
// under the session id, and the frame body arrives verbatim. A bug in the
// atomic seq (reset, skip, or reuse) would break the monotonic 1,2,3.
func TestRelaySequencesFramesMonotonically(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, capture))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-1", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Feed three well-formed frames.
	writeFrame(t, engine.stdoutW, sessionFrame("first"))
	writeFrame(t, engine.stdoutW, sessionFrame("second"))
	writeFrame(t, engine.stdoutW, sessionFrame("third"))

	for i, want := range []struct {
		seq   uint64
		event string
	}{{1, "first"}, {2, "second"}, {3, "third"}} {
		f := capture.recvFrame(t)
		if f.GetRunnerSeq() != want.seq {
			t.Fatalf("frame %d seq = %d, want %d (Runner-sequenced, monotonic)", i, f.GetRunnerSeq(), want.seq)
		}
		if f.GetSessionId() != "sess-1" {
			t.Fatalf("frame %d session id = %q, want sess-1", i, f.GetSessionId())
		}
		if got := f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText(); got != want.event {
			t.Fatalf("frame %d event = %q, want %q (frame body relayed verbatim)", i, got, want.event)
		}
	}

	// Close stdout so the relay ends its scanner and closes the stream cleanly.
	engine.closeStdout()
}

// An undecodable line is skipped, not fatal: a garbage line between two good
// frames must not stop the relay, and the good frames keep their monotonic seq
// (the skipped line consumes NO sequence number). A bug that treated a decode
// error as fatal would drop everything after it.
func TestRelaySkipsUndecodableLineNonFatal(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, capture))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-1", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	writeFrame(t, engine.stdoutW, sessionFrame("good-1"))
	if _, err := engine.stdoutW.Write([]byte("this is not protojson\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	writeFrame(t, engine.stdoutW, sessionFrame("good-2"))

	// The two good frames arrive as seq 1 and 2 — the garbage line consumed no
	// sequence number (it was skipped before Send).
	f1 := capture.recvFrame(t)
	if f1.GetRunnerSeq() != 1 || f1.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText() != "good-1" {
		t.Fatalf("first frame = seq %d %q, want seq 1 good-1", f1.GetRunnerSeq(), f1.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText())
	}
	f2 := capture.recvFrame(t)
	if f2.GetRunnerSeq() != 2 {
		t.Fatalf("second good frame seq = %d, want 2 (the skipped line consumes no seq)", f2.GetRunnerSeq())
	}
	if f2.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText() != "good-2" {
		t.Fatalf("second good frame event = %q, want good-2 (relay survived the garbage line)", f2.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText())
	}

	engine.closeStdout()
}

// A blank line is skipped (len 0), consuming no sequence number — the relay
// tolerates empty lines in the agent's stdout stream.
func TestRelaySkipsBlankLines(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, capture))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-1", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	if _, err := engine.stdoutW.Write([]byte("\n")); err != nil {
		t.Fatalf("write blank line: %v", err)
	}
	writeFrame(t, engine.stdoutW, sessionFrame("after-blank"))

	f := capture.recvFrame(t)
	if f.GetRunnerSeq() != 1 || f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText() != "after-blank" {
		t.Fatalf("frame after blank = seq %d %q, want seq 1 after-blank", f.GetRunnerSeq(), f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText())
	}
	engine.closeStdout()
}

// A conversation frame relays verbatim too — the relay does not classify, it
// forwards; classification is the Server's Deliver seam. This proves the relay
// carries the conversation variant through, not only session frames.
func TestRelayForwardsConversationFrame(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, capture))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-conv", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	frame := &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationPosted{
			ConversationPosted: &compassv1.MessagePosted{
				Message: &compassv1.Message{
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
				},
			},
		},
	}
	writeFrame(t, engine.stdoutW, frame)

	f := capture.recvFrame(t)
	if f.GetFrame().GetConversationPosted() == nil {
		t.Fatalf("relayed frame variant = %T, want ConversationPosted (relay forwards, does not reclassify)", f.GetFrame().GetFrame())
	}
	engine.closeStdout()
}

// stderr is drained continuously so a chatty agent cannot fill the OS pipe
// buffer and stall the stdout frame stream. This writes far more stderr than a
// pipe buffer holds while stdout frames keep flowing; if stderr were not
// drained, the stdout relay would stall and the frame would never arrive. The
// arriving frame is the proof the drain ran.
func TestRelayDrainsStderrSoStdoutDoesNotStall(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, capture))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-1", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Flood stderr from a goroutine (its writes block until drained), and send
	// one stdout frame. The frame arriving proves stdout was not stalled behind
	// an undrained stderr pipe.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		big := make([]byte, 256*1024) // far over a default pipe buffer
		for i := range big {
			big[i] = 'x'
		}
		big[len(big)-1] = '\n'
		_, _ = engine.stderrW.Write(big)
		_ = engine.stderrW.Close()
	}()

	writeFrame(t, engine.stdoutW, sessionFrame("through"))
	f := capture.recvFrame(t)
	if f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText() != "through" {
		t.Fatalf("frame event = %q, want through (stdout must flow while stderr floods)", f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText())
	}

	<-stderrDone
	engine.closeStdout()
}
