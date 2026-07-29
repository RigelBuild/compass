//go:build unix

package runner

// StartAgent's exec tail after the C5 cutover: the agent's compass.v1 traffic
// rides the per-container socket, so both pipes are pure diagnostics and are
// drained to the log. Driven over a pipe-backed StreamingExec (the test writes
// into IO.Stdout / IO.Stderr) with a capturing slog handler as the observable.
// The relay-era tests (Runner-sequenced PublishEvents frames off stdout) are
// retired with the relay itself; what survives is the property that outlived the
// protocol — an undrained pipe stalls the agent, whatever the bytes mean.

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

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

// An undrained pipe fills its OS buffer and blocks the agent's next write, so
// the property is asserted directly: flood stderr past any plausible pipe
// buffer and require the write to COMPLETE. Without the stderr drain it blocks
// forever and the test fails on the deadline. The completed write is the
// observable — with no protocol on the pipes, nothing else proves consumption.
func TestStderrFloodDoesNotStallTheAgent(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-flood", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Far over a default 64KiB pipe buffer: without a reader this write blocks.
	big := make([]byte, 256*1024)
	for i := range big {
		big[i] = 'x'
	}
	big[len(big)-1] = '\n'

	wrote := make(chan error, 1)
	go func() {
		_, err := engine.stderrW.Write(big)
		wrote <- err
	}()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("stderr write = %v, want nil (the drain must consume it)", err)
		}
	case <-timeAfter():
		t.Fatal("stderr write never completed — the agent is stalled behind an undrained pipe")
	}

	// Deferred Close on an in-memory test pipe: no actionable error (errcheck is
	// relaxed for test cleanup).
	_ = engine.stderrW.Close()
	engine.closeStdout()
}

// --- stdout cutover (C5) ------------------------------------------------------

// After the cutover, stdout carries NO protocol traffic: a well-formed
// protojson AgentFrame written to the agent's raw stdout must NOT surface as a
// PublishEvents frame.
//
// Two independent observables, because neither alone is sound. A published
// frame crosses a real h2c wire into the handler goroutine, while the drain's
// log line is an in-process send — nothing orders those against each other, so
// gating on the log line and then sampling the frame channel could miss a frame
// still in flight. Instead: a sentinel line written after the frame proves the
// drain consumed both, and `streamOpens` proves no publisher ever opened the
// PublishEvents stream — which a live relay does before it sends anything, so
// that count trips even on a frame that never arrives.
func TestStdoutFrameIsNotPublishedAfterCutover(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-cutover", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// A frame that WOULD relay: well-formed protojson the old scanner decoded
	// and published. Anything less would pass for the wrong reason.
	writeFrame(t, engine.stdoutW, sessionFrame("must-not-relay"))
	if _, err := engine.stdoutW.Write([]byte(cutoverSentinel + "\n")); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Read until the sentinel: the drain has now consumed the frame line too.
	for {
		line := logs.recvLine(t)
		if line.msg != "agent stdout" {
			t.Fatalf("log msg = %q, want %q (stdout must be drained to the diagnostic log)", line.msg, "agent stdout")
		}
		if line.attrs["line"] == cutoverSentinel {
			break
		}
	}

	// The contract: no protocol traffic on stdout.
	if n := capture.streamOpens(); n != 0 {
		t.Fatalf("PublishEvents streams opened = %d, want 0 — something still publishes agent stdout", n)
	}
	select {
	case f := <-capture.frames:
		t.Fatalf("stdout byte surfaced as a published frame (%v) — the relay is not retired", f.GetFrame())
	default:
	}

	engine.closeStdout()
}

// cutoverSentinel is a plain line written after the frame; observing it in the
// log proves the drain consumed everything before it.
const cutoverSentinel = "cutover-sentinel"

// The other half of the cutover: the bytes are not silently dropped. A non-frame
// line on the agent's raw stdout lands in the diagnostic log with its session id
// — the drain is really there. A bug that closed stdout instead of draining it
// (or dropped the session id) would break this while the test above still passed.
func TestStdoutIsDrainedToDiagnosticLog(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-drain", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	if _, err := engine.stdoutW.Write([]byte("plain diagnostic chatter\n")); err != nil {
		t.Fatalf("write stdout: %v", err)
	}

	line := logs.recvLine(t)
	if line.msg != "agent stdout" {
		t.Fatalf("log msg = %q, want %q", line.msg, "agent stdout")
	}
	if got := line.attrs["line"]; got != "plain diagnostic chatter" {
		t.Fatalf("drained line = %q, want %q", got, "plain diagnostic chatter")
	}
	if got := line.attrs["session_id"]; got != "sess-drain" {
		t.Fatalf("session_id = %q, want %q (the drain must stamp the session)", got, "sess-drain")
	}

	engine.closeStdout()
}

