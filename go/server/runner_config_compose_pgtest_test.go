//go:build pgtest && unix

package server

// Whole-flow composition of the Server->Runner fleet-config delivery seam: a
// bundle PUT into the store must stream back over the PRODUCTION RunnerService
// door. No suite on main composes this, so it drives the real serving path and
// reddens if buildNetworkServer ever drops the config store from the runner door.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Builds an incompressible config bundle that spans several 512 KiB chunks.
func buildValidConfigBundle(t *testing.T) []byte {
	t.Helper()
	// Fixed-seed data keeps the chunked bundle and content hash deterministic.
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

// Reassembles FetchAgentConfig frames into a version and bundle, counts chunks,
// and rejects a chunk received before the version.
func drainConfigStream(t *testing.T, stream compassv1internalconnect.RunnerServiceFetchAgentConfigClientStream) (string, []byte, int) {
	t.Helper()
	var (
		version     string
		gotVersion  bool
		bundle      []byte
		chunkFrames int
	)
	for {
		frame, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("FetchAgentConfig stream: %v", err)
		}
		switch payload := frame.GetFrame().(type) {
		case *compassv1internal.FetchAgentConfigResponse_Version:
			version = payload.Version
			gotVersion = true
		case *compassv1internal.FetchAgentConfigResponse_Chunk:
			if !gotVersion {
				t.Fatal("received a chunk frame before the version frame")
			}
			chunkFrames++
			bundle = append(bundle, payload.Chunk...)
		}
	}
	return version, bundle, chunkFrames
}

// A bundle stored through the real store must stream back through the production
// Runner door to a minted token, with its version and bytes intact.
func TestRunnerFetchesConfigThroughNetworkDoor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)

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

	addr := serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: dsn,
		Version:     "runner-config-test",
		Listen:      loopbackAny,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	}).network
	// The network listener binds before the socket (Serve's ordering), so once the
	// socket serves an RPC the TLS door — RunnerService included — is accepting.
	waitServing(t, socketPath)

	httpClient := newTLSRunnerHTTPClient(t, pool)
	client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(
		httpClient, "https://"+addr,
		connecthttp.WithReadMaxBytes(0),
	), runnerBearer(token).ClientInterceptor,
	))

	fctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	stream, err := client.FetchAgentConfig(fctx, &compassv1internal.FetchAgentConfigRequest{})
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
