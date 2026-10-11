//go:build !unix

package runtime

import "os/exec"

// killedBySIGKILL falls back to "signalled" where there is no WaitStatus.
func killedBySIGKILL(e *exec.ExitError) bool { return e.ExitCode() == -1 }
