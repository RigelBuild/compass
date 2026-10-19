//go:build unix

// The agent exec tail: StartAgent spawns the first-party agent in a container and
// drains both its pipes to the diagnostic log. The compass.v1 traffic rides the
// per-container AgentGateway socket, so stdout/stderr carry no protocol — but both
// are drained continuously, since a full OS pipe buffer would stall the next write.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec" //nolint:depguard // runner exec: *exec.Cmd/*exec.ExitError types for the container agent stream lifecycle
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/RigelBuild/compass/go/internal/agentidentity"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// agentCommand is the argv the Runner execs to start the first-party agent in a
// container. The agent binary is installed in the image; it speaks compass.v1
// over the per-container socket (`AgentGateway`), not over its pipes.
var agentCommand = []string{"compass-agent"}

// AgentEnv is the container-side configuration the Runner hands the agent
// process at exec time. The agent reads each as an environment variable
// (packages/compass-agent/src/cli.ts): HOME locates the provider seed,
// COMPASS_WORKDIR is the session cwd, COMPASS_MODEL selects the model,
// COMPASS_PERSONA is the identity overlay appended to the system prompt,
// COMPASS_ROLE is the operator-set block-0 selector delivered as the
// container's customSystemPrompt; COMPASS_RESUME_SESSION_FILE is the absolute
// in-container path of a server-reconstructed session file the agent loads to
// resume. ContinueSession requests reuse of the agent's own current session on
// Reload. Empty Model, Persona, Role, or ResumeSessionFile is omitted rather
// than exported blank, so the agent falls back to its SDK default (or a fresh
// session) instead of receiving a value it must special-case.
// COMPASS_AGENT_BATCHING is omitted when empty.
type AgentEnv struct {
	// UID sets the agent exec user. Container tiers require it explicitly so the
	// agent does not inherit the capabilities used to arm egress.
	UID uint32
	// HomeDir is the agent's scoped HOME.
	HomeDir string
	// Workdir is the in-container checkout.
	Workdir string
	// AgentAccountID sets the identity used for jj and git commits.
	AgentAccountID string
	// Model is the model selector; empty uses the agent default.
	Model string
	// Batching is the Runner-wide idle-batching setting.
	Batching string
	// Persona is the server-authoritative identity overlay.
	Persona string
	// Role is the server-authoritative block-0 selector sent as customSystemPrompt.
	Role string
	// ResumeSessionFile is the absolute in-container path to a resumed session.
	ResumeSessionFile string
	// ContinueSession resumes the agent's current session on Reload.
	ContinueSession bool
	// SocketPath overrides the default gateway socket path; ConfigMountPath
	// overrides the default config-mount root when set.
	SocketPath      string
	ConfigMountPath string
}

// execSpec starts the agent unprivileged in its checkout with only its needed
// environment. Secrets are materialized at 0600 under the agent's HOME; passing
// them through exec arguments or a host-resolved env file would expose or fail.
func (e AgentEnv) execSpec() runtime.StreamingExecSpec {
	spec := runtime.NewStreamingExecSpec(agentCommand...).
		AsUser(strconv.FormatUint(uint64(e.UID), 10)).
		InDir(e.Workdir)
	spec.Env["HOME"] = e.HomeDir
	spec.Env["COMPASS_WORKDIR"] = e.Workdir
	if e.AgentAccountID != "" {
		email := agentidentity.AgentCommitEmail(e.AgentAccountID)
		spec.Env["JJ_USER"] = e.AgentAccountID
		spec.Env["JJ_EMAIL"] = email
		spec.Env["GIT_AUTHOR_EMAIL"] = email
		spec.Env["GIT_COMMITTER_EMAIL"] = email
		spec.Env["GIT_AUTHOR_NAME"] = e.AgentAccountID
		spec.Env["GIT_COMMITTER_NAME"] = e.AgentAccountID
	}
	if e.Model != "" {
		spec.Env["COMPASS_MODEL"] = e.Model
	}
	if e.Batching != "" {
		spec.Env["COMPASS_AGENT_BATCHING"] = e.Batching
	}
	if e.Persona != "" {
		spec.Env["COMPASS_PERSONA"] = e.Persona
	}
	if e.Role != "" {
		spec.Env["COMPASS_ROLE"] = e.Role
	}
	if e.ResumeSessionFile != "" {
		spec.Env["COMPASS_RESUME_SESSION_FILE"] = e.ResumeSessionFile
	}
	if e.ContinueSession {
		spec.Env["COMPASS_CONTINUE_SESSION"] = "1"
	}
	if e.SocketPath != "" {
		spec.Env["COMPASS_AGENT_SOCKET_PATH"] = e.SocketPath
	}
	if e.ConfigMountPath != "" {
		spec.Env["COMPASS_AGENT_CONFIG_MOUNT_PATH"] = e.ConfigMountPath
	}
	return spec
}

