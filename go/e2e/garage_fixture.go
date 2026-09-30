//go:build podman

package e2e

import (
	"fmt"
	"os"
	"os/exec" //nolint:depguard // e2e harness: podman run/exec for the Garage archive container
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const garageImage = "docker.io/dxflrs/garage:v2.1.0@sha256:850490b7aef237f30859c7deeae8a7c99121cccbdf13dc57d6d10ae6e3e3694d"

type garageFixture struct{ endpoint, bucket, accessKey, secretKey string }

func startGarageFixture(t *testing.T) *garageFixture {
	t.Helper()
	port := freePorts(t, 1)[0]
	root := t.TempDir()
	// The image has no writable /var/lib, so metadata and data live under /tmp.
	config := fmt.Sprintf(`metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = %q
[s3_api]
s3_region = "garage"
api_bind_addr = "0.0.0.0:3900"
[admin]
api_bind_addr = "0.0.0.0:3903"
admin_token = %q
`, strings.Repeat("a", 64), strings.Repeat("b", 64))
	configPath := filepath.Join(root, "garage.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write Garage config: %v", err)
	}
	name := fmt.Sprintf("compass-e2e-garage-%d-%d", os.Getpid(), port)
	cmd := exec.Command("podman", "run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:3900", port), "-v", configPath+":/etc/garage.toml:Z", garageImage) //nolint:gosec // fixed podman argv; name/port/path are fixture-minted
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start Garage: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("podman", "rm", "-f", name).CombinedOutput(); err != nil { //nolint:gosec // exact-name rm of the fixture container
			t.Errorf("remove Garage container: %v: %s", err, out)
		}
	})
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("podman", append([]string{"exec", name, "/garage"}, args...)...).CombinedOutput() //nolint:gosec // fixed garage CLI verbs from this file
		if err != nil {
			t.Fatalf("garage %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	nodeID := awaitGarageNodeID(t, name)
	run("layout", "assign", "-z", "dc1", "-c", "1G", nodeID)
	run("layout", "apply", "--version", "1")
	id, secret := parseGarageKey(t, run("key", "create", "compass-e2e"))
	bucket := "compass-e2e"
	run("bucket", "create", bucket)
	run("bucket", "allow", "--read", "--write", bucket, "--key", id)
	return &garageFixture{endpoint: fmt.Sprintf("127.0.0.1:%d", port), bucket: bucket, accessKey: id, secretKey: secret}
}

// awaitGarageNodeID polls until the server has written its node key; garage
// exposes no readiness signal, so a CLI probe is the only observable.
func awaitGarageNodeID(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := exec.Command("podman", "exec", name, "/garage", "node", "id", "-q").Output() //nolint:gosec // fixture-minted container name
		if err == nil {
			id, _, _ := strings.Cut(strings.TrimSpace(string(out)), "@")
			if id != "" {
				return id
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("garage node id never became readable: %v: %s", err, out)
		}
		<-ticker.C
	}
}

func parseGarageKey(t *testing.T, output string) (string, string) {
	t.Helper()
	var id, secret string
	for line := range strings.SplitSeq(output, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		switch strings.TrimSpace(parts[0]) {
		case "Key ID":
			id = strings.TrimSpace(parts[1])
		case "Secret key":
			secret = strings.TrimSpace(parts[1])
		}
	}
	if id == "" || secret == "" {
		t.Fatalf("parse Garage key output: %s", output)
	}
	return id, secret
}

func garageS3Client(t *testing.T, g *garageFixture) *minio.Client {
	t.Helper()
	client, err := minio.New(g.endpoint, &minio.Options{Creds: credentials.NewStaticV4(g.accessKey, g.secretKey, ""), Secure: false, Region: "garage", BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatalf("create Garage S3 client: %v", err)
	}
	return client
}
