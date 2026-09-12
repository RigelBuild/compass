//go:build unix && gtk4 && !linux

package main

// liveChildren has no portable non-linux implementation; the leaked-child reap
// targets WebKitGTK under the linux e2e gate, and darwin does not run it. A
// no-op keeps the non-linux unix build compiling and the reap a clean pass.
func liveChildren() []procInfo { return nil }
