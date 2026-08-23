//go:build pgtest && unix

package server

// Whole-flow composition of the Server->Runner fleet-config delivery seam: a
// config bundle PUT into the store of record must stream back out over the
// PRODUCTION RunnerService door the way a live Runner fetches it. This is the
// wiring `buildNetworkServer` owns — it constructs the runnerhub handler and
// decides whether the config surface is served — and no suite on main composes
// it end to end.
//
// The runnerhub package's own FetchAgentConfig wire tests (config_fetch_test.go)
// mount the handler with a FAKE config store via NewMountedHandler directly; they
// prove the handler streams a bundle when handed a store, but never that
// buildNetworkServer hands it the REAL *store.Store. So a server built with the
// config store dropped (configStore == nil at the mount site) fails
// CodeFailedPrecondition — "no config surface" — and every agent provisions with
// no materialized config (no model provider, no skills), while every handler-level
// test stays green. This test closes exactly that gap: it drives the real serving
// path (Serve -> buildDoors -> buildNetworkServer, --listen + TLS), PUTs a bundle
// through the store, and dials FetchAgentConfig over the served RunnerService door
// with a real minted Runner token. If buildNetworkServer ever stops wiring the
// config store into the runner door, the reassembled-bundle assertion reddens.
//
// Store-gated (Serve opens the store; PutAgentConfig writes the bundle row), so it
// lives in the `pgtest` lane behind the shared harness. It reuses
// network_door_test.go's TLS + door helpers (writeSelfSignedCert, freeLoopbackAddr,
// serveInBackground, waitServing) and runner_enroll_compose_pgtest_test.go's
// TLS Runner HTTP client. White-box (package server) so it drives Serve through the
// unexported serving path the sibling network-door pgtest tests use.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// buildValidConfigBundle produces a gzip+tar config bundle that passes the store
// door grammar (validateAndHashConfigBundle): one whitelisted skills/ member with
// a deterministic mtime. FetchAgentConfig chunks the STORED bytes — the gzip
// stream, not the decompressed content — so the member is filled with
// incompressible bytes (a deterministic PRNG, fixed seed) sized so the compressed
// bundle exceeds the door's 512 KiB chunk size several times over. The stream
// then carries multiple chunk frames, exercising drainConfigStream's cross-frame
// reassembly; a compressible filler would collapse to a single chunk and leave
// that loop uncovered. Returned bytes are what a real `compass agent-config push`
// would send.
func buildValidConfigBundle(t *testing.T) []byte {
	t.Helper()
	// ~1.5 MiB of incompressible content, so the gzip-compressed STORED bundle
	// (what the door chunks) stays above 512 KiB by ~3x and streams as several
	// chunk frames. A fixed seed keeps the bundle — and thus its content-hash
	// version — deterministic across runs.
	body := make([]byte, 3*512*1024)
	if _, err := rand.NewChaCha8([32]byte{1}).Read(body); err != nil {
		t.Fatalf("fill random bundle body: %v", err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range []struct {
		name    string
		typ     byte
		content []byte
	}{
		{name: "skills/", typ: tar.TypeDir},
		{name: "skills/review/SKILL.md", typ: tar.TypeReg, content: body},
	} {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     0o644,
			ModTime:  time.Unix(1000, 0),
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("write tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// drainConfigStream reassembles a FetchAgentConfig server stream into (version,
// bundle, chunkFrames): the first frame carries the version, subsequent frames
// carry the tarball in receive order, and chunkFrames counts the chunk frames so
// a caller can assert the bundle actually streamed in more than one. Fails on a
// chunk before the version frame.
func drainConfigStream(t *testing.T, stream *connect.ServerStreamForClient[compassv1internal.FetchAgentConfigResponse]) (string, []byte, int) {
	t.Helper()
	var (
		version     string
		gotVersion  bool
		bundle      []byte
		chunkFrames int
	)
	for stream.Receive() {
		switch frame := stream.Msg().GetFrame().(type) {
		case *compassv1internal.FetchAgentConfigResponse_Version:
			version = frame.Version
			gotVersion = true
		case *compassv1internal.FetchAgentConfigResponse_Chunk:
			if !gotVersion {
				t.Fatal("received a chunk frame before the version frame")
			}
			chunkFrames++
			bundle = append(bundle, frame.Chunk...)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("FetchAgentConfig stream: %v", err)
	}
	return version, bundle, chunkFrames
}

// TestRunnerFetchesConfigThroughNetworkDoor composes the fleet-config delivery seam
// end to end: a bundle PUT through the store of record streams back over the
// production RunnerService door to a real minted Runner token. The positive leg
// reddens if buildNetworkServer drops the config store from the runner door (the
// no-config-surface regression), and the version + byte-identical reassembly prove
// it is the real stored bundle, not a vacuous empty stream.
func TestRunnerFetchesConfigThroughNetworkDoor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	// Mint the Runner token and PUT the bundle against the SAME per-test schema
	// Serve will open, synchronously before Serve starts (no concurrent-Open race;
	// migration is idempotent so Serve re-opening the DSN is a no-op). The admin
	// account is the config-bundle writer PutAgentConfig requires.
	const runnerID = "runner-config-e2e"
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	token, err := runnerhub.MintRunnerToken(ctx, st, runnerID)
	if err != nil {
		t.Fatalf("MintRunnerToken: %v", err)
	}
	wantBundle := buildValidConfigBundle(t)
	wantVersion, err := st.PutAgentConfig(ctx, admin.ID, wantBundle)
	if err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: dsn,
		Version:     "runner-config-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// The network listener binds before the socket (Serve's ordering), so once the
	// socket serves an RPC the TLS door — RunnerService included — is accepting.
	waitServing(t, socketPath)

	httpClient := newTLSRunnerHTTPClient(t, pool)
	client := compassv1internalconnect.NewRunnerServiceClient(
		httpClient, "https://"+addr,
		connect.WithInterceptors(runnerBearer(token)),
	)

	fctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	stream, err := client.FetchAgentConfig(fctx, connect.NewRequest(&compassv1internal.FetchAgentConfigRequest{}))
	if err != nil {
		t.Fatalf("FetchAgentConfig open: %v", err)
	}
	gotVersion, gotBundle, chunkFrames := drainConfigStream(t, stream)
	_ = stream.Close()

	if gotVersion != wantVersion {
		t.Fatalf("fetched config version = %q, want %q (the stored bundle's content hash)", gotVersion, wantVersion)
	}
	if !bytes.Equal(gotBundle, wantBundle) {
		t.Fatalf("reassembled bundle (%d bytes) != stored bundle (%d bytes) — buildNetworkServer must wire the real config store into the runner door", len(gotBundle), len(wantBundle))
	}
	if chunkFrames < 2 {
		t.Fatalf("bundle streamed in %d chunk frame(s); want >= 2 so cross-frame reassembly is exercised (stored bundle %d bytes, 512 KiB chunk size)", chunkFrames, len(wantBundle))
	}
}