// The same for stderr, under its own label. Both pipes share one drain, so the
// label is the only thing distinguishing the agent's error output from its
// chatter in the diagnostic log — an operator reading the log has nothing else
// to go on. A swapped or empty msg is invisible to every other test here.
func TestStderrIsDrainedUnderItsOwnLabel(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-err", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	if _, err := engine.stderrW.Write([]byte("panic: something broke\n")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}

	line := logs.recvLine(t)
	if line.msg != "agent stderr" {
		t.Fatalf("log msg = %q, want %q (stderr must not be labelled as stdout)", line.msg, "agent stderr")
	}
	if got := line.attrs["line"]; got != "panic: something broke" {
		t.Fatalf("drained line = %q, want %q", got, "panic: something broke")
	}
	if got := line.attrs["session_id"]; got != "sess-err" {
		t.Fatalf("session_id = %q, want %q", got, "sess-err")
	}

	// Deferred Close on an in-memory test pipe: no actionable error (errcheck is
	// relaxed for test cleanup).
	_ = engine.stderrW.Close()
	engine.closeStdout()
}

// A bare trailing `\r` with NO newline is payload, not a terminator, and must
// survive the drain. The bug this pins is a disagreement between two
// computations of the same thing inside `readBoundedLine`: the truncation
// arithmetic discounts a `\r` only when a `\n` followed it (`tail[1] == '\n'`),
// while `trimEOL` stripped one unconditionally. So an unterminated final line —
// exactly what an agent that dies mid-write leaves behind, and what a `\r`
// progress-bar write produces — silently lost its last byte in the log.
//
// EOF is the discriminator: only an unterminated line reaches `trimEOL` with a
// `\r` that no `\n` follows, so the write must close the pipe rather than end
// in a newline. A test writing "x\r\n" cannot fail either way.
func TestBareTrailingCarriageReturnIsPayload(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-cr", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// No trailing newline: the `\r` is the final byte before EOF, so nothing
	// terminated this line and the CR is ordinary payload.
	if _, err := engine.stdoutW.Write([]byte("progress 50%\r")); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	engine.closeStdout()

	line := logs.recvLine(t)
	if got := line.attrs["line"]; got != "progress 50%\r" {
		t.Fatalf("drained line = %q, want %q — a bare CR with no newline after it is payload, not a terminator", got, "progress 50%\r")
	}
}

// A line too long to log must NOT end the drain. Draining is the contract — an
// unread pipe stalls the agent's next write — so an oversized line costs a
// truncated entry and nothing else. A bufio.Scanner ends its scan on
// ErrTooLong, which would leave the pipe unread forever with no diagnostic
// anywhere; the following line arriving is the proof that did not happen.
func TestOverlongLineTruncatesButKeepsDraining(t *testing.T) {
	engine := newPipeRuntime()
	capture := newCapturePublish()
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, capture))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := link.StartAgent(ctx, "sess-long", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger()); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Twice the cap, so the line is split across many reads.
	overlong := make([]byte, 2*maxLoggedLine)
	for i := range overlong {
		overlong[i] = 'x'
	}
	overlong[len(overlong)-1] = '\n'

	go func() {
		_, _ = engine.stdoutW.Write(overlong)
		_, _ = engine.stdoutW.Write([]byte("after-overlong\n"))
	}()

	first := logs.recvLine(t)
	if got := len(first.attrs["line"]); got != maxLoggedLine {
		t.Fatalf("logged prefix = %d bytes, want the %d-byte cap", got, maxLoggedLine)
	}
	if first.attrs["truncated"] != "true" {
		t.Fatalf("truncated flag = %q, want %q — a clipped line must say so", first.attrs["truncated"], "true")
	}

	// The contract: the drain survived and the pipe is still being read.
	next := logs.recvLine(t)
	if got := next.attrs["line"]; got != "after-overlong" {
		t.Fatalf("next drained line = %q, want %q — the drain died on the overlong line", got, "after-overlong")
	}

	engine.closeStdout()
}

