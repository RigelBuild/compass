// The Runner's map of launched agent containers to their AgentHandles, keyed by
// container name. StartAgentSession carries only a container name; the registry
// resolves it to the launched handle the session manager needs. The launch path
// (AgentRuntime.Launch) registers a handle here so the session RPCs can find it
// later; the container engine remains the source of truth for existence, this is
// only the in-memory handle cache.
//
// The registry is a leaf: it stores AgentHandle (an agent-lifecycle type) and
// pulls in no session-manager state, so it lives with AgentRuntime in the
// runtime package rather than the session tier that consumes it.

package runtime

import "sync"

// AgentRegistry is a concurrency-safe cache of launched agent handles keyed by
// container name. The zero value is ready to use.
type AgentRegistry struct {
	mu      sync.Mutex
	handles map[string]*AgentHandle
}

// NewAgentRegistry builds an empty registry.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{handles: map[string]*AgentHandle{}}
}

// Register records a launched agent's handle under its container name.
func (r *AgentRegistry) Register(handle *AgentHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handles == nil {
		r.handles = map[string]*AgentHandle{}
	}
	r.handles[handle.Name()] = handle
}

// Resolve returns the launched handle registered under containerName, or (nil,
// false) if none is registered.
func (r *AgentRegistry) Resolve(containerName string) (*AgentHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	handle, ok := r.handles[containerName]
	return handle, ok
}

// Deregister drops a container's handle (on teardown). Removing an absent name
// is a no-op.
func (r *AgentRegistry) Deregister(containerName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handles, containerName)
}
