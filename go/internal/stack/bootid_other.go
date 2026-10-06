//go:build unix && !linux && !darwin

package stack

import (
	"fmt"
	"runtime"
)

// readCurrentBootID has no reader here; callers treat the error as an unknown boot.
func readCurrentBootID() (string, error) {
	return "", fmt.Errorf("reading the boot identity is not implemented on %s", runtime.GOOS)
}