// Stop must reap BEFORE it joins the drains. The drains block in a read on
// pipes only the child's death closes, so joining first waits on goroutines
// that cannot finish yet and the teardown pays the full drainGrace. The bug is
// silent — it costs only time, and every assertion still passes — which is
// exactly the kind that survives review.
//
// Reproducing it needs the specific condition, arrived at by measurement after
// two wrong guesses: neither a bare host Stop (~1.7ms) nor one after a plain
// dial (~230µs) is slow, because nothing holds the pipes open. It takes a call
// genuinely IN-FLIGHT at the Server — its handler goroutine parked on the
// socket — which is what pins the drains and makes a premature join wait out
// the whole grace. `release` is the wedge; the fake's `started` channel is the
// event-gate proving the forward really is in-flight before Stop runs.
func TestStopReapsBeforeJoiningTheDrains(t *testing.T) {
	fake := &recordingRelay{started: make(chan struct{}), release: make(chan struct{})}
	defer close(fake.release) // free the wedged handler however the test exits
	h := newTransportFixture(t, fake)
	// context.Background() as the test root — the rule's explicit test exemption.
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "acct-1"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name})
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	client := dialAgent(t, listenerPath(t, h, name))
	go func() {
		_, _ = client.Comms(context.Background(), connect.NewRequest(&compassv1internal.CommsCallRequest{CallId: "tc-stop"}))
	}()

	// Event-gated, not slept: proceed only once the forward is really in-flight.
	select {
	case <-fake.started:
	case <-timeAfter():
		t.Fatal("relay forward never entered in-flight; the drains would not be pinned")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.Stop(ctx, sessionID) }()

	// Generous against a slow reap, far under drainGrace: the failure caught is
	// a full-grace stall, not a few hundred milliseconds of jitter.
	const reapBudget = drainGrace / 2
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop = %v, want nil", err)
		}
	case <-time.After(reapBudget):
		t.Fatalf("Stop did not return within %v (drainGrace is %v) — it is joining the drains before Terminate, so teardown pays the full grace", reapBudget, drainGrace)
	}
}

// A clean Stop must be quiet. The reap ends both drains one of two ways, and
// which one is a race: SIGKILL closes the child's write end (EOF), while
// cmd.Wait closes our read end (os.ErrClosed). Measured against this stub EOF
// wins every time, so this test alone does NOT reach the os.ErrClosed arm —
// TestDrainToLogIsSilentOnDeliberateStop drives that arm directly, and
// TestStopArmsTheStoppingDiscriminator pins the wiring between them. What this
// test covers is the whole-path guarantee: however the race lands, a clean Stop
// emits no WARN. Treating either end as a fault fires `drain ended early` on
// every stop (including the first half of every Reload) and trains operators to
// ignore the single WARN that means a live agent is about to stall behind an
// unread pipe.
//
// Needs a capturing logger: the other teardown tests use discardLoggerRunner,
// so the spurious records were thrown away and invisible to the whole suite.
func TestCleanStopEmitsNoDrainWarning(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stream, err := link.StartAgent(ctx, "sess-quiet", runtime.ContainerID("c1"), engine, testAgentEnv(), logs.logger())
	if err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Stop joins both drains before returning, so every record they will ever
	// emit is already in the channel — draining it here races nothing.
	if err := stream.Stop(); err != nil {
		t.Fatalf("Stop = %v", err)
	}

	for {
		select {
		case l := <-logs.lines:
			if strings.Contains(l.msg, "drain ended early") {
				t.Fatalf("clean Stop logged %q (error %q) — the reap's pipe close is an expected end, not a fault",
					l.msg, l.attrs["error"])
			}
		default:
			return
		}
	}
}

// The wiring between the two halves above, and the one thing neither reaches:
// Stop must ARM the discriminator, and arm it before the reap. The drains hold
// no other way to tell the reap's pipe close from a live agent's, so an unarmed
// flag silently reclassifies every stop as a fault — and it cannot be caught
// through Stop's own path, because whether the reap delivers os.ErrClosed at all
// is a race this stub loses to EOF. Asserting the flag directly is what makes
// the deliberate-stop arm reachable in production rather than only in a test.
func TestStopArmsTheStoppingDiscriminator(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stream, err := link.StartAgent(ctx, "sess-armed", runtime.ContainerID("c1"), engine, testAgentEnv(), discardLoggerRunner())
	if err != nil {
		t.Fatalf("StartAgent = %v", err)
	}
	if stream.stopping.Load() {
		t.Fatal("stopping is armed before Stop — a live agent's pipe close would be misread as an expected end")
	}

	if err := stream.Stop(); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if !stream.stopping.Load() {
		t.Fatal("Stop did not arm stopping — the reap's os.ErrClosed would be reported as `drain ended early` on every stop")
	}
}
