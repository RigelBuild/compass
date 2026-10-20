//go:build (linux && gtk4) || darwin

package main

import (
	"os"
	"strings"
	"testing"
)

// TestAppIDMatchesPackaging guards against a one-sided rename: Linux launchers
// group windows by StartupWMClass, and macOS keys app state on the bundle id.
func TestAppIDMatchesPackaging(t *testing.T) {
	for path, want := range map[string]string{
		"../../../app-bundle/compass.desktop":  "StartupWMClass=" + appID + "\n",
		"../../../tools/macos-bundle/index.ts": `identifier: "` + appID + `",`,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not contain %q", path, want)
		}
	}
}
