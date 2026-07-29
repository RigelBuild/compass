//go:build unix

package runner

// readBoundedLine is the drain's whole safety property in one function: it must
// keep the reader aligned to the next line no matter what the agent wrote, and
// it must say when it dropped bytes. The drain-level tests reach exactly one of
// its shapes (a long line then a short one), so every boundary is covered here
// instead — against a plain strings.Reader, where an exact-size input is cheap
// and a read error is actually producible (an io.Pipe cannot fail on read).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// readAllSized is readAll with the reader's buffer size exposed. The chunking
// boundary is where the payload measurement is hardest to get right, so a test
// that needs a CR to land on a specific chunk edge sets the size directly
// rather than padding its input to hit the default.
func readAllSized(t *testing.T, src io.Reader, limit, bufSize int) []boundedLine {
	t.Helper()
	r := bufio.NewReaderSize(src, bufSize)
	var out []boundedLine
	for range 64 { // bounded: a spin bug fails loudly instead of hanging
		line, err := readBoundedLine(r, limit)
		out = append(out, boundedLine{
			text:      string(line),
			truncated: errors.Is(err, errLineTruncated),
			eof:       errors.Is(err, io.EOF),
			err:       err,
		})
		if err != nil && !errors.Is(err, errLineTruncated) {
			return out
		}
		if errors.Is(err, io.EOF) {
			return out
		}
	}
	t.Fatal("readBoundedLine did not terminate within 64 iterations — it is spinning")
	return nil
}

// readAll drives readBoundedLine to exhaustion, returning one record per line.
func readAll(t *testing.T, src io.Reader, limit int) []boundedLine {
	t.Helper()
	return readAllSized(t, src, limit, 16)
}

type boundedLine struct {
	text      string
	truncated bool
	eof       bool
	err       error
}

// The size boundaries around the cap, under BOTH line endings. A line of
// exactly limit bytes fits and is NOT truncated; one byte more is. Inferring
// truncation from the returned length cannot tell those apart and reports the
// exact fit as clipped.
//
// The two dimensions must cross. trimEOL strips the CR, so if the payload
// measurement counts it, an exactly-limit CRLF line measures one byte over and
// is reported clipped though nothing was lost. Sizes under LF only, plus CRLF
// only at a short length, leaves that one cell — the only broken one — dark.
func TestReadBoundedLineSizeBoundaries(t *testing.T) {
	const limit = 8
	sizes := []struct {
		name      string
		payload   string
		wantText  string
		truncated bool
	}{
		{"under the cap", "abc", "abc", false},
		{"one under", strings.Repeat("a", limit-1), strings.Repeat("a", limit-1), false},
		{"exactly the cap", strings.Repeat("a", limit), strings.Repeat("a", limit), false},
		{"one over", strings.Repeat("a", limit+1), strings.Repeat("a", limit), true},
		{"far over", strings.Repeat("a", limit*10), strings.Repeat("a", limit), true},
		{"empty line", "", "", false},
	}
	endings := []struct {
		name string
		eol  string
	}{
		{"LF", "\n"},
		{"CRLF", "\r\n"},
	}
	for _, eol := range endings {
		for _, tc := range sizes {
			t.Run(eol.name+"/"+tc.name, func(t *testing.T) {
				got := readAll(t, strings.NewReader(tc.payload+eol.eol), limit)
				if got[0].text != tc.wantText {
					t.Errorf("text = %q, want %q", got[0].text, tc.wantText)
				}
				if got[0].truncated != tc.truncated {
					t.Errorf("truncated = %v, want %v (the EOL is not payload)", got[0].truncated, tc.truncated)
				}
			})
		}
	}
}

// An over-long line ended by EOF rather than a newline must STILL be flagged.
// This is the shape of an agent dying mid-write — the final unterminated blob
// (a panic dump) is the most diagnostically valuable output in the log, and
// reporting it as a clean line hides that bytes were dropped.
func TestReadBoundedLineFlagsTruncationAtEOF(t *testing.T) {
	const limit = 8
	got := readAll(t, strings.NewReader(strings.Repeat("a", limit+8)), limit) // no trailing \n

	if got[0].text != strings.Repeat("a", limit) {
		t.Fatalf("text = %q, want the %d-byte prefix", got[0].text, limit)
	}
	if !got[0].truncated {
		t.Error("truncated = false, want true — 8 bytes were dropped with no indication")
	}
	if !got[0].eof {
		t.Error("eof = false, want true — the caller must still see the stream ended")
	}
}