// AgentStream is a live agent exec. Stop terminates the in-container agent; its
// telemetry travels over the socket, so nothing pumps its pipes upward.
type AgentStream struct {
	sessionID string
	exec      *runtime.StreamingExec
	// drains is signalled by both drain goroutines as they return. The reaper joins
	// it after the reap, bounded by drainGrace from exit: every backend hands over
	// caller-owned pipes, so the reap leaves buffered output readable, and a
	// descendant holding a pipe delays the join but never the exit report.
	drains sync.WaitGroup
	// drainGrace is the post-exit join bound; tests shorten it via ServerLink.
	drainGrace time.Duration
	// closeStdout/closeStderr close each read end once, from its drain on return
	// or from closeDrains (Stop or the reaper), whichever comes first.
	closeStdout func() error
	closeStderr func() error
	// stopDrains ends both drains on teardown even if the pipes never reach EOF,
	// so a wedged read can't hold Stop past its bounded wait.
	stopDrains context.CancelFunc
	// stopping is set by Stop before it kills, so retireOnExit can tell a
	// deliberate stop from a self-exit and skip the ERRORED report.
	stopping atomic.Bool
	// waitOnce makes the reaper and Stop share ONE Process.Wait (podman's cmd.Wait
	// is not idempotent) without a lock held across it, so Stop can always Kill.
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
	// stderrTail keeps the last lines for the unexpected-exit record. Written by
	// the stderr drain, read by the reaper after a bounded join; the mutex covers a
	// drain still running because a descendant outlived that join.
	stderrMu    sync.Mutex
	stderrTail  []stderrTailLine
	stderrBytes int
	// drainsReleased mirrors drainCtx.Done(): it closes when the drain context is
	// cancelled, whichever path ends the exec — closeDrains from Stop or from
	// StartAgent's reaper on self-exit. Held as a channel, not the context (containedctx forbids
	// that in shipped state), so the ctx-node release is observable.
	drainsReleased <-chan struct{}
	// reaped closes when the reaper finishes (reap + exit classification), the
	// event tests and the host's self-exit handling wait on.
	reaped     chan struct{}
	log        *slog.Logger
	beforeWait func()
}

// SessionID returns the Server-side session id this stream carries.
func (s *AgentStream) SessionID() string { return s.sessionID }

