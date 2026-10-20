//go:build (linux && gtk4) || darwin

package main

import (
	"slices"
	"testing"
	"testing/synctest"
)

// TestSerialOpenerNamesConcurrentOpensUniquely parks the first create until the
// second open is blocked; without serialization both would pick "bridge".
func TestSerialOpenerNamesConcurrentOpensUniquely(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		live := map[string]bool{}
		var names []string
		release := make(chan struct{})
		open := serialOpener(
			func(n string) bool { return live[n] },
			func(name string) {
				names = append(names, name)
				if len(names) == 1 {
					<-release
				}
				live[name] = true
			},
		)

		go open()
		synctest.Wait() // first create is parked on release
		go open()
		synctest.Wait() // second open is blocked, or has already named a window
		close(release)
		synctest.Wait()

		if want := []string{"bridge", "bridge-2"}; !slices.Equal(names, want) {
			t.Fatalf("opened %v, want %v", names, want)
		}
	})
}
