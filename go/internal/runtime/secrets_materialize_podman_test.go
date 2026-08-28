//go:build podman

package runtime

// Real-podman coverage for env-delivery secret materialization (RIG-1327 T5).
//
// This test defends the write-then-read contract the broken `podman exec
// --env-file` channel could never satisfy: the materializer writes the
// aggregate env file INSIDE the container over `sh -s`, and the agent sources
// it from its own namespace — it is NOT passed on the agent exec via `podman
// exec --env-file` (podman resolves that path host-side, where the
// container-internal file does not exist, so that exec failed at spawn). This
// asserts the file is readable in-container at AgentEnvFilePath, the place the
// host-side --env-file resolution never reached. Every stub-runtime test
// necessarily misses that mismatch; only a real container exercises the actual
// write-then-read path. Build-tagged (`podman`) so it stays out of the hermetic
// gate, skipped when podman is unusable.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/secrets"
)

// TestMaterializeEnvDeliveryReadableInContainer materializes a DeliveryEnv
// secret into a real container and asserts the agent can read it back from
// $HOME/.compass/env in its own namespace: the file exists at the path
// AgentEnvFilePath names, is mode 0600, and carries the exact KEY=VALUE dotenv
// line the agent's parseEnvFile consumes. This container-internal read path is
// precisely what the broken `podman exec --env-file` channel (host-side path
// resolution) could never reach.
func TestMaterializeEnvDeliveryReadableInContainer(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}

	tmp := t.TempDir()
	buildImage(t, tmp)

	name := "compass-secrets-env-" + strconv.Itoa(os.Getpid())
	facade := NewAgentRuntime(NewPodmanCLI())
	ctx := context.Background()

	_ = exec.Command("podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", name).Run() })

	const home = "/home/agent"
	spec := AgentSpec{
		Name:  name,
		Image: imageTag,
		Workspace: Workspace{
			CheckoutDir: "/work/repo",
			HomeDir:     home,
			UID:         1000,
		},
		Egress: MustAllowEgress(allowedHost),
	}
	handle, err := facade.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch the agent container: %v", err)
	}
	t.Cleanup(func() { _ = facade.Teardown(ctx, handle) })

	// A value chosen to break a naive delivery: it carries an '=', a space, and
	// shell metacharacters. The base64 transport must carry it verbatim, and the
	// KEY=VALUE line grammar must survive the embedded '='.
	const key = "MY_API_TOKEN"
	const value = "a b=c$(touch /tmp/pwned)'\"|&"

	m := NewSecretMaterializer(NewPodmanCLI(), nil)
	if err := m.Install(ctx, handle.ID(), home, spec.Workspace.UID, []secrets.ResolvedSecret{
		{Name: key, Value: value, Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
	}); err != nil {
		t.Fatalf("Install env-delivery secret: %v", err)
	}

	envPath := AgentEnvFilePath(home)

	// The file exists at the canonical path and is mode 0600 (no group/other).
	perm, err := facade.ExecAsAgent(ctx, handle, "stat", "-c", "%a", envPath)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if got := strings.TrimSpace(perm.Stdout); got != "600" {
		t.Fatalf("env file mode = %q, want 600", got)
	}

	// The file is dotenv format (KEY=VALUE, value literal to end-of-line — what
	// the agent's parseEnvFile consumes), NOT a shell script: read it back with a
	// direct-argv `cat` (no shell), so the value's metacharacters are never
	// interpreted on read. The raw line must be exactly KEY=VALUE\n, verbatim.
	cat, err := facade.ExecAsAgent(ctx, handle, "cat", envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	wantLine := key + "=" + value + "\n"
	if cat.Stdout != wantLine {
		t.Fatalf("env file = %q, want %q (verbatim KEY=VALUE line)", cat.Stdout, wantLine)
	}
	// The agent splits each line on the FIRST '=' (values may contain '='); that
	// reproduces the value exactly, embedded '=' and all.
	line := strings.TrimSuffix(cat.Stdout, "\n")
	gotKey, gotVal, ok := strings.Cut(line, "=")
	if !ok || gotKey != key || gotVal != value {
		t.Fatalf("dotenv split = (%q, %q, %v), want (%q, %q, true)", gotKey, gotVal, ok, key, value)
	}
	// The write path fed the value to `sh -s` over stdin (base64-embedded); if
	// that transport ever shell-evaluated the value, the $(touch ...) payload
	// would have created the marker during Install. It must not exist.
	if marker, err := facade.ExecAsAgent(ctx, handle, "sh", "-c", "test -e /tmp/pwned; echo rc=$?"); err != nil {
		t.Fatalf("marker check: %v", err)
	} else if !strings.Contains(marker.Stdout, "rc=1") {
		t.Fatalf("materializing the env file executed the secret value (marker created): %s", marker.Stdout)
	}
}
