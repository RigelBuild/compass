package runtime

// The Runner's live-container handle cache the session RPCs resolve a launched
// container by name through. Pins the resolve-only-registered contract (a miss
// returns (nil,false), a hit returns the exact handle) across the full
// Register -> Resolve(hit) -> Deregister -> Resolve(miss) round-trip, and that
// concurrent access stays data-race-free (run under -race).

import (
	"strconv"
	"sync"
	"testing"
)

// handleNamed builds a minimal AgentHandle whose container name is name — enough
// for the registry, which keys purely on Name().
func handleNamed(name string) *AgentHandle {
	return &AgentHandle{spec: AgentSpec{Name: name}}
}

func TestRegistryResolvesOnlyRegisteredContainers(t *testing.T) {
	registry := NewAgentRegistry()

	if handle, ok := registry.Resolve("absent"); ok || handle != nil {
		t.Fatalf("Resolve(absent) = (%v, %v), want (nil, false) on an empty registry", handle, ok)
	}
}

func TestRegistryRegisterResolveDeregisterRoundTrip(t *testing.T) {
	registry := NewAgentRegistry()
	handle := handleNamed("atlas-1")

	// Before registration: a miss.
	if _, ok := registry.Resolve("atlas-1"); ok {
		t.Fatal("Resolve(atlas-1) hit before Register")
	}

	// Register -> the exact handle resolves.
	registry.Register(handle)
	got, ok := registry.Resolve("atlas-1")
	if !ok {
		t.Fatal("Resolve(atlas-1) missed after Register")
	}
	if got != handle {
		t.Fatalf("Resolve(atlas-1) = %p, want the registered handle %p", got, handle)
	}

	// A different name never resolves to a registered handle.
	if _, ok := registry.Resolve("other"); ok {
		t.Fatal("Resolve(other) hit; only registered names must resolve")
	}

	// Deregister -> back to a miss.
	registry.Deregister("atlas-1")
	if handle, ok := registry.Resolve("atlas-1"); ok || handle != nil {
		t.Fatalf("Resolve(atlas-1) = (%v, %v) after Deregister, want (nil, false)", handle, ok)
	}

	// Deregistering an absent name is a no-op (no panic, still a miss).
	registry.Deregister("atlas-1")
	if _, ok := registry.Resolve("atlas-1"); ok {
		t.Fatal("Resolve(atlas-1) hit after a redundant Deregister")
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	registry := NewAgentRegistry()
	const workers = 16

	var wg sync.WaitGroup
	// Register, Resolve, and Deregister run concurrently across distinct keys so
	// -race exercises the mutex without the test itself imposing an ordering the
	// registry doesn't promise.
	for i := range workers {
		name := "agent-" + strconv.Itoa(i)
		wg.Go(func() {
			registry.Register(handleNamed(name))
			registry.Resolve(name)
			registry.Deregister(name)
		})
	}
	wg.Wait()

	// Every key registered and then deregistered — the registry ends empty.
	for i := range workers {
		name := "agent-" + strconv.Itoa(i)
		if _, ok := registry.Resolve(name); ok {
			t.Errorf("Resolve(%s) hit after its Deregister; concurrent access lost a delete", name)
		}
	}
}