// StartAgent spawns the agent in container id over ExecStreaming. Both pipes are
// drained to the diagnostic log continuously: the agent's protocol traffic rides
// the per-container socket, so anything on stdout/stderr is diagnostics. env
// carries the identity and configuration the exec runs with. The returned
// AgentStream lives until Stop or ctx cancellation terminates the in-container
// agent.
func (l *ServerLink) StartAgent(ctx context.Context, sessionID string, id runtime.WorkloadID, engine runtime.WorkloadRuntime, env AgentEnv, log *slog.Logger) (*AgentStream, error) {
	if log == nil {
		log = slog.Default()
	}
	xs, err := engine.ExecStreaming(ctx, id, env.execSpec())
	if err != nil {
		return nil, err
	}

	// Derived from the caller's ctx, never re-rooted: a Runner shutdown reaches
	// the drains, and Stop can end them on its own. The drains and reaper outlive
	// the Start command, so they drop its span.
	lifeCtx := otelx.WithoutSpan(ctx)
	drainCtx, stopDrains := context.WithCancel(lifeCtx)
	grace := l.drainGrace
	if grace == 0 {
		grace = drainGrace
	}
	stream := &AgentStream{
		sessionID: sessionID, exec: xs, stopDrains: stopDrains, drainGrace: grace,
		closeStdout: sync.OnceValue(xs.IO.Stdout.Close), closeStderr: sync.OnceValue(xs.IO.Stderr.Close),
		drainsReleased: drainCtx.Done(), reaped: make(chan struct{}), waitDone: make(chan struct{}),
		log: log, beforeWait: l.beforeWait,
	}

	stderr := &stderrLogger{
		limiter: newLineRateLimiter(),
		onLine:  func(text string, truncated bool) { stream.retainStderrLine(text, truncated) },
	}

	// Drain both pipes continuously so a full OS pipe buffer can never stall the
	// agent. Neither carries protocol traffic now, but an undrained pipe blocks
	// the writer just the same.
	stream.drains.Add(2)
	go func() {
		defer stream.drains.Done()
		stream.drainToLog(drainCtx, xs.IO.Stderr, "agent stderr", log, stderr)
		stream.logPipeClose(drainCtx, "agent stderr", stream.closeStderr())
	}()
	go func() {
		defer stream.drains.Done()
		stream.drainToLog(drainCtx, xs.IO.Stdout, "agent stdout", log, nil)
		stream.logPipeClose(drainCtx, "agent stdout", stream.closeStdout())
	}()

	// The reaper: reap first so a descendant holding a pipe cannot delay the exit
	// report; join the drains, bounded from exit, for a complete stderr tail; then
	// cut them loose (which also releases the drain ctx node) and log the exit.
	go func() {
		defer close(stream.reaped)
		if xs.Process == nil {
			// No handle means no exit to observe: pipe EOF is the only signal.
			stream.drains.Wait()
		}
		waitErr := stream.wait()
		stream.joinDrains()
		stream.closeDrains(lifeCtx)
		if shouldLogExit(lifeCtx, waitErr) {
			logUnexpectedExit(lifeCtx, log, stream, waitErr)
		}
	}()

	return stream, nil
}

// drainGrace bounds how long the drains may run past the agent's exit before
// they are cancelled. A drain normally ends as soon as it reads the pipe's
// buffered tail; the bound covers a descendant that inherited the pipe and
// outlives the agent.
const drainGrace = 5 * time.Second

// Stop terminates the in-container agent and waits for its exec to reap. Stop is
// the deliberate-teardown path (StopAgentSession, and the first half of Reload),
// so the SIGKILL it delivers is the intended outcome, not a failure. Once the
// exec is reaped Stop returns nil: the agent is gone, and a non-kill exit is the
// reaper's to log, so the result does not depend on which side reaped first.
//
// Order matters, and only one order terminates. The drains block in a read on
// pipes that stay open while the agent (or a descendant) lives — a quiet agent
// produces no line for a between-lines cancellation check to reach — so joining
// BEFORE the kill waits on goroutines that cannot finish yet. Kill, reap, close
// the pipes, then join.
func (s *AgentStream) Stop(ctx context.Context) error {
	return s.terminate(ctx)
}

// logPipeClose reports a failed read-end close. sync.OnceValue closes each end
// once, so the drain and closeDrains share one result; os.ErrClosed means the
// backend already closed it.
func (s *AgentStream) logPipeClose(ctx context.Context, msg string, err error) {
	if err != nil && !errors.Is(err, os.ErrClosed) {
		s.log.DebugContext(ctx, msg+" pipe close failed", slog.String("session_id", s.sessionID), slog.Any("error", err))
	}
}

