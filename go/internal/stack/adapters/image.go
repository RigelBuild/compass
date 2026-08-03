//go:build unix

package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sealedsecurity/compass/go/internal/runtime"
	"github.com/sealedsecurity/compass/go/internal/stack"
)

// imagePullTimeout is the per-command timeout the ImageEnsurer gives podman for
// a pull. A cold pull of the agent image (a multi-hundred-MB layer set over a
// slow link) can far exceed PodmanCLI's default per-command timeout, so the
// generous window is set here at the callsite rather than baked into Pull —
// Pull stays a plain passthrough usable under the default timeout for cheap
// re-pulls, and only the ensure path pays for the cold-pull headroom.
const imagePullTimeout = 10 * time.Minute

// imagePuller is the narrow surface the ensure logic needs from the container
// CLI: pull an image by ref. *runtime.PodmanCLI satisfies it; a fake satisfies
// it in tests, so the ensure logic is unit-testable without a real podman.
type imagePuller interface {
	Pull(ctx context.Context, image string) error
}

// ImageEnsurer is the real stack.ImageEnsurer: it ensures the agent image ref
// is present in the local container store by pulling it, so the runner (which
// refuses to boot without the image, DL-112) can start.
type ImageEnsurer struct {
	puller imagePuller
}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.ImageEnsurer = (*ImageEnsurer)(nil)

// NewImageEnsurer builds an ImageEnsurer over a real *runtime.PodmanCLI carrying
// a generous per-command timeout for cold pulls (imagePullTimeout).
func NewImageEnsurer() *ImageEnsurer {
	return newImageEnsurer(runtime.NewPodmanCLI().WithTimeout(imagePullTimeout))
}

// newImageEnsurer is the injection seam: it builds an ImageEnsurer over any
// imagePuller, so both NewImageEnsurer and tests share one construction path.
func newImageEnsurer(p imagePuller) *ImageEnsurer {
	return &ImageEnsurer{puller: p}
}

// EnsureImage pulls image into the local store. An empty ref is rejected early —
// the runner can't boot without an image, and pulling "" would surface as an
// opaque podman error. Otherwise it delegates to Pull: `podman pull` is
// idempotent (a present image re-pulls cheaply against the registry digest), so
// no pre-existence check is done by deliberate choice — the pull IS the ensure.
func (e *ImageEnsurer) EnsureImage(ctx context.Context, image string) error {
	if image == "" {
		return errors.New("ensure agent image: empty image ref (the runner cannot boot without an image)")
	}
	if err := e.puller.Pull(ctx, image); err != nil {
		return fmt.Errorf("pulling agent image %q: %w", image, err)
	}
	return nil
}