// A final line with no trailing newline is real output and must be returned,
// not swallowed as an empty EOF read.
func TestReadBoundedLineReturnsUnterminatedFinalLine(t *testing.T) {
	got := readAll(t, strings.NewReader("first\nlast-no-newline"), 64)

	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (%+v)", len(got), got)
	}
	if got[0].text != "first" {
		t.Errorf("line 1 = %q, want %q", got[0].text, "first")
	}
	if got[1].text != "last-no-newline" {
		t.Errorf("line 2 = %q, want %q — an unterminated final line is still output", got[1].text, "last-no-newline")
	}
	if !got[1].eof {
		t.Error("line 2 eof = false, want true")
	}
}

// A clean EOF right after a terminated line yields an empty record the drain
// must not log. This pins the other half of drainToLog's `len(line) > 0 || err
// == nil` guard: a blank line IS logged, a bare EOF is NOT.
func TestReadBoundedLineCleanEOFIsEmpty(t *testing.T) {
	got := readAll(t, strings.NewReader("only\n"), 64)

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (the line, then a bare EOF): %+v", len(got), got)
	}
	if got[1].text != "" || !got[1].eof {
		t.Errorf("final record = %q eof=%v, want empty text at EOF", got[1].text, got[1].eof)
	}
}

// CRLF input: the carriage return is stripped with the newline, so a Windows-
// style or terminal-emitted line does not carry a trailing \r into the log.
func TestReadBoundedLineStripsCRLF(t *testing.T) {
	got := readAll(t, strings.NewReader("carriage\r\nplain\n"), 64)

	if got[0].text != "carriage" {
		t.Errorf("CRLF line = %q, want %q (the \\r must be stripped)", got[0].text, "carriage")
	}
	if got[1].text != "plain" {
		t.Errorf("LF line = %q, want %q", got[1].text, "plain")
	}
}

// A read error that is not EOF propagates to the caller, which is what lets the
// drain log why it stopped instead of dying silently. errReadFailed stands in
// for a pipe fault, which an io.Pipe cannot produce on the read side.
func TestReadBoundedLinePropagatesReadError(t *testing.T) {
	got := readAll(t, &failingReader{after: []byte("good\n")}, 64)

	if got[0].text != "good" {
		t.Fatalf("line 1 = %q, want %q", got[0].text, "good")
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2 (the line, then the failure)", len(got))
	}
	if !errors.Is(got[1].err, errReadFailed) {
		t.Errorf("error = %v, want it to wrap errReadFailed — a swallowed fault stalls the agent silently", got[1].err)
	}
	if got[1].eof {
		t.Error("a genuine read fault must not be reported as EOF: the drain would call it a clean agent exit")
	}
}

// errReadFailed is the injected pipe fault.
var errReadFailed = errors.New("simulated pipe failure")

// failingReader serves `after` once, then fails every subsequent read with err
// (errReadFailed when unset).
type failingReader struct {
	after []byte
	err   error
	done  bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		if f.err != nil {
			return 0, f.err
		}
		return 0, errReadFailed
	}
	f.done = true
	n := copy(p, f.after)
	return n, nil
}

// The classification one level up, which is what the WARN's value depends on.
// Widening the expected-ends case to cover the reap's pipe close is only safe
// if a GENUINE fault still gets reported: an undrained pipe stalls the agent's
// next write, so silence there is the failure mode with no other symptom.
func TestDrainToLogReportsGenuineReadFault(t *testing.T) {
	logs := newCaptureLog()

	// context.Background() as the test root — the rule's explicit test exemption.
	s := &AgentStream{sessionID: "sess-fault"}
	s.drainToLog(context.Background(), &failingReader{after: []byte("hello\n")},
		"agent stdout", logs.logger())

	var msgs []string
	for draining := true; draining; {
		select {
		case l := <-logs.lines:
			msgs = append(msgs, l.msg)
		default:
			draining = false
		}
	}
	if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, "drain ended early") }) {
		t.Fatalf("genuine read fault was swallowed: records = %v", msgs)
	}
}

// The counterpart: a cancelled ctx is teardown, not a fault, so it stays quiet.
// Pinning both directions is what keeps the WARN meaningful — one that fires on
// ordinary teardown is noise operators learn to skip past.
func TestDrainToLogIsSilentOnTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logs := newCaptureLog()

	s := &AgentStream{sessionID: "sess-torn"}
	s.drainToLog(ctx, &failingReader{after: []byte("hello\n")}, "agent stdout", logs.logger())

	select {
	case l := <-logs.lines:
		t.Fatalf("teardown logged %q; a cancelled drain is an expected end", l.msg)
	default:
	}
}

