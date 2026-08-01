//go:build unix

// The agent exec tail: StartAgent spawns the first-party agent in a container
// over the built streaming exec and drains both its pipes to the diagnostic log.
// The agent's compass.v1 traffic rides the per-container `AgentGateway` socket,
// so stdout and stderr carry no protocol — but both are drained continuously,
// because a full OS pipe buffer would stall the agent's next write regardless of
// what the bytes mean.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// agentCommand is the argv the Runner execs to start the first-party agent in a
// container. The agent binary is installed in the image; it speaks compass.v1
// over the per-container socket (`AgentGateway`), not over its pipes.
var agentCommand = []string{"compass-agent"}

// AgentEnv is the container-side configuration the Runner hands the agent
// process at exec time. The agent reads each as an environment variable
// (packages/compass-agent/src/cli.ts): HOME locates the provider seed,
// COMPASS_WORKDIR is the session cwd, COMPASS_MODEL selects the model,
// COMPASS_PERSONA is the identity overlay appended to the system prompt. Empty
// Model or Persona is omitted rather than exported blank, so the agent falls
// back to its SDK default instead of receiving a value it must special-case.
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
	// Persona is the server-authoritative identity overlay, or empty for none.
	Persona string
}

// execSpec builds the streaming exec that starts the agent: unprivileged, in
// the checkout, carrying exactly the vars the agent reads. Env-delivery secrets
// are NOT passed on this exec: the Runner's materializer writes them to the
// 0600 in-container $HOME/.compass/env (SEA-1327 T5), and the agent sources that
// file from its own namespace at startup. They are deliberately not `-e
// KEY=VALUE` here (host-process-list visible) nor `--env-file` (podman resolves
// that path host-side, where the container-internal file does not exist).
func (e AgentEnv) execSpec() runtime.StreamingExecSpec {
	spec := runtime.NewStreamingExecSpec(agentCommand...).
		AsUser(strconv.FormatUint(uint64(e.UID), 10)).
		InDir(e.Workdir)
	spec.Env["HOME"] = e.HomeDir
	spec.Env["COMPASS_WORKDIR"] = e.Workdir
	if e.Model != "" {
		spec.Env["COMPASS_MODEL"] = e.Model
	}
	if e.Persona != "" {
		spec.Env["COMPASS_PERSONA"] = e.Persona
	}
	return spec
}

// AgentStream is a live agent exec. Stop terminates the in-container agent; its
// telemetry travels over the socket, so nothing pumps its pipes upward.
type AgentStream struct {
	sessionID string
	exec      *runtime.StreamingExec
	// drains is signalled by both drain goroutines as they return. Stop waits on
	// it before reaping: these are cmd.StdoutPipe/StderrPipe readers, and
	// os/exec makes Wait close the pipes, so reaping first would race the drains
	// off the end of the stream (os/exec: "it is incorrect to call Wait before
	// all reads from the pipe have completed").
	drains sync.WaitGroup
	// stopDrains ends both drains on teardown even if the pipes never reach EOF,
	// so a wedged read can't hold Stop past its bounded wait.
	stopDrains context.CancelFunc
	// stopping is set by Stop before it reaps. The reap closes the pipes, so a
	// drain's os.ErrClosed is ordinary during a deliberate stop and a genuine
	// fault at any other time; the flag says which, where ctx cannot — Stop
	// must Terminate before it cancels (see Stop), so the reap's ErrClosed
	// always arrives while drainCtx is still live.
	stopping atomic.Bool
	// drainsReleased mirrors drainCtx.Done(): it closes when the drain context
	// is cancelled, whichever path ends the exec — Stop's endDrains on
	// deliberate teardown, or StartAgent's reaper once both drains return on a
	// self-exit. Held as a channel rather than the context itself, which
	// containedctx forbids in shipped state, so the ctx-node release is
	// observable without re-rooting a context on the struct.
	drainsReleased <-chan struct{}
}

// SessionID returns the Server-side session id this stream carries.
func (s *AgentStream) SessionID() string { return s.sessionID }

