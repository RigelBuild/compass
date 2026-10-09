//go:build podman

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/minio/minio-go/v7"
)

const (
	sessionImageMarker       = "e2e-session-image-resume"
	sessionImageReply        = "session image inspected"
	sessionImageUnavailable  = "[image unavailable after resume]"
	sessionImageResumeHandle = "session-image-resume"
)

var sessionImageRefPattern = regexp.MustCompile(`blob:sha256:([a-f0-9]{64})`)

func TestLegSessionImageResume(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		objectStore bool
	}{
		{name: "with_object_store", objectStore: true},
		{name: "without_object_store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runSessionImageResume(t, ctx, tc.objectStore)
		})
	}
}

func runSessionImageResume(t *testing.T, ctx context.Context, withObjectStore bool) {
	t.Helper()
	f := NewFixture(ctx, t, sessionImageFixtureOptions(t, withObjectStore)...)
	accountID, err := f.CreateAgent(ctx, sessionImageResumeHandle, "Session image resume")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	container1, err := f.Provision(ctx, accountID, "session-image-provision-1")
	if err != nil {
		t.Fatalf("Provision (first container): %v", err)
	}
	firstRemoved := false
	t.Cleanup(func() {
		if firstRemoved {
			return
		}
		if err := f.RemoveWorkspace(ctx, container1, "session-image-cleanup-1"); err != nil {
			t.Errorf("RemoveWorkspace cleanup (first container): %v", err)
		}
	})
	sessionID, err := f.StartSession(ctx, container1)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	acc, err := adminAgentByHandle(ctx, st, sessionImageResumeHandle)
	if err != nil {
		t.Fatalf("AgentByHandle: %v", err)
	}
	channel := string(acc.Agent.HomeChannelID)
	tail, err := f.OpenSessionTail(ctx, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionTail (first container): %v", err)
	}
	defer tail.Close()
	postID, err := f.PostMessage(ctx, channel, "general", sessionImageMarker+": inspect the image and stop")
	if err != nil {
		t.Fatalf("PostMessage (image trigger): %v", err)
	}
	if err := f.AwaitTurnSettled(ctx, tail); err != nil {
		t.Fatalf("AwaitTurnSettled (image turn): %v", err)
	}
	if err := f.waitDeliveryCursorPast(ctx, st, acc.ID, acc.Agent.HomeChannelID, store.MessageID(postID)); err != nil {
		t.Fatalf("waitDeliveryCursorPast (image trigger): %v", err)
	}
	transcript, err := f.awaitTranscriptPersisted(ctx, st, sessionID, sessionImageReply)
	if err != nil {
		t.Fatalf("awaitTranscriptPersisted (image reply): %v", err)
	}
	sha256Hex := sessionImageRef(t, transcript)
	blobPath := filepath.Join("/home/agent/.omp/agent/blobs", sha256Hex)
	firstBlob, err := readSessionImageContainerFile(ctx, container1, blobPath)
	if err != nil {
		t.Fatalf("read first-lifetime blob %s: %v", sha256Hex, err)
	}
	if len(firstBlob) < 1024 {
		t.Fatalf("first-lifetime blob is %d bytes, want at least 1024", len(firstBlob))
	}
	digest := sha256.Sum256(firstBlob)
	if hex.EncodeToString(digest[:]) != sha256Hex {
		t.Fatalf("first-lifetime blob digest = %x, want %s", digest, sha256Hex)
	}
	if withObjectStore {
		storedBlob, err := awaitSessionImageObject(ctx, garageS3Client(t, f.garage), f.garage.bucket, sha256Hex)
		if err != nil {
			t.Fatalf("await object-store blob %s: %v", sha256Hex, err)
		}
		if !bytes.Equal(storedBlob, firstBlob) {
			t.Fatalf("object-store blob %s differs from the first container bytes", sha256Hex)
		}
	}
	if _, err := f.Compass().StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{SessionId: sessionID})); err != nil {
		t.Fatalf("StopAgentSession: %v", err)
	}
	// Remove the bind-mounted blob so the fresh container must restore it.
	if err := removeSessionImageContainerFile(ctx, container1, blobPath); err != nil {
		t.Fatalf("remove first-lifetime blob %s: %v", sha256Hex, err)
	}
	if err := f.RemoveWorkspace(ctx, container1, "session-image-teardown-1"); err != nil {
		t.Fatalf("RemoveWorkspace (first container): %v", err)
	}
	firstRemoved = true

	container2, err := f.Provision(ctx, accountID, "session-image-provision-2")
	if err != nil {
		t.Fatalf("Provision (fresh container): %v", err)
	}
	t.Cleanup(func() {
		if err := f.RemoveWorkspace(ctx, container2, "session-image-cleanup-2"); err != nil {
			t.Errorf("RemoveWorkspace cleanup (fresh container): %v", err)
		}
	})
	resumed, err := f.Resume(ctx, container2, sessionID)
	if err != nil {
		t.Fatalf("Resume (fresh container): %v", err)
	}
	if resumed != sessionID {
		t.Fatalf("resumed session = %q, want original %q", resumed, sessionID)
	}
	resumePath := filepath.Join("/home/agent/.compass/resume", sessionID+".jsonl")
	resumedBody, err := readSessionImageContainerFile(ctx, container2, resumePath)
	if err != nil {
		t.Fatalf("read resumed transcript: %v", err)
	}
	resumedText := string(resumedBody)
	assertResumedSessionImage(t, ctx, container2, resumedText, blobPath, sha256Hex, firstBlob, withObjectStore)
}