// The reap closes the pipes, so os.ErrClosed is what EVERY deliberate stop
// delivers to the drains — and it arrives while ctx is still live, because Stop
// Terminates before it cancels. Without the stopping flag the WARN below would
// fire on 100% of ordinary stops, which is the alarm-fatigue failure the
// classification exists to prevent.
func TestDrainToLogIsSilentOnDeliberateStop(t *testing.T) {
	logs := newCaptureLog()

	// context.Background() as the test root — the rule's explicit test exemption.
	s := &AgentStream{sessionID: "sess-stopped"}
	s.stopping.Store(true)
	s.drainToLog(context.Background(), &failingReader{after: []byte("hello\n"), err: os.ErrClosed},
		"agent stdout", logs.logger())

	for {
		select {
		case l := <-logs.lines:
			if strings.Contains(l.msg, "drain ended early") {
				t.Fatalf("deliberate stop logged %q; the reap's pipe close is an expected end", l.msg)
			}
			continue
		default:
		}
		return
	}
}

// The direction that makes the flag load-bearing rather than a blanket mute: an
// os.ErrClosed with no stop in flight means a LIVE agent's pipe closed under the
// drain. Nothing reads it after that, so the agent stalls on its next write with
// no other symptom — this WARN is the only signal that exists.
func TestDrainToLogReportsPipeCloseWithoutAStop(t *testing.T) {
	logs := newCaptureLog()

	// context.Background() as the test root — the rule's explicit test exemption.
	s := &AgentStream{sessionID: "sess-live"}
	s.drainToLog(context.Background(), &failingReader{after: []byte("hello\n"), err: os.ErrClosed},
		"agent stdout", logs.logger())

	var msgs []string
	for draining := true; draining; {
		select {
		case l := <-logs.lines:
			msgs = append(msgs, l.msg)
		default:
			draining = false
		}
	}
	if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, "drain ended early") }) {
		t.Fatalf("a live pipe close was swallowed: records = %v", msgs)
	}
}

// The payload measurement must span the whole line, not each chunk: only the
// FINAL chunk can hold the terminator, so discounting a trailing CR per chunk
// discounts a byte that is ordinary payload. The miss is the dangerous
// direction — bytes dropped, line reported clean — and the input that produces
// it is an over-cap line whose last payload byte is a CR, i.e. a progress bar
// or ANSI redraw, which is what agent output is largely made of.
func TestReadBoundedLineFlagsTruncationWhenPayloadCREndsAChunk(t *testing.T) {
	const (
		limit   = 16
		bufSize = 17 // the CR lands as chunk 1's last byte, its CRLF in chunk 2
	)
	got := readAllSized(t, strings.NewReader(strings.Repeat("a", limit)+"\r\r\n"), limit, bufSize)

	if got[0].text != strings.Repeat("a", limit) {
		t.Fatalf("text = %q, want the %d-byte prefix", got[0].text, limit)
	}
	if !got[0].truncated {
		t.Error("truncated = false, want true — a payload CR was dropped and reported clean")
	}
}

// A CR sitting one byte from the end of an unterminated line is payload, and
// the discount must not reach it. This is the row that separates measuring the
// terminator from stripping any trailing CR: the line ends "\rb", so there is
// no newline and therefore no EOL to discount at all. Counting the CR as a
// terminator here drops the payload to exactly the cap and reports a clipped
// line as clean.
//
// This one documents the invariant rather than pinning the fix: the old
// per-chunk measurement passes it too, because it only discounted a CR with a
// newline behind it in the same chunk. The straddled-CRLF case above is the
// regression pin; treat this as the statement of the rule it belongs to.
func TestReadBoundedLineCountsCRBeforeFinalByte(t *testing.T) {
	const limit = 8
	got := readAll(t, strings.NewReader(strings.Repeat("a", limit-1)+"\rb"), limit)

	if !got[0].truncated {
		t.Error("truncated = false, want true — 9 payload bytes against a cap of 8, " +
			"and an unterminated line has no EOL to discount")
	}
}

// A truncated line whose KEPT prefix ends in a payload CR sitting exactly on
// the cap boundary: the real terminator is beyond the cap and was never
// appended, so the kept prefix is entirely payload and trimEOL must leave it
// alone. Stripping that boundary CR drops an in-cap byte from the log. Unlike
// ...PayloadCREndsAChunk (CR one byte PAST the cap), here the CR is the last
// retained byte, so this is the case that pins trimEOL's truncated no-op.
func TestReadBoundedLineKeepsBoundaryCROnTruncatedLine(t *testing.T) {
	const limit = 8
	got := readAll(t, strings.NewReader(strings.Repeat("a", limit-1)+"\r"+strings.Repeat("b", limit)+"\n"), limit)

	if !got[0].truncated {
		t.Error("truncated = false, want true")
	}
	if want := strings.Repeat("a", limit-1) + "\r"; got[0].text != want {
		t.Errorf("text = %q, want %q — the boundary CR is payload and must survive", got[0].text, want)
	}
}
