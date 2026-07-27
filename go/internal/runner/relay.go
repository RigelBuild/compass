//go:build unix

// The agent relay tail (design compass-0.6 §T5, the Runner side that lives in
// T4): StartAgent spawns the first-party agent in a container over the built
// streaming exec. The agent's frames ride the AgentGateway socket the Runner
// bind-mounts into the container; the newline-framed compass.v1 stdout this
// package scans and relays up the PublishEvents client-stream — Runner-sequenced
// — is the legacy carrier, retained until it is retired. stderr is drained
// continuously so a chatty agent can never fill the OS pipe buffer and stall the
// frame stream. A frame whose oneof variant is unset or unrecognized is logged
// and counted, never silently dropped.
package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// agentCommand is the argv the Runner execs to start the first-party agent in a
// container. The agent binary is installed in the image; it speaks compass.v1
// over the AgentGateway socket bind-mounted into the container, and also over
// the newline-framed stdio contract on its stdin/stdout — the legacy carrier
// retained until it is retired.
var agentCommand = []string{"compass-agent"}

// AgentEnv is the container-side configuration the Runner hands the agent
// process at exec time. The agent reads each as an environment variable
// (packages/compass-agent/src/cli.ts): HOME locates the provider seed,
// COMPASS_WORKDIR is the session cwd, COMPASS_MODEL selects the model. Empty
// Model is omitted rather than exported blank, so the agent falls back to its
// SDK default instead of receiving a value it must special-case.
type AgentEnv struct {
	// UID is the agent user the exec runs as. Set explicitly because podman
	// strips the container's ambient capabilities only when --user is passed:
	// without it the agent's own process inherits the NET_ADMIN the container
	// carries to arm its firewall (runtime/agent.go:212) and can flush the
	// ruleset meant to contain it, violating runtime/egress.go:6-10. This
	// closes that on this exec path. A follow-up removes the capability from the
	// agent container entirely so no exec path can reach the ruleset.
	UID uint32
	// HomeDir is the agent's scoped $HOME.
	HomeDir string
	// Workdir is the in-container checkout the session runs in.
	Workdir string
	// Model is the model selector, or empty for the agent's default.
	Model string
}

// execSpec builds the streaming exec that starts the agent: unprivileged, in
// the checkout, carrying exactly the vars the agent reads.
func (e AgentEnv) execSpec() runtime.StreamingExecSpec {
	spec := runtime.NewStreamingExecSpec(agentCommand...).
		AsUser(strconv.FormatUint(uint64(e.UID), 10)).
		InDir(e.Workdir)
	spec.Env["HOME"] = e.HomeDir
	spec.Env["COMPASS_WORKDIR"] = e.Workdir
	if e.Model != "" {
		spec.Env["COMPASS_MODEL"] = e.Model
	}
	return spec
}

// AgentStream is a live relayed agent: the streaming exec handle plus the
// publisher pumping its frames upward. Stop terminates the in-container agent and
// drains the relay.
type AgentStream struct {
	sessionID string
	exec      *runtime.StreamingExec
	publisher *eventPublisher
	log       *slog.Logger
}

// SessionID returns the Server-side session id this stream relays for.
func (s *AgentStream) SessionID() string { return s.sessionID }

// StartAgent spawns the agent in container id over ExecStreaming and starts
// relaying its stdout frames up the PublishEvents stream under sessionID. env
// carries the identity and configuration the exec runs with. stderr is drained
// to the diagnostic log continuously. The returned AgentStream lives until Stop
// or ctx cancellation terminates the in-container agent.
func (l *ServerLink) StartAgent(ctx context.Context, sessionID string, id runtime.ContainerID, engine runtime.ContainerRuntime, env AgentEnv, log *slog.Logger) (*AgentStream, error) {
	if log == nil {
		log = slog.Default()
	}
	xs, err := engine.ExecStreaming(ctx, id, env.execSpec())
	if err != nil {
		return nil, err
	}

	pub := newEventPublisher(l.client, sessionID, log)
	stream := &AgentStream{sessionID: sessionID, exec: xs, publisher: pub, log: log}

	// Drain stderr continuously so the OS pipe buffer never fills and stalls the
	// agent / the stdout frame stream (design.md:1467-1470).
	go drainStderr(xs.IO.Stderr, sessionID, log)
	// Relay stdout frames upward until the agent's stdout closes or ctx ends.
	go pub.relay(ctx, xs.IO.Stdout)

	return stream, nil
}

