//go:build linux

package guestd

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// The virtio-fs workspace tag→path contract, fixed by E3 and moved into guestd
// because the guest has no systemd (guestd IS init), §(d) step 3. The host
// exports the share under the `workspace` tag; guestd mounts it at /workspace,
// the stable-path invariant the agent's cwd depends on.
const (
	workspaceTag    = "workspace"
	workspaceTarget = "/workspace"
)

// apiFilesystem is one kernel pseudo-filesystem guestd mounts on boot. The
// initramfs hands over a bare root, so guestd mounts the ones it and the rest of
// the boot depend on (§(d) step 1).
type apiFilesystem struct {
	source string
	target string
	fstype string
}

// apiFilesystems are the pseudo-filesystems mounted first, in order. /proc is
// read for the kernel cmdline; /sys and /dev (devtmpfs) back netlink and device
// nodes the later steps use.
var apiFilesystems = []apiFilesystem{
	{source: "proc", target: "/proc", fstype: "proc"},
	{source: "sysfs", target: "/sys", fstype: "sysfs"},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
}

// mountAPIFilesystems mounts the API pseudo-filesystems (§(d) step 1). The mount
// point directories are created if missing (the bare root may lack them), then
// mounted. An already-mounted target (EBUSY) is tolerated — a re-exec or an
// initramfs that pre-mounted one must not hard-fail the boot — but any other
// mount failure is fatal.
func mountAPIFilesystems() error {
	for _, fs := range apiFilesystems {
		if err := os.MkdirAll(fs.target, 0o750); err != nil {
			return fmt.Errorf("creating mount point %s: %w", fs.target, err)
		}
		if err := syscall.Mount(fs.source, fs.target, fs.fstype, 0, ""); err != nil {
			if errors.Is(err, syscall.EBUSY) {
				// Already mounted — idempotent-tolerant, continue.
				continue
			}
			return fmt.Errorf("mounting %s at %s (%s): %w", fs.source, fs.target, fs.fstype, err)
		}
	}
	return nil
}

// virtioFSMounter is the real workspaceMounter: it mount(2)s the virtio-fs
// share at its stable path (§(d) step 3).
type virtioFSMounter struct {
	tag    string
	target string
}

// Mount mounts the virtio-fs share (tag → target, type virtiofs). The target
// directory is created if missing. An already-mounted target (EBUSY) is
// tolerated; any other failure is fatal.
func (m *virtioFSMounter) Mount() error {
	if err := os.MkdirAll(m.target, 0o750); err != nil {
		return fmt.Errorf("creating workspace mount point %s: %w", m.target, err)
	}
	if err := syscall.Mount(m.tag, m.target, "virtiofs", 0, ""); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil
		}
		return fmt.Errorf("mounting virtio-fs %q at %s: %w", m.tag, m.target, err)
	}
	return nil
}