func (s *AgentStream) terminate(ctx context.Context) error {
	s.stopping.Store(true)
	select {
	case <-s.waitDone:
		s.closeDrains(ctx)
		s.joinDrains()
		return nil
	default:
	}
	if s.exec.Process == nil {
		return errors.New("runner: agent exec has no process handle")
	}
	killErr := s.exec.Process.Kill()
	s.reap()
	// The pipes are caller-owned, so the reap does not close them; closing them
	// here keeps a descendant holding a pipe from stalling a deliberate stop.
	s.closeDrains(ctx)
	s.joinDrains()
	if killErr != nil {
		// The reap says the agent is gone; a late kill error (e.g. a microVM Signal
		// timeout) is not a Stop failure, as ChildHandle.Terminate treats it.
		s.log.DebugContext(ctx, "agent kill failed during stop", slog.String("session_id", s.sessionID), slog.Any("error", killErr))
	}
	// Reaped either way. A non-kill exit is the reaper's to log, so Stop reports
	// nil whether it or the reaper reaped first.
	return nil
}

func (s *AgentStream) wait() error {
	s.reap()
	return s.waitErr
}

// reap runs the one shared Process.Wait; Stop uses it without the exit error,
// which the reaper classifies and logs.
func (s *AgentStream) reap() {
	s.waitOnce.Do(func() {
		if s.beforeWait != nil {
			s.beforeWait()
		}
		if s.exec.Process != nil {
			s.waitErr = s.exec.Process.Wait()
		}
		close(s.waitDone)
	})
	<-s.waitDone
}

// closeDrains cancels the drain ctx, then closes both read ends. Cancelling first
// makes any closed-pipe error a drain reads an expected end on every backend.
func (s *AgentStream) closeDrains(ctx context.Context) {
	if s.stopDrains != nil {
		s.stopDrains()
	}
	if s.closeStdout != nil {
		s.logPipeClose(ctx, "agent stdout", s.closeStdout())
	}
	if s.closeStderr != nil {
		s.logPipeClose(ctx, "agent stderr", s.closeStderr())
	}
}