// Stop terminates the in-container agent and waits for its exec to reap. Stop is
// the deliberate-teardown path (StopAgentSession, and the first half of Reload),
// so the SIGKILL Terminate delivers is the intended outcome, not a failure: the
// exec's own Terminate is Kill+Wait, and Wait on a SIGKILLed child returns a
// "signal: killed" *exec.ExitError (runtime/podman.go:212-218). Treat that
// deliberate-kill exit as success so a normal stop is not reported as an error;
// any other error (a real spawn/reap fault, or a non-signal exit) propagates.
func (s *AgentStream) Stop() error {
	if err := s.exec.Process.Terminate(); err != nil && !isDeliberateKill(err) {
		return err
	}
	return nil
}

// isDeliberateKill reports whether err is the exit of a process we SIGKILLed on
// purpose — an *exec.ExitError whose wait status is "terminated by SIGKILL".
// That is exactly the outcome Terminate produces on the deliberate-teardown
// path, so Stop treats it as success while still surfacing any other failure.
func isDeliberateKill(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// drainStderr copies the agent's stderr to the diagnostic log line by line. It
// runs for the life of the exec; an EOF (agent exit) ends it quietly.
func drainStderr(stderr io.Reader, sessionID string, log *slog.Logger) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Debug("agent stderr", slog.String("session_id", sessionID), slog.String("line", sc.Text()))
	}
}

// eventPublisher owns the PublishEvents client-stream and stamps a monotonic
// sequence on every relayed frame (OQ6 Runner-sequenced, for in-transit gap
// detection).
//
// Scope (SEA-1243 T4): the sequence is per-session (one eventPublisher, one
// atomic counter, per StartAgent), which is exact for the single-Runner /
// single-live-session MVP T4 targets. The frozen contract states the sequence
// as per-*Runner* across the whole event stream; hoisting the counter onto the
// shared Runner link so gap detection holds across concurrent multi-session
// streams is deferred with the rest of the multi-session lifecycle to T9
// (go-toolchain-default.md:979). Until then the gap guarantee holds per session.
type eventPublisher struct {
	client    compassv1internalconnect_RunnerServiceClient
	sessionID string
	log       *slog.Logger
	seq       atomic.Uint64
}

// compassv1internalconnect_RunnerServiceClient is the narrow client surface the
// publisher needs — just PublishEvents — kept as a local alias so the publisher
// is unit-testable against a fake without the full generated client.
type compassv1internalconnect_RunnerServiceClient interface {
	PublishEvents(ctx context.Context) *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse]
}

func newEventPublisher(client compassv1internalconnect_RunnerServiceClient, sessionID string, log *slog.Logger) *eventPublisher {
	return &eventPublisher{client: client, sessionID: sessionID, log: log}
}

// relay reads newline-delimited protojson AgentFrame lines off the agent's
// stdout and sends each up the PublishEvents stream, Runner-sequenced. A line
// that fails to decode is logged and skipped (a malformed frame is not fatal to
// the session). The stream is closed and its ack awaited when stdout ends.
func (p *eventPublisher) relay(ctx context.Context, stdout io.Reader) {
	stream := p.client.PublishEvents(ctx)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		frame := &compassv1internal.AgentFrame{}
		if err := protojson.Unmarshal(line, frame); err != nil {
			p.log.Warn("skipping undecodable agent frame",
				slog.String("session_id", p.sessionID), slog.String("error", err.Error()))
			continue
		}
		req := &compassv1internal.PublishEventsRequest{
			RunnerSeq: p.seq.Add(1),
			SessionId: p.sessionID,
			Frame:     frame,
		}
		if err := stream.Send(req); err != nil {
			p.log.Warn("publish events send failed; ending relay",
				slog.String("session_id", p.sessionID), slog.String("error", err.Error()))
			break
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil && !errors.Is(err, io.EOF) {
		p.log.Debug("publish events close",
			slog.String("session_id", p.sessionID), slog.String("error", err.Error()))
	}
}
