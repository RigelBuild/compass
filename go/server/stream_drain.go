//go:build unix

package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	// Imported for registration: the descriptor walk below reads both packages' files.
	_ "github.com/RigelBuild/compass/go/gen/compass/v1"
	_ "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// serverStreamPaths holds every server-streaming procedure in compass.v1. A
// registry walk, so a new proto file joins without a hand-kept list.
var serverStreamPaths = buildServerStreamPaths()

func buildServerStreamPaths() map[string]struct{} {
	paths := make(map[string]struct{})
	protoregistry.GlobalFiles.RangeFilesByPackage("compass.v1", func(file protoreflect.FileDescriptor) bool {
		services := file.Services()
		for i := range services.Len() {
			service := services.Get(i)
			methods := service.Methods()
			for j := range methods.Len() {
				method := methods.Get(j)
				if method.IsStreamingServer() && !method.IsStreamingClient() {
					paths["/"+string(service.FullName())+"/"+string(method.Name())] = struct{}{}
				}
			}
		}
		return true
	})
	return paths
}

// streamDrainGrace lets a stream that ends on bus close write its trailers
// cleanly; only a write still stalled after it is cut.
const streamDrainGrace = time.Second

// streamEntry is one in-flight server stream: its writer and its handler ctx cancel.
type streamEntry struct {
	controller *http.ResponseController
	cancel     context.CancelFunc
	path       string
}

type streamDrain struct {
	mu       sync.Mutex
	streams  map[*streamEntry]struct{}
	draining bool
}

func newStreamDrain() *streamDrain {
	return &streamDrain{streams: make(map[*streamEntry]struct{})}
}

func (d *streamDrain) wrap(next http.Handler) http.Handler {
	if d == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := serverStreamPaths[r.URL.Path]; !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		entry := &streamEntry{controller: http.NewResponseController(w), cancel: cancel, path: r.URL.Path}
		d.mu.Lock()
		if d.draining {
			entry.stop(ctx)
		}
		d.streams[entry] = struct{}{}
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			delete(d.streams, entry)
			d.mu.Unlock()
		}()

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// abort ends every in-flight server stream because Shutdown neither cancels
// handler contexts nor unblocks a stalled response write.
func (d *streamDrain) abort(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining = true
	for entry := range d.streams {
		entry.stop(ctx)
	}
}

// stop cancels the handler ctx, which wakes an idle stream, and bounds a write
// already blocked on a client that stopped reading.
func (e *streamEntry) stop(ctx context.Context) {
	e.cancel()
	if err := e.controller.SetWriteDeadline(time.Now().Add(streamDrainGrace)); err != nil {
		slog.WarnContext(ctx, "failed to set server-stream write deadline", "path", e.path, "err", err)
	}
}