// StartAgent spawns the agent in container id over ExecStreaming. Both pipes are
// drained to the diagnostic log continuously: the agent's protocol traffic rides
// the per-container socket, so anything on stdout/stderr is diagnostics. env
// carries the identity and configuration the exec runs with. The returned
// AgentStream lives until Stop or ctx cancellation terminates the in-container
// agent.
func (l *ServerLink) StartAgent(ctx context.Context, sessionID string, id runtime.ContainerID, engine runtime.ContainerRuntime, env AgentEnv, log *slog.Logger) (*AgentStream, error) {
	if log == nil {
		log = slog.Default()
	}
	xs, err := engine.ExecStreaming(ctx, id, env.execSpec())
	if err != nil {
		return nil, err
	}

	// Derived from the caller's ctx, never re-rooted: a Runner shutdown reaches
	// the drains, and Stop can end them on its own.
	drainCtx, stopDrains := context.WithCancel(ctx)
	stream := &AgentStream{sessionID: sessionID, exec: xs, stopDrains: stopDrains, drainsReleased: drainCtx.Done()}

	// Drain both pipes continuously so a full OS pipe buffer can never stall the
	// agent. Neither carries protocol traffic now, but an undrained pipe blocks
	// the writer just the same.
	stream.drains.Add(2)
	go func() {
		defer stream.drains.Done()
		stream.drainToLog(drainCtx, xs.IO.Stderr, "agent stderr", log)
	}()
	go func() {
		defer stream.drains.Done()
		stream.drainToLog(drainCtx, xs.IO.Stdout, "agent stdout", log)
	}()

	// Release the ctx node whichever way the exec ends. Stop cancels drainCtx
	// on the deliberate-teardown path, but a self-exiting agent (pipes reach
	// EOF, both drains return, no Stop/Reload/Close) would otherwise leave the
	// context.WithCancel node attached to the caller's long-lived ctx until
	// Runner shutdown. Waiting on the same drains and cancelling once they
	// finish covers that path; it is idempotent with Stop's endDrains, since a
	// CancelFunc is safe to call more than once. This reaper exits the moment
	// the drains do, so it adds no lifetime of its own.
	go func() {
		stream.drains.Wait()
		stopDrains()
	}()

	return stream, nil
}

// drainGrace bounds how long Stop waits for the drains to finish before reaping.
// A drain normally ends the instant the child's exit closes the pipe; the bound
// exists so a pathological reader can delay teardown but never block it.
const drainGrace = 5 * time.Second

// Stop terminates the in-container agent and waits for its exec to reap. Stop is
// the deliberate-teardown path (StopAgentSession, and the first half of Reload),
// so the SIGKILL Terminate delivers is the intended outcome, not a failure: the
// exec's own Terminate is Kill+Wait, and Wait on a SIGKILLed child returns a
// "signal: killed" *exec.ExitError (runtime/podman.go:212-218). Treat that
// deliberate-kill exit as success so a normal stop is not reported as an error;
// any other error (a real spawn/reap fault, or a non-signal exit) propagates.
//
// Order matters, and only one order terminates. The drains block in a read on
// pipes only the child's death closes — a quiet agent produces no line for a
// between-lines cancellation check to reach — so joining BEFORE Terminate waits
// on goroutines that cannot finish yet, and every teardown pays the full grace.
// Terminate first, then join: the reap closes the pipes, both drains fall out on
// EOF, and the join collects them. os/exec's "don't Wait before reads complete"
// still holds, since the drains end on that same close rather than mid-read;
// the join is what makes their exit observable instead of merely likely.
func (s *AgentStream) Stop() error {
	s.stopping.Store(true)
	err := s.exec.Process.Terminate()
	s.endDrains()
	if err != nil && !isDeliberateKill(err) {
		return err
	}
	return nil
}

// endDrains cancels both drains and waits up to drainGrace for them to return.
// The cancel covers the case the reap left a pipe open; the bound means a stuck
// drain delays teardown but never blocks it.
func (s *AgentStream) endDrains() {
	if s.stopDrains != nil {
		s.stopDrains()
	}
	done := make(chan struct{})
	go func() {
		s.drains.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainGrace):
	}
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

// maxLoggedLine bounds how much of one pipe line reaches the diagnostic log.
// Past it the remainder is consumed and discarded rather than buffered, so a
// pathological line costs a truncated log entry instead of unbounded memory.
const maxLoggedLine = 1024 * 1024

// drainReaderSize is the drain's read buffer. Its size does not affect
// correctness: readBoundedLine's truncation flag is measured from `seen`, the
// exact running byte total for the line, so it is exact regardless of how the
// line is split across reads — buffer-size-independent.
const drainReaderSize = 64 * 1024