func sessionImageSVG() string {
	const (
		edge     = 40
		cellSize = 5
	)
	rng := rand.New(rand.NewSource(1582))
	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" viewBox="0 0 200 200">`)
	for y := range edge {
		for x := range edge {
			color := rng.Intn(1 << 24)
			fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="%d" height="%d" fill="#%06x"/>`, x*cellSize, y*cellSize, cellSize, cellSize, color)
		}
	}
	svg.WriteString("</svg>")
	return svg.String()
}

func sessionImageFixtureOptions(t *testing.T, withObjectStore bool) []fixtureOption {
	t.Helper()
	writeArgs, err := json.Marshal(map[string]string{
		"path":    "resume-image.svg",
		"content": sessionImageSVG(),
	})
	if err != nil {
		t.Fatalf("marshal image write args: %v", err)
	}
	readArgs, err := json.Marshal(map[string]string{"path": "resume-image.svg:img"})
	if err != nil {
		t.Fatalf("marshal image read args: %v", err)
	}
	options := []fixtureOption{
		WithForgeStub(),
		WithCannedImageInput(),
		WithCannedScript(CannedText("session image fallback")),
		WithCannedMarkerScript(
			sessionImageMarker,
			CannedToolCall("write", string(writeArgs)),
			CannedToolCall("read", string(readArgs)),
			CannedText(sessionImageReply),
		),
	}
	if withObjectStore {
		options = append(options, WithObjectStore(startGarageFixture(t)))
	}
	return options
}

// sessionImageRef returns the hash the SDK externalized when the read tool returned the image.
func sessionImageRef(t *testing.T, transcript []store.TranscriptEntryRow) string {
	t.Helper()
	var transcriptBody strings.Builder
	for _, entry := range transcript {
		transcriptBody.WriteString(entry.EntryJSON)
	}
	matches := sessionImageRefPattern.FindStringSubmatch(transcriptBody.String())
	if len(matches) != 2 {
		t.Fatalf("first-lifetime transcript has no externalized image reference: %q", transcriptBody.String())
	}
	return matches[1]
}

func assertResumedSessionImage(t *testing.T, ctx context.Context, container2, resumedText, blobPath, sha256Hex string, firstBlob []byte, withObjectStore bool) {
	t.Helper()
	if withObjectStore {
		if !strings.Contains(resumedText, "blob:sha256:"+sha256Hex) {
			t.Fatalf("resumed transcript lost image reference %s", sha256Hex)
		}
		resumedBlob, err := readSessionImageContainerFile(ctx, container2, blobPath)
		if err != nil {
			t.Fatalf("read resumed blob %s: %v", sha256Hex, err)
		}
		if !bytes.Equal(resumedBlob, firstBlob) {
			t.Fatalf("resumed blob %s differs from the first-lifetime bytes", sha256Hex)
		}
		return
	}
	if !strings.Contains(resumedText, sessionImageUnavailable) {
		t.Fatalf("resumed transcript lacks unavailable-image marker %q", sessionImageUnavailable)
	}
	if strings.Contains(resumedText, "blob:sha256:"+sha256Hex) {
		t.Fatalf("resumed transcript still carries absent image reference %s", sha256Hex)
	}
}

func readSessionImageContainerFile(ctx context.Context, container, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "podman", "exec", container, "cat", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman exec cat %q: %w: %q", path, err, out)
	}
	return out, nil
}

func removeSessionImageContainerFile(ctx context.Context, container, path string) error {
	cmd := exec.CommandContext(ctx, "podman", "exec", container, "rm", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman exec rm %q: %w: %q", path, err, out)
	}
	return nil
}

func awaitSessionImageObject(ctx context.Context, client *minio.Client, bucket, sha256Hex string) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, sessionImageUploadWait)
	defer cancel()
	ticker := time.NewTicker(sessionImageUploadPoll)
	defer ticker.Stop()
	key := "blobs/" + sha256Hex
	for {
		obj, err := client.GetObject(waitCtx, bucket, key, minio.GetObjectOptions{})
		if err == nil {
			data, readErr := io.ReadAll(obj)
			closeErr := obj.Close()
			if readErr == nil && closeErr == nil {
				return data, nil
			}
			if minio.ToErrorResponse(readErr).Code != "NoSuchKey" {
				return nil, fmt.Errorf("read %s: %w", key, errors.Join(readErr, closeErr))
			}
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("object %s was not uploaded: %w", key, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

const (
	sessionImageUploadPoll = 100 * time.Millisecond
	sessionImageUploadWait = 15 * time.Second
)