// joinDrains waits up to drainGrace for both drains to return, so a stuck drain
// delays the caller but never blocks it.
func (s *AgentStream) joinDrains() {
	done := make(chan struct{})
	go func() {
		s.drains.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.drainGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// isDeliberateKill recognizes deliberate termination across guest and podman waits.
func isDeliberateKill(err error) bool {
	if exitStatus, ok := errors.AsType[*runtime.ExitStatusError](err); ok {
		// Guest waits expose any signal; podman waits must specifically be SIGKILL.
		return exitStatus.Signal != 0
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

func shouldLogExit(ctx context.Context, waitErr error) bool {
	return !isDeliberateKill(waitErr) && ctx.Err() == nil
}

const maxStderrTailLines = 20
const maxStderrTailBytes = 16 * 1024

type stderrTailLine struct {
	text      string
	truncated bool
}

func (s *AgentStream) retainStderrLine(line string, truncated bool) {
	if len(line) > maxStderrTailBytes {
		start := len(line) - maxStderrTailBytes
		for start < len(line) && !utf8.RuneStart(line[start]) {
			start++
		}
		line = line[start:]
		truncated = true
	}
	line = strings.Clone(line)
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	s.stderrTail = append(s.stderrTail, stderrTailLine{text: line, truncated: truncated})
	s.stderrBytes += len(line)
	for len(s.stderrTail) > maxStderrTailLines || s.stderrBytes > maxStderrTailBytes {
		s.stderrBytes -= len(s.stderrTail[0].text)
		s.stderrTail[0] = stderrTailLine{}
		s.stderrTail = s.stderrTail[1:]
	}
}

func (s *AgentStream) stderrTailText() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	lines := make([]string, len(s.stderrTail))
	for i, line := range s.stderrTail {
		lines[i] = line.text
		if line.truncated {
			lines[i] += " [truncated]"
		}
	}
	return strings.Join(lines, "\n")
}

func logUnexpectedExit(ctx context.Context, log *slog.Logger, s *AgentStream, err error) {
	exitResult := "clean"
	if err != nil {
		exitResult = err.Error()
	}
	log.ErrorContext(ctx, "agent exited unexpectedly", slog.String("session_id", s.sessionID),
		slog.String("stderr_tail", s.stderrTailText()), slog.String("exit_result", exitResult))
}

// maxInfoStderrLine caps the Info copy of a stderr line; the full line still
// reaches the tail and Debug. The limiter keeps a chatty agent off the journal.
const maxInfoStderrLine = 8 * 1024
const stderrLinesPerSecond = 20
const stderrLineBurst = 100

// maxLoggedLine bounds how much of one pipe line reaches the diagnostic log.
// Past it the remainder is consumed and discarded rather than buffered, so a
// pathological line costs a truncated log entry instead of unbounded memory.
const maxLoggedLine = 1024 * 1024

// drainReaderSize is the drain's read buffer. Its size does not affect
// correctness: readBoundedLine's truncation flag is measured from `seen`, the
// exact running byte total for the line, so it is exact regardless of how the
// line is split across reads — buffer-size-independent.
const drainReaderSize = 64 * 1024

type stderrLineSink func(text string, truncated bool)

type lineRateLimiter struct {
	tokens     float64
	lastRefill time.Time
	now        func() time.Time
}

func newLineRateLimiter() *lineRateLimiter {
	return &lineRateLimiter{tokens: stderrLineBurst, lastRefill: time.Now(), now: time.Now}
}

func (l *lineRateLimiter) allow() bool {
	now := l.now()
	l.tokens = min(float64(stderrLineBurst), l.tokens+now.Sub(l.lastRefill).Seconds()*stderrLinesPerSecond)
	l.lastRefill = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// stderrLogger is one stderr drain's tail sink and Info rate limiter. Only that
// drain's goroutine touches it, so it needs no lock.
type stderrLogger struct {
	limiter      *lineRateLimiter
	droppedLines int
	lastFlush    time.Time
	onLine       stderrLineSink
}

func (s *stderrLogger) flushDropped(ctx context.Context, log *slog.Logger, sessionID string, final bool) {
	if s == nil || s.droppedLines == 0 {
		return
	}
	now := s.limiter.now()
	if !final && !s.lastFlush.IsZero() && now.Sub(s.lastFlush) < time.Second {
		return
	}
	log.WarnContext(ctx, "agent stderr rate-limited", slog.String("session_id", sessionID), slog.Int("dropped_lines", s.droppedLines))
	s.droppedLines = 0
	s.lastFlush = now
}

// drainToLog keeps reading each pipe: an unread OS pipe buffer blocks the agent.
// Oversized lines are truncated but fully consumed, so the next line stays aligned.
// Closing the pipe unblocks reads during teardown; joins remain bounded.
func (s *AgentStream) drainToLog(ctx context.Context, pipe io.Reader, msg string, log *slog.Logger, stderr *stderrLogger) {
	sessionID := s.sessionID
	r := bufio.NewReaderSize(pipe, drainReaderSize)
	defer stderr.flushDropped(ctx, log, s.sessionID, true)
	for {
		if ctx.Err() != nil {
			return // teardown: closeDrains is closing the pipe under us.
		}
		line, err := readBoundedLine(r, maxLoggedLine)
		if len(line) > 0 || err == nil {
			logDrainLine(ctx, log, s.sessionID, msg, string(line), errors.Is(err, errLineTruncated), stderr)
		}
		switch {
		// The expected ends, tested FIRST: a truncated final line joins its error
		// with the terminal one, and "keep going" would spin on a dead pipe.

		// closeDrains cancels ctx before it closes the pipe, so the os.ErrClosed it
		// causes lands here; one with ctx still live is a fault (below).
		case errors.Is(err, io.EOF), ctx.Err() != nil:
			return // agent exit or teardown: the expected ends.
		// A truncated line comes back as the sentinel BY VALUE; a truncated line
		// that ALSO faulted joins the fault in, and the ends above peeled off the
		// terminal faults — so an identity test keeps draining on pure truncation
		// while a fault-carrying join falls through to the warn.
		case err == nil, err == errLineTruncated: //nolint:errorlint // identity is the point: the by-value sentinel is pure truncation; a truncated line that also faulted is a *joinError, which must fall through to the warn — errors.Is would match that join and swallow the fault.
			continue
		default:
			// Never silent: the pipe is no longer being drained, which stalls
			// the agent's next write, so the reason has to reach the log.
			log.WarnContext(ctx, msg+" drain ended early",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return
		}
	}
}

func logDrainLine(ctx context.Context, log *slog.Logger, sessionID, msg, text string, truncated bool, stderr *stderrLogger) {
	if stderr == nil {
		attrs := []any{slog.String("session_id", sessionID), slog.String("line", text)}
		if truncated {
			attrs = append(attrs, slog.Bool("truncated", true))
		}
		log.DebugContext(ctx, msg, attrs...)
		return
	}
	stderr.onLine(text, truncated)
	logText := clipLine(text, maxInfoStderrLine)
	truncated = truncated || len(logText) < len(text)
	attrs := []any{slog.String("session_id", sessionID), slog.String("line", logText)}
	if truncated {
		attrs = append(attrs, slog.Bool("truncated", true))
	}
	if stderr.limiter.allow() {
		log.InfoContext(ctx, msg, attrs...)
		stderr.flushDropped(ctx, log, sessionID, false)
		return
	}
	stderr.droppedLines++
	log.DebugContext(ctx, msg, attrs...)
}

func clipLine(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	end := limit
	for end > 0 && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end]
}

// errLineTruncated reports that a line exceeded the cap: its prefix is returned
// and the remainder was consumed. Draining continues — unlike bufio.Scanner's
// ErrTooLong, which ends the scan and leaves the pipe unread forever. When EOF
// also ended the line it is joined, so a caller can test for either.
var errLineTruncated = errors.New("line truncated")

// readBoundedLine returns at most limit bytes and consumes any remainder, keeping
// the reader aligned. It tracks truncation so exact-limit lines remain complete.
func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var (
		line []byte
		seen int     // total bytes read for this line, terminator included
		tail [2]byte // the last two bytes seen, for a CRLF split across chunks
	)
	for {
		chunk, err := r.ReadSlice('\n')
		// Measure against the running total, never per chunk. The terminator is not
		// payload, but only the FINAL chunk can hold one, so discounting a chunk's
		// trailing CR would discount ordinary payload and under-report a line that
		// is over the cap by exactly that byte.
		if chunk != nil {
			seen += len(chunk)
			for _, b := range chunk[max(0, len(chunk)-2):] {
				tail[0], tail[1] = tail[1], b
			}
			if room := limit - len(line); room > 0 {
				line = append(line, chunk[:min(len(chunk), room)]...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // more of the same line; keep consuming.
		}
		// The whole line is in hand, so discount its one real terminator. `tail`
		// carries the last two bytes across the chunk boundary (a CRLF can straddle
		// one). It is also the ONLY witness to the terminator: `line` may have been
		// clipped at the cap, and an unterminated final line legitimately ends "\r".
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

// trimEOL removes CR only when it preceded a newline. A clipped line may lose its
// newline, while an unterminated payload may legitimately end in CR; only the
// reader's terminated flag distinguishes these cases.
func trimEOL(b []byte, terminated bool) []byte {
	if !terminated {
		return b
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}
