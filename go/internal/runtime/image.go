// Building a repo's per-agent container image from its devenv environment
// (compass.md §5.5) and loading it into local rootless container storage.
//
// The image carries the repo's toolchain — its language runtimes, build tools,
// and dependencies — from devenv, so every agent runs in a reproducible,
// Nix-pinned image with exactly what the project declares. The per-agent image
// is node-local and ephemeral, so it's loaded straight into containers-storage:
// rather than pushed through a registry.
//
// `devenv container build <name>` emits a nix2container image spec; loading it
// into podman's storage is a copyToPodman-style `skopeo copy … containers-
// storage:` step. This module drives that build+load and names the result so the
// runtime can create a container from it.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ImageRef is a reference to an image in local container storage (<name>:<tag>).
type ImageRef string

// String returns the raw image reference.
func (r ImageRef) String() string { return string(r) }

// NoDevenvError is a repo that has no devenv.nix — it must be made
// container-ready before it can build an agent image.
type NoDevenvError struct {
	RepoPath string
}

func (e *NoDevenvError) Error() string {
	return fmt.Sprintf("repo %q has no devenv.nix — it must be made container-ready first", e.RepoPath)
}

// ImageBuilder builds a repo's devenv container into local storage.
//
// devenvContainer names the container attribute declared in the repo's
// devenv.nix (containers.<name>) to build.
type ImageBuilder struct {
	devenvProgram   string
	devenvContainer string
	timeout         time.Duration
}

// NewImageBuilder builds the default `agent` container attribute via `devenv`.
func NewImageBuilder() *ImageBuilder {
	return &ImageBuilder{devenvProgram: "devenv", devenvContainer: "agent", timeout: defaultCommandTimeout}
}

// ImageBuilderForContainer targets a specific container attribute from the
// repo's devenv.nix.
func ImageBuilderForContainer(name string) *ImageBuilder {
	b := NewImageBuilder()
	b.devenvContainer = name
	return b
}

// WithProgram overrides the devenv engine binary (an explicit path, or a stub
// binary in tests). Defaults to `devenv` on PATH.
func (b *ImageBuilder) WithProgram(program string) *ImageBuilder {
	b.devenvProgram = program
	return b
}

// WithTimeout overrides the per-command wall-clock cap.
func (b *ImageBuilder) WithTimeout(timeout time.Duration) *ImageBuilder {
	b.timeout = timeout
	return b
}

// IsContainerReady reports whether a repo is container-ready: has a devenv.nix
// the image builds from. When false, the repo needs a Setup pass to author one
// before it can run agents (compass.md §5.5) — that authoring flow is a separate
// concern from this substrate.
func IsContainerReady(repoPath string) bool {
	info, err := os.Stat(filepath.Join(repoPath, "devenv.nix"))
	return err == nil && info.Mode().IsRegular()
}

// Build builds the repo's devenv container and loads it into local container
// storage, returning the ImageRef a container can be created from.
//
// It errors early with NoDevenvError when the repo isn't container-ready, so the
// caller gets a precise reason rather than an opaque devenv failure.
func (b *ImageBuilder) Build(ctx context.Context, repoPath string) (ImageRef, error) {
	if !IsContainerReady(repoPath) {
		return "", &NoDevenvError{RepoPath: repoPath}
	}

	// `devenv container build <name>` builds the image and prints the store path
	// of the copy-to-podman script (nix2container). Running that script loads the
	// image into containers-storage: — no registry round-trip.
	build, err := b.run(ctx, repoPath, "devenv container build",
		b.devenvProgram, "container", "build", b.devenvContainer)
	if err != nil {
		return "", err
	}
	copyScript := strings.TrimSpace(string(build))

	if _, err := b.run(ctx, repoPath, "devenv container load", copyScript); err != nil {
		return "", err
	}

	// `devenv` loads the image as <container>:latest, a mutable name two repos
	// with the same container attribute share. Resolve it to its immutable image
	// ID immediately, then tag the ID with a repo-path digest — so a concurrent
	// build that moves <container>:latest between the load and the tag can't make
	// us namespace the wrong image.
	built := b.devenvContainer + ":latest"
	inspect, err := b.run(ctx, repoPath, "podman image inspect",
		"podman", "image", "inspect", "--format", "{{.Id}}", built)
	if err != nil {
		return "", err
	}
	imageID := strings.TrimSpace(string(inspect))

	namespaced := fmt.Sprintf("%s-%s:latest", b.devenvContainer, repoDigest(repoPath))
	if _, err := b.run(ctx, repoPath, "podman tag",
		"podman", "tag", imageID, namespaced); err != nil {
		return "", err
	}

	return ImageRef(namespaced), nil
}

// run runs argv[0] with argv[1:] in cwd, requiring a zero exit (a non-zero
// becomes a CommandError; a spawn failure a SpawnError).
func (b *ImageBuilder) run(ctx context.Context, cwd, summary string, argv ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	//nolint:gosec // G204: the image-build seam — spawns the operator-set
	// devenv/podman/skopeo toolchain with a Runner-assembled argv; the repo path
	// is a local filesystem path, not attacker-controlled command input.
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	// A build wedged past the timeout is SIGKILLed by CommandContext; WaitDelay
	// bounds the wait if it leaks a child still holding the output pipe, so a
	// leaked-pipe hang can't outlive the timeout by more than WaitDelay.
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
			// This call's own timeout fired (not the parent): surface a timeout
			// rather than a bogus exit code.
			return nil, &TimeoutError{Summary: summary, Timeout: b.timeout}
		case ctx.Err() != nil:
			// The caller cancelled: propagate the context error.
			return nil, ctx.Err()
		default:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, &CommandError{
					Summary:  summary,
					ExitCode: exitErr.ExitCode(),
					Stderr:   strings.TrimSpace(stderr.String()),
				}
			}
			return nil, &SpawnError{Program: argv[0], Err: err}
		}
	}
	return stdout.Bytes(), nil
}

// repoDigest is a short, stable hex digest of a repo's absolute path — folded
// into the image tag so two repos declaring the same devenv container attribute
// don't share (and clobber) one image in local storage.
func repoDigest(repoPath string) string {
	h := fnv.New64a()
	// Hash.Write never returns an error (documented), so the write is safe to
	// ignore here.
	_, _ = h.Write([]byte(repoPath))
	return fmt.Sprintf("%016x", h.Sum64())
}