// drainToLog copies one of the agent's pipes to the diagnostic log line by line
// under msg ("agent stdout" / "agent stderr"). It runs for the life of the exec;
// an EOF (agent exit) ends it quietly, and a cancelled ctx ends it promptly.
//
// Draining is not optional even though neither pipe carries protocol traffic: an
// unread pipe fills its OS buffer and blocks the agent's next write. That makes
// "keep reading" the drain's actual contract, so it must survive a line it can't
// log. A line over maxLoggedLine is logged truncated (flagged `truncated`) and
// the rest consumed, and a read error is reported rather than swallowed — a
// silent exit here wedges the agent with no diagnostic anywhere.
//
// ctx is checked between lines rather than mid-read: a blocked read ends when
// teardown closes the pipe, and Stop's join is bounded, so a cancel can never
// hold teardown open. Closing the reader here instead would race the writer.
func (s *AgentStream) drainToLog(ctx context.Context, pipe io.Reader, msg string, log *slog.Logger) {
	sessionID := s.sessionID
	r := bufio.NewReaderSize(pipe, drainReaderSize)
	for {
		if ctx.Err() != nil {
			return // teardown: the pipe is being closed under us.
		}
		line, err := readBoundedLine(r, maxLoggedLine)
		if len(line) > 0 || err == nil {
			attrs := []any{slog.String("session_id", sessionID), slog.String("line", string(line))}
			if errors.Is(err, errLineTruncated) {
				attrs = append(attrs, slog.Bool("truncated", true))
			}
			log.Debug(msg, attrs...)
		}
		switch {
		// The expected ends, tested FIRST: a truncated final line returns its
		// error joined with the terminal one, and treating that as "keep going"
		// would spin on a dead pipe.
		//
		// os.ErrClosed is ordinary only during a deliberate stop: Terminate
		// reaps via cmd.Wait, which CLOSES the pipe rather than EOF-ing it, so
		// every stop ends the drains this way and warning unconditionally would
		// fire on 100% of stops. ctx cannot make that distinction — Stop must
		// Terminate before it cancels, so the reap's ErrClosed always arrives
		// while ctx is still live — hence the explicit flag. Outside a stop the
		// same error means a live agent's pipe closed under us and the drain is
		// dead, which is exactly what the warn below exists to report.
		case errors.Is(err, io.EOF), ctx.Err() != nil:
			return // agent exit or teardown: the expected ends.
		case errors.Is(err, os.ErrClosed) && s.stopping.Load():
			return // the reap closed the pipe on the deliberate-stop path.
		case err == nil, errors.Is(err, errLineTruncated):
			continue
		default:
			// Never silent: the pipe is no longer being drained, which stalls
			// the agent's next write, so the reason has to reach the log.
			log.Warn(msg+" drain ended early",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return
		}
	}
}

// errLineTruncated reports that a line exceeded the cap: its prefix is returned
// and the remainder was consumed. Draining continues — unlike bufio.Scanner's
// ErrTooLong, which ends the scan and leaves the pipe unread forever. When EOF
// also ended the line it is joined, so a caller can test for either.
var errLineTruncated = errors.New("line truncated")

// readBoundedLine reads one newline-terminated line, returning at most limit bytes
// of it. When the line is longer, the prefix is returned with errLineTruncated
// and the remainder is consumed so the reader stays aligned to the next line.
//
// Truncation is tracked as it happens rather than inferred from the returned
// length: a line of exactly limit bytes is NOT truncated, and one cut short by
// EOF before its newline still is. Both cases are invisible to a length test.
func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var (
		line []byte
		seen int     // total bytes read for this line, terminator included
		tail [2]byte // the last two bytes seen, for a CRLF split across chunks
	)
	for {
		chunk, err := r.ReadSlice('\n')
		// Measure against the running total, never per chunk. The terminator is
		// not payload — a line of exactly limit bytes plus its EOL loses
		// nothing — but only the FINAL chunk can hold one, so discounting a
		// chunk's trailing CR discounts a byte that is ordinary payload and
		// under-reports a line that is over the cap by exactly that byte.
		seen += len(chunk)
		for _, b := range chunk[max(0, len(chunk)-2):] {
			tail[0], tail[1] = tail[1], b
		}
		if room := limit - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // more of the same line; keep consuming.
		}
		// The whole line is in hand, so discount its one real terminator. `tail`
		// carries the last two bytes across the chunk boundary, since a CRLF can
		// straddle one and leave this chunk holding a bare "\n". It is also the
		// ONLY witness to what terminated the line: `line` may have been clipped
		// at the cap, so its own suffix says nothing about the terminator, and an
		// unterminated final line legitimately ends in a payload "\r".
		terminated := seen > 0 && tail[1] == '\n'
		payload := seen
		if terminated {
			payload--
			if payload > 0 && tail[0] == '\r' {
				payload--
			}
		}
		truncated := payload > limit
		line = trimEOL(line, terminated && !truncated)
		switch {
		case err != nil && truncated:
			return line, errors.Join(errLineTruncated, err)
		case err != nil:
			return line, err
		case truncated:
			return line, errLineTruncated
		default:
			return line, nil
		}
	}
}

// trimEOL drops the line's terminator: a trailing newline and, only because that
// newline followed it, its CR. `terminated` is threaded in rather than sniffed
// from `b`, because `b` cannot answer the question. Two cases defeat a suffix
// test: a line clipped at the cap has already lost its newline, so it looks
// unterminated; and an unterminated final line — an agent dying mid-write, or a
// progress-bar write — legitimately ENDS in a payload "\r" that stripping would
// silently drop from the log. Only readBoundedLine's `tail` witnesses the real
// terminator, and it is the same witness the truncation arithmetic uses, so the
// logged bytes and the measured length can never disagree about what counted.
func trimEOL(b []byte, terminated bool) []byte {
	if !terminated {
		return b
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}
