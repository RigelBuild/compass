//go:build unix

package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// The admin token's dir may be an operator's existing shared dir, whose mode
// ensurePrivateDir leaves alone; a group/other bit on it must surface as a
// warning naming the path and mode, and a private dir must stay silent.
func TestWarnIfSharedTokenDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		warn bool
	}{
		{"private 0700", 0o700, false},
		{"owner read-only 0500", 0o500, false},
		{"group traverse 0710", 0o710, true},
		{"group rwx 0770", 0o770, true},
		{"other read 0704", 0o704, true},
		{"world 0755", 0o755, true},
		{"sticky world-writable 1777", os.ModeSticky | 0o777, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			warnIfSharedTokenDir(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)), dir)
			out := buf.String()
			if !tc.warn {
				if out != "" {
					t.Fatalf("private dir logged %q", out)
				}
				return
			}
			for _, want := range []string{"level=WARN", "path=" + dir, "mode=" + fmt.Sprintf("%#o", tc.mode.Perm())} {
				if !strings.Contains(out, want) {
					t.Fatalf("warning %q lacks %q", out, want)
				}
			}
		})
	}
}

// A dir that cannot be stat'ed (removed underneath) logs a warning rather
// than failing silently or aborting startup.
func TestWarnIfSharedTokenDirStatError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	var buf bytes.Buffer
	warnIfSharedTokenDir(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)), dir)
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "path="+dir) {
		t.Fatalf("stat failure logged %q", out)
	}
}

func TestWarnIfSharedTokenDirLogCarriesCallerTrace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	ctx, span := tp.Tracer("test").Start(t.Context(), "caller")
	defer span.End()

	var buf bytes.Buffer
	logger := slog.New(otelx.NewTraceHandler(slog.NewTextHandler(&buf, nil)))
	warnIfSharedTokenDir(ctx, logger, dir)

	spanContext := span.SpanContext()
	for _, want := range []string{
		"trace_id=" + spanContext.TraceID().String(),
		"span_id=" + spanContext.SpanID().String(),
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning %q lacks %q", buf.String(), want)
		}
	}
}
